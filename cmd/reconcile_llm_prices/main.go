package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const farContext = 1 << 40

var datasetID = regexp.MustCompile(`^[a-z0-9_-]+/[a-z0-9_]+$`)

var groupDimensions = []string{"provider", "requested_model", "executed_model", "service_tier", "price_provider", "price_model", "price_service_tier", "price_min_context"}

type attemptGroup struct {
	provider, requested, executed, tier *string
	current                             *usage.PriceKey
	count                               int64
}

type update struct {
	where    map[string]any
	set      map[string]any
	attempts int64
	key      usage.PriceKey
	found    bool
}

type contextGroup struct {
	group    attemptGroup
	tokens   int64
	band     int64
	attempts int64
}

type queryResult struct {
	Rows [][]any `json:"rows"`
}

type client struct {
	http          *http.Client
	url           string
	token         string
	attempts      string
	prices        string
	attemptsTable string
	pricesTable   string
}

func text(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

func optionalString(value any) *string {
	s, ok := value.(string)
	if !ok {
		return nil
	}

	return &s
}

func optionalInt(value any) (int64, bool) {
	number, ok := value.(float64)

	return int64(number), ok
}

func (g attemptGroup) event(contextTokens int64) usage.AccountingEvent {
	return usage.AccountingEvent{
		Provider: text(g.provider), RequestedAlias: text(g.requested), ExecutedModel: text(g.executed), ReportedServiceTier: text(g.tier),
		Tokens: &usage.AccountingTokens{Breakdown: usage.NewSubsetTokenBreakdown(contextTokens, 0, 0, 0, 0, contextTokens)},
	}
}

func (g attemptGroup) where() map[string]any {
	where := map[string]any{"provider": g.provider, "requested_model": g.requested, "executed_model": g.executed, "service_tier": g.tier, "price_provider": nil, "price_model": nil, "price_service_tier": nil, "price_min_context_tokens": nil}
	if g.current != nil {
		where["price_provider"], where["price_model"], where["price_service_tier"], where["price_min_context_tokens"] = g.current.Provider, g.current.Model, g.current.Tier, g.current.MinContext
	}

	return where
}

func keyColumns(key usage.PriceKey) map[string]any {
	return map[string]any{"price_provider": key.Provider, "price_model": key.Model, "price_service_tier": key.Tier, "price_min_context_tokens": key.MinContext}
}

func isBanded(book *usage.PriceBook, g attemptGroup) bool {
	low, _ := book.Key(g.event(0))
	high, _ := book.Key(g.event(farContext))

	return low.MinContext != high.MinContext
}

func planGroups(book *usage.PriceBook, groups []attemptGroup) []update {
	var updates []update
	for _, g := range groups {
		if g.current != nil || g.provider == nil {
			continue
		}

		key, found := book.Key(g.event(0))
		updates = append(updates, update{where: g.where(), set: keyColumns(key), attempts: g.count, key: key, found: found})
	}

	return updates
}

func planBands(book *usage.PriceBook, groups []contextGroup) []update {
	var updates []update
	for _, c := range groups {
		low, _ := book.Key(c.group.event(0))
		high, _ := book.Key(c.group.event(c.tokens))
		if high.MinContext == 0 {
			continue
		}

		where := c.group.where()
		where["price_provider"], where["price_model"], where["price_service_tier"], where["price_min_context_tokens"] = low.Provider, low.Model, low.Tier, int64(0)
		where["input_tokens"] = c.tokens
		updates = append(updates, update{where: where, set: map[string]any{"price_min_context_tokens": high.MinContext}, attempts: c.attempts, key: high, found: true})
	}

	return updates
}

func (c client) call(ctx context.Context, method, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.url, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(answer)))
	}

	return json.Unmarshal(answer, out)
}

func (c client) query(ctx context.Context, dimensions []string, where []map[string]any) ([][]any, error) {
	request := map[string]any{
		"query": map[string]any{
			"model": "llm_usage", "measures": []string{"attempts.attempts"}, "dimensions": dimensions, "where": where,
		},
		"datasets": map[string]string{c.attemptsTable: c.attempts, c.pricesTable: c.prices},
	}

	var result queryResult
	if err := c.call(ctx, http.MethodPost, "/api/query", request, &result); err != nil {
		return nil, err
	}

	return result.Rows, nil
}

func parseGroup(row []any) (attemptGroup, error) {
	if len(row) != len(groupDimensions)+1 {
		return attemptGroup{}, errors.New("unexpected query row")
	}

	count, ok := optionalInt(row[len(groupDimensions)])
	if !ok {
		return attemptGroup{}, errors.New("unexpected attempt count")
	}

	g := attemptGroup{provider: optionalString(row[0]), requested: optionalString(row[1]), executed: optionalString(row[2]), tier: optionalString(row[3]), count: count}
	provider, model, tier := optionalString(row[4]), optionalString(row[5]), optionalString(row[6])
	band, hasBand := optionalInt(row[7])
	if provider != nil && model != nil && tier != nil && hasBand {
		g.current = &usage.PriceKey{Provider: *provider, Model: *model, Tier: *tier, MinContext: band}
	}

	return g, nil
}

func (c client) groups(ctx context.Context) ([]attemptGroup, error) {
	rows, err := c.query(ctx, groupDimensions, []map[string]any{})
	if err != nil {
		return nil, err
	}

	groups := make([]attemptGroup, 0, len(rows))
	for _, row := range rows {
		g, err := parseGroup(row)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}

	return groups, nil
}

func (c client) contexts(ctx context.Context, book *usage.PriceBook, groups []attemptGroup) ([]contextGroup, error) {
	var contexts []contextGroup
	seen := map[string]bool{}
	for _, g := range groups {
		if !isBanded(book, g) || seen[text(g.executed)] {
			continue
		}
		seen[text(g.executed)] = true

		dimensions := append(append([]string{}, groupDimensions...), "input_tokens")
		rows, err := c.query(ctx, dimensions, []map[string]any{{"dimension": "executed_model", "op": "eq", "value": text(g.executed)}})
		if err != nil {
			return nil, err
		}

		for _, row := range rows {
			tokens, ok := optionalInt(row[len(groupDimensions)])
			if !ok {
				continue
			}

			rest := append(append([]any{}, row[:len(groupDimensions)]...), row[len(groupDimensions)+1])
			group, err := parseGroup(rest)
			if err != nil {
				return nil, err
			}
			low, _ := book.Key(group.event(0))
			if group.current == nil || *group.current == low {
				contexts = append(contexts, contextGroup{group: group, tokens: tokens, attempts: group.count})
			}
		}
	}

	return contexts, nil
}

func (c client) apply(ctx context.Context, updates []update) error {
	for _, u := range updates {
		var result struct {
			Rows int `json:"rows"`
		}
		body := map[string]any{"where": u.where, "set": u.set}
		if err := c.call(ctx, http.MethodPatch, "/api/datasets/"+c.attempts+"/rows", body, &result); err != nil {
			return err
		}
		if int64(result.Rows) != u.attempts {
			return fmt.Errorf("patched %d rows where %d attempts were planned for %v", result.Rows, u.attempts, u.where)
		}
	}

	return nil
}

func report(out io.Writer, title string, updates []update) {
	sort.Slice(updates, func(i, j int) bool { return updates[i].attempts > updates[j].attempts })

	var attempts int64
	missing := map[string]int64{}
	for _, u := range updates {
		attempts += u.attempts
		if !u.found {
			missing[u.key.Provider+"/"+u.key.Model] += u.attempts
		}
	}

	_, _ = fmt.Fprintf(out, "%s: %d updates covering %d attempts\n", title, len(updates), attempts)
	for _, u := range updates[:min(len(updates), 25)] {
		_, _ = fmt.Fprintf(out, "  %6d  %s/%s tier=%q band=%d priced=%t\n", u.attempts, u.key.Provider, u.key.Model, u.key.Tier, u.key.MinContext, u.found)
	}
	for model, count := range missing {
		_, _ = fmt.Fprintf(out, "  no price in the catalog yet: %s (%d attempts)\n", model, count)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("reconcile_llm_prices", flag.ContinueOnError)
	catalogPath := flags.String("catalog", "", "LiteLLM price catalog file, the one ai-proxy publishes prices from")
	apply := flags.Bool("apply", false, "Patch the attempts, default is a dry run")
	maxUpdates := flags.Int("max-updates", 500, "Refuse to apply more updates than this, each one is a synchronous mutation")
	url := flags.String("url", "https://analytics.blamedevs.com", "Analytics base URL")
	attempts := flags.String("dataset", "vaynerakawalo/llm_usage", "Attempts dataset owner/name")
	prices := flags.String("prices-dataset", "vaynerakawalo/llm_model_prices", "Model prices dataset owner/name")
	tokenEnv := flags.String("token-env", "ANALYTICS_TOKEN", "Environment variable with an Analytics API token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *catalogPath == "" || !datasetID.MatchString(*attempts) || !datasetID.MatchString(*prices) || os.Getenv(*tokenEnv) == "" {
		return errors.New("catalog, owner/name datasets and a token in the token environment variable are required")
	}

	raw, err := os.ReadFile(*catalogPath)
	if err != nil {
		return err
	}
	book, err := usage.NewLiteLLMPriceBook(raw)
	if err != nil {
		return err
	}

	c := client{http: &http.Client{Timeout: 5 * time.Minute}, url: *url, token: os.Getenv(*tokenEnv), attempts: *attempts, prices: *prices, attemptsTable: "llm_usage_attempts", pricesTable: "llm_prices"}
	groups, err := c.groups(ctx)
	if err != nil {
		return err
	}
	contexts, err := c.contexts(ctx, book, groups)
	if err != nil {
		return err
	}

	keys, bands := planGroups(book, groups), planBands(book, contexts)
	report(out, "price keys", keys)
	report(out, "context bands", bands)
	if !*apply {
		return nil
	}
	if len(keys)+len(bands) > *maxUpdates {
		return fmt.Errorf("%d updates exceed max-updates %d", len(keys)+len(bands), *maxUpdates)
	}
	if err := c.apply(ctx, keys); err != nil {
		return err
	}

	return c.apply(ctx, bands)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
