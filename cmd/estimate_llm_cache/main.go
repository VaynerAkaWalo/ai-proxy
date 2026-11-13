package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var datasetID = regexp.MustCompile(`^[a-z0-9_-]+/[a-z0-9_]+$`)

type modelKey struct {
	provider string
	model    string
}

type classified struct {
	input, output, cacheRead, cacheWrite, reasoning float64
}

type legacy struct {
	attempts            int64
	input, output, cost float64
}

type rates struct {
	input, cacheRead, cacheWrite, output, reasoning float64
}

type estimate struct {
	key        modelKey
	attempts   int64
	readShare  float64
	writeShare float64
	reasoning  float64
	classified float64
	skipped    string
}

type queryResult struct {
	Rows [][]any `json:"rows"`
}

type client struct {
	http     *http.Client
	url      string
	token    string
	attempts string
	prices   string
}

func number(value any) float64 {
	n, _ := value.(float64)

	return n
}

func decimal(value any) float64 {
	s, ok := value.(string)
	if !ok {
		return number(value)
	}
	n, _ := strconv.ParseFloat(s, 64)

	return n
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

func (c client) query(ctx context.Context, measures []string, where []map[string]any) ([][]any, error) {
	request := map[string]any{
		"query": map[string]any{
			"model": "llm_usage", "measures": measures, "dimensions": []string{"provider", "executed_model"}, "where": where,
		},
		"datasets": map[string]string{"llm_usage_attempts": c.attempts, "llm_prices": c.prices},
	}

	var result queryResult
	if err := c.call(ctx, http.MethodPost, "/api/query", request, &result); err != nil {
		return nil, err
	}

	return result.Rows, nil
}

func eq(dimension, value string) map[string]any {
	return map[string]any{"dimension": dimension, "op": "eq", "value": value}
}

func (c client) classified(ctx context.Context) (map[modelKey]classified, error) {
	measures := []string{"attempts.input_tokens", "attempts.output_tokens", "attempts.cache_read_tokens", "attempts.cache_write_tokens", "attempts.reasoning_tokens"}
	rows, err := c.query(ctx, measures, []map[string]any{eq("usage_quality", "complete")})
	if err != nil {
		return nil, err
	}

	byModel := map[modelKey]classified{}
	for _, row := range rows {
		provider, _ := row[0].(string)
		model, _ := row[1].(string)
		byModel[modelKey{provider, model}] = classified{input: number(row[2]), output: number(row[3]), cacheRead: number(row[4]), cacheWrite: number(row[5]), reasoning: number(row[6])}
	}

	return byModel, nil
}

func (c client) legacy(ctx context.Context) (map[modelKey]legacy, error) {
	measures := []string{"attempts.attempts", "attempts.input_tokens", "attempts.output_tokens", "attempts.cost_usd"}
	where := []map[string]any{eq("source", "ai_proxy"), eq("usage_quality", "unknown"), eq("cost_status", "priced")}
	rows, err := c.query(ctx, measures, where)
	if err != nil {
		return nil, err
	}

	byModel := map[modelKey]legacy{}
	for _, row := range rows {
		provider, _ := row[0].(string)
		model, _ := row[1].(string)
		byModel[modelKey{provider, model}] = legacy{attempts: int64(number(row[2])), input: number(row[3]), output: number(row[4]), cost: decimal(row[5])}
	}

	return byModel, nil
}

func rateOf(value *string) (float64, bool) {
	if value == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(*value, 64)

	return n, err == nil
}

func priceOf(book *usage.PriceBook, key modelKey) (rates, bool) {
	event := usage.AccountingEvent{
		Provider: key.provider, RequestedAlias: key.model, ExecutedModel: key.model,
		Tokens: &usage.AccountingTokens{Breakdown: usage.NewSubsetTokenBreakdown(1, 0, 0, 0, 0, 1)},
	}
	found := book.Estimate(event).Rates
	if found == nil {
		return rates{}, false
	}

	input, okInput := rateOf(found.Input)
	cacheRead, okRead := rateOf(found.CacheRead)
	cacheWrite, okWrite := rateOf(found.CacheCreation)
	output, okOutput := rateOf(found.Output)
	if !okInput || !okRead || !okWrite || !okOutput {
		return rates{}, false
	}

	reasoning, okReasoning := rateOf(found.Reasoning)
	if !okReasoning {
		reasoning = output
	}

	return rates{input: input, cacheRead: cacheRead, cacheWrite: cacheWrite, output: output, reasoning: reasoning}, true
}

func plan(key modelKey, old legacy, known classified, price rates) estimate {
	e := estimate{key: key, attempts: old.attempts}
	if known.input == 0 || known.output == 0 || old.input == 0 {
		e.skipped = "no classified usage to interpolate from"

		return e
	}
	if price.cacheRead == price.input {
		e.skipped = "cache reads cost the same as input, the split cannot be solved from the cost"

		return e
	}

	e.writeShare = known.cacheWrite / known.input
	e.reasoning = known.reasoning / known.output
	e.classified = known.cacheRead / known.input

	outputCost := old.output * (e.reasoning*price.reasoning + (1-e.reasoning)*price.output)
	perInput := (old.cost - outputCost) / old.input
	e.readShare = (perInput - e.writeShare*price.cacheWrite - (1-e.writeShare)*price.input) / (price.cacheRead - price.input)
	if e.readShare >= 0 && e.readShare <= 1-e.writeShare {
		return e
	}

	uncached := 1 - e.writeShare - e.classified
	if known.cacheWrite == 0 || price.cacheWrite == price.cacheRead {
		e.skipped = fmt.Sprintf("the stored cost implies a cache read share of %.3f, outside what is possible", e.readShare)

		return e
	}

	classifiedWrite := e.writeShare
	e.writeShare = (perInput - (1-uncached)*price.cacheRead - uncached*price.input) / (price.cacheWrite - price.cacheRead)
	e.readShare = 1 - uncached - e.writeShare
	if e.writeShare < 0 || e.writeShare > classifiedWrite || e.readShare < 0 {
		e.skipped = fmt.Sprintf("the stored cost implies a cache write share of %.3f and a read share of %.3f, outside what is possible", e.writeShare, e.readShare)
	}

	return e
}

func share(value float64) float64 {
	return math.Round(value*1e6) / 1e6
}

func (e estimate) where() map[string]any {
	return map[string]any{
		"provider": e.key.provider, "executed_model": e.key.model,
		"source": "ai_proxy", "usage_quality": "unknown", "cost_status": "priced", "cache_read_tokens": nil,
	}
}

func (e estimate) set() map[string]any {
	read, write, reasoning := share(e.readShare), share(e.writeShare), share(e.reasoning)

	return map[string]any{
		"usage_quality":               "estimated",
		"unclassified_tokens":         0,
		"cache_read_tokens":           map[string]any{"multiply": []any{"input_tokens", read}},
		"cache_write_tokens":          map[string]any{"multiply": []any{"input_tokens", write}},
		"input_uncached_tokens":       map[string]any{"subtract": []any{"input_tokens", map[string]any{"multiply": []any{"input_tokens", share(read + write)}}}},
		"reasoning_tokens":            map[string]any{"multiply": []any{"output_tokens", reasoning}},
		"output_non_reasoning_tokens": map[string]any{"subtract": []any{"output_tokens", map[string]any{"multiply": []any{"output_tokens", reasoning}}}},
	}
}

func (c client) apply(ctx context.Context, estimates []estimate) error {
	for _, e := range estimates {
		var result struct {
			Rows int `json:"rows"`
		}
		body := map[string]any{"where": e.where(), "set": e.set()}
		if err := c.call(ctx, http.MethodPatch, "/api/datasets/"+c.attempts+"/rows", body, &result); err != nil {
			return err
		}
		if int64(result.Rows) != e.attempts {
			return fmt.Errorf("patched %d rows where %d attempts were planned for %s/%s", result.Rows, e.attempts, e.key.provider, e.key.model)
		}
	}

	return nil
}

func report(out io.Writer, estimates []estimate) {
	sort.Slice(estimates, func(i, j int) bool { return estimates[i].attempts > estimates[j].attempts })

	for _, e := range estimates {
		if e.skipped != "" {
			_, _ = fmt.Fprintf(out, "skip %s/%s (%d attempts): %s\n", e.key.provider, e.key.model, e.attempts, e.skipped)
			continue
		}
		_, _ = fmt.Fprintf(out, "%6d  %s/%s read=%.3f (classified rows %.3f) write=%.3f reasoning=%.3f\n", e.attempts, e.key.provider, e.key.model, e.readShare, e.classified, e.writeShare, e.reasoning)
	}
}

func selected(providers string) map[string]bool {
	chosen := map[string]bool{}
	for _, provider := range strings.Split(providers, ",") {
		chosen[strings.TrimSpace(provider)] = true
	}

	return chosen
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("estimate_llm_cache", flag.ContinueOnError)
	catalogPath := flags.String("catalog", "", "LiteLLM price catalog file, the one ai-proxy publishes prices from")
	apply := flags.Bool("apply", false, "Patch the attempts, default is a dry run")
	providers := flags.String("providers", "claude,codex", "Providers whose old attempts are estimated")
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

	c := client{http: &http.Client{Timeout: 5 * time.Minute}, url: *url, token: os.Getenv(*tokenEnv), attempts: *attempts, prices: *prices}
	known, err := c.classified(ctx)
	if err != nil {
		return err
	}
	old, err := c.legacy(ctx)
	if err != nil {
		return err
	}

	chosen := selected(*providers)
	var estimates []estimate
	for key, sums := range old {
		if !chosen[key.provider] {
			continue
		}
		price, priced := priceOf(book, key)
		if !priced {
			estimates = append(estimates, estimate{key: key, attempts: sums.attempts, skipped: "no complete price in the catalog"})
			continue
		}
		estimates = append(estimates, plan(key, sums, known[key], price))
	}

	report(out, estimates)
	if !*apply {
		return nil
	}

	var ready []estimate
	for _, e := range estimates {
		if e.skipped == "" {
			ready = append(ready, e)
		}
	}

	return c.apply(ctx, ready)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
