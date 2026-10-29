package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type spendLog struct {
	ExportTotalRows  *int      `json:"export_total_rows"`
	RequestID        string    `json:"request_id"`
	CallType         string    `json:"call_type"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	RequestedModel   string    `json:"requested_model"`
	ClientID         string    `json:"client_id"`
	SessionID        string    `json:"session_id"`
	StartedAt        time.Time `json:"started_at"`
	CompletedAt      time.Time `json:"completed_at"`
	Status           string    `json:"status"`
	Spend            *string   `json:"spend"`
	PromptTokens     *int64    `json:"prompt_tokens"`
	CompletionTokens *int64    `json:"completion_tokens"`
	TotalTokens      *int64    `json:"total_tokens"`
	CacheReadTokens  *int64    `json:"cache_read_tokens"`
	CacheWriteTokens *int64    `json:"cache_write_tokens"`
	ReasoningTokens  *int64    `json:"reasoning_tokens"`
	CostKind         string    `json:"cost_kind"`
	ProxySchema      *int      `json:"proxy_schema"`
}

type row map[string]any

type report struct {
	Read                int            `json:"read"`
	Prepared            int            `json:"prepared"`
	Existing            int            `json:"skipped_existing"`
	Live                int            `json:"skipped_live_proxy"`
	NonLLM              int            `json:"skipped_non_llm"`
	BySource            map[string]int `json:"by_source"`
	SpendUSD            string         `json:"spend_usd"`
	RoundingDeltaUSD    string         `json:"rounding_delta_usd"`
	Repriced            int            `json:"repriced"`
	RepricedSpendUSD    string         `json:"repriced_source_spend_usd"`
	RepricedEstimateUSD string         `json:"repriced_estimate_usd"`
	Prices              int            `json:"prices"`
	RepairedTotals      int            `json:"repaired_live_totals"`
}

type plan struct {
	// existing maps IDV keys to whether live export wrote the row.
	existing map[string]bool
	clients  map[string]string
	cutoff   time.Time
	book     *usage.PriceBook
	replace  bool
}

var sourceAmount = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]{1,3})?$`)
var decimalAmount = regexp.MustCompile(`^(0|[1-9]\d{0,11})\.\d{12}$`)
var datasetID = regexp.MustCompile(`^[a-z0-9_-]+/[a-z0-9_]+$`)
var keyHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func nullable(value string) any {
	if value == "" || value == "None" || value == "null" {
		return nil
	}
	return value
}

// LiteLLM routed these models through ai-proxy's Codex executor.
var codexProviders = map[string]bool{"openai": true, "chatgpt": true}

func canonicalModel(model string) string {
	for _, prefix := range []string{"openai/", "xai/", "chatgpt/"} {
		model = strings.TrimPrefix(model, prefix)
	}
	return model
}

func isModelCall(log spendLog) bool {
	switch log.CallType {
	case "anthropic_messages", "acompletion", "completion", "aresponses", "responses":
		return true
	case "":
		return log.Model != ""
	default:
		return false
	}
}

func historicalRow(log spendLog, clients map[string]string) (row, *big.Rat, error) {
	if log.RequestID == "" || log.StartedAt.IsZero() || log.CompletedAt.Before(log.StartedAt) {
		return nil, nil, errors.New("invalid identity or timestamps")
	}
	status := ""
	switch log.Status {
	case "success":
		status = "succeeded"
	case "failure":
		status = "failed"
	default:
		return nil, nil, errors.New("unknown request status")
	}
	if status == "failed" {
		// LiteLLM logs a zero spend and its own prompt estimate for failed calls, not provider usage.
		log.Spend, log.PromptTokens, log.CompletionTokens, log.TotalTokens = nil, nil, nil, nil
		log.CacheReadTokens, log.CacheWriteTokens, log.ReasoningTokens = nil, nil, nil
	}
	for _, count := range []*int64{log.PromptTokens, log.CompletionTokens, log.TotalTokens, log.CacheReadTokens, log.CacheWriteTokens, log.ReasoningTokens} {
		if count != nil && (*count < 0 || *count > 9007199254740991) {
			return nil, nil, errors.New("invalid token count")
		}
	}
	source, session := "litellm", log.SessionID
	if log.ProxySchema != nil {
		if *log.ProxySchema != 1 {
			return nil, nil, errors.New("unknown proxy accounting schema")
		}
		// LiteLLM generated a session per proxy-reported call.
		source, session = "ai_proxy", ""
	}
	provider := log.Provider
	if codexProviders[provider] {
		provider = "codex"
	}
	client := ""
	if keyHash.MatchString(log.ClientID) {
		client = clients[log.ClientID]
		if client == "" {
			client = "litellm_key:" + log.ClientID
		}
	}
	requested := canonicalModel(log.RequestedModel)
	if requested == "" {
		requested = canonicalModel(log.Model)
	}
	kind := log.CostKind
	if kind == "" {
		kind = "source_reported"
	}

	result := row{
		"source": source, "execution_id": log.RequestID,
		"started_at": log.StartedAt.UTC(), "completed_at": log.CompletedAt.UTC(),
		"provider": nullable(provider), "executed_model": nullable(canonicalModel(log.Model)), "requested_model": nullable(requested),
		"client_id": nullable(client), "session_id": nullable(session),
		"status": status, "usage_quality": "unknown", "cost_status": "unpriced", "cost_kind": nil,
		"input_tokens": log.PromptTokens, "output_tokens": log.CompletionTokens,
		"total_tokens":      log.TotalTokens,
		"cache_read_tokens": log.CacheReadTokens, "cache_write_tokens": log.CacheWriteTokens, "reasoning_tokens": log.ReasoningTokens,
	}
	var input, output *int64
	if log.PromptTokens != nil && log.CacheReadTokens != nil && log.CacheWriteTokens != nil {
		value := *log.PromptTokens - *log.CacheReadTokens - *log.CacheWriteTokens
		if value < 0 {
			result["usage_quality"] = "inconsistent"
		} else {
			input = &value
		}
	}
	if log.CompletionTokens != nil && log.ReasoningTokens != nil {
		value := *log.CompletionTokens - *log.ReasoningTokens
		if value < 0 {
			result["usage_quality"] = "inconsistent"
		} else {
			output = &value
		}
	}
	result["input_uncached_tokens"], result["output_non_reasoning_tokens"] = input, output
	if log.TotalTokens != nil {
		known := int64(0)
		for _, count := range []*int64{input, output, log.CacheReadTokens, log.CacheWriteTokens, log.ReasoningTokens} {
			if count != nil {
				known += *count
			}
		}
		remaining := *log.TotalTokens - known
		if remaining >= 0 {
			result["unclassified_tokens"] = remaining
		} else {
			result["usage_quality"] = "inconsistent"
		}
		if log.PromptTokens != nil && log.CompletionTokens != nil && *log.PromptTokens+*log.CompletionTokens != *log.TotalTokens {
			result["usage_quality"] = "inconsistent"
		}
		if input != nil && output != nil && remaining == 0 && result["usage_quality"] != "inconsistent" {
			result["usage_quality"] = "complete"
		}
	}

	spend := new(big.Rat)
	if log.Spend != nil {
		if !sourceAmount.MatchString(*log.Spend) {
			return nil, nil, errors.New("invalid source spend format")
		}
		var ok bool
		spend, ok = new(big.Rat).SetString(*log.Spend)
		if !ok || spend.Sign() < 0 {
			return nil, nil, errors.New("invalid source spend")
		}
		amount := spend.FloatString(12)
		if !decimalAmount.MatchString(amount) {
			return nil, nil, errors.New("spend exceeds Decimal(24,12)")
		}
		result["cost_usd"], result["cost_status"], result["cost_kind"] = amount, "priced", kind
	}
	return result, spend, nil
}

func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

// reprice replaces LiteLLM's spend with ai-proxy's estimate so history has a single cost kind.
// Cache buckets LiteLLM did not record are priced as zero, which matches xAI's catalog having no
// cache-write price, while the row itself keeps them unknown. The catalog has no reasoning rates,
// so all output is priced at the output rate regardless of how LiteLLM split it.
func reprice(result row, book *usage.PriceBook) (any, error) {
	prompt, completion := result["input_tokens"].(*int64), result["output_tokens"].(*int64)
	if book == nil || result["source"] != "litellm" || result["status"] != "succeeded" || prompt == nil || completion == nil {
		return nil, nil
	}
	known := func(name string) int64 {
		if count := result[name].(*int64); count != nil {
			return *count
		}
		return 0
	}
	read, write := known("cache_read_tokens"), known("cache_write_tokens")
	if read+write > *prompt {
		return nil, nil
	}
	breakdown := usage.TokenBreakdown{
		SchemaVersion: usage.TokenAccountingSchemaVersion, Quality: usage.TokenAccountingQualityComplete,
		TotalTokens: *prompt + *completion,
		Input: usage.TokenInputBreakdown{
			TotalTokens: *prompt, UncachedTokens: *prompt - read - write, CacheReadTokens: read, CacheWriteTokens: write,
		},
		Output: usage.TokenOutputBreakdown{TotalTokens: *completion, NonReasoningTokens: *completion},
	}
	estimate := book.Estimate(usage.AccountingEvent{
		Provider: stringValue(result["provider"]), ExecutedModel: stringValue(result["executed_model"]), RequestedAlias: stringValue(result["requested_model"]),
		Status: "succeeded", Tokens: &usage.AccountingTokens{Breakdown: breakdown},
	})
	if estimate.Status != "priced" || estimate.Currency != "USD" {
		return nil, nil
	}

	record, id, err := usage.IDVPriceRecord(estimate)
	if err != nil {
		return nil, err
	}
	result["cost_usd"], result["cost_kind"], result["price_id"], result["pricing_snapshot"] = *estimate.Amount, estimate.Kind, id, estimate.Snapshot
	return record, nil
}

// repairTotals fills the input and output totals that live export omitted before it kept them,
// derived from the complete disjoint buckets of the same row.
func repairTotals(snapshot row) (row, error) {
	if snapshot["source"] != "ai_proxy" || snapshot["usage_quality"] != "complete" || snapshot["input_tokens"] != nil {
		return nil, nil
	}
	sum := func(names ...string) (int64, error) {
		total := int64(0)
		for _, name := range names {
			value, ok := snapshot[name].(json.Number)
			if !ok {
				return 0, fmt.Errorf("complete row lacks %s", name)
			}
			count, err := value.Int64()
			if err != nil {
				return 0, err
			}
			total += count
		}
		return total, nil
	}
	input, err := sum("input_uncached_tokens", "cache_read_tokens", "cache_write_tokens")
	if err != nil {
		return nil, err
	}
	output, err := sum("output_non_reasoning_tokens", "reasoning_tokens")
	if err != nil {
		return nil, err
	}

	repaired := row{}
	for name, value := range snapshot {
		repaired[name] = value
	}
	repaired["input_tokens"], repaired["output_tokens"] = input, output
	for _, name := range []string{"started_at", "completed_at"} {
		// ClickHouse exports DateTime64(3, 'UTC') without a zone, IDV requires one.
		at, err := time.Parse("2006-01-02 15:04:05.999", stringValue(snapshot[name]))
		if err != nil {
			return nil, err
		}
		repaired[name] = at.UTC()
	}
	return repaired, nil
}

func rowKey(row row) string {
	return fmt.Sprint(row["source"]) + "\x00" + fmt.Sprint(row["execution_id"])
}

func readLines(path string, consume func([]byte) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		if err := consume(scanner.Bytes()); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
	}
	return scanner.Err()
}

func (p plan) prepare(input string) ([]row, []any, report, error) {
	rows, prices := []row{}, []any{}
	stats := report{BySource: map[string]int{}}
	seen, priced := map[string]bool{}, map[string]bool{}
	original, rounded := new(big.Rat), new(big.Rat)
	repricedSpend, repricedEstimate := new(big.Rat), new(big.Rat)
	expectedRows := -1
	err := readLines(input, func(raw []byte) error {
		var log spendLog
		if err := json.Unmarshal(raw, &log); err != nil {
			return err
		}
		if log.ExportTotalRows != nil {
			if expectedRows != -1 || stats.Read != 0 || *log.ExportTotalRows < 0 {
				return errors.New("invalid export header")
			}
			expectedRows = *log.ExportTotalRows
			return nil
		}
		stats.Read++
		if !isModelCall(log) {
			stats.NonLLM++
			return nil
		}
		if log.ProxySchema != nil && !log.StartedAt.Before(p.cutoff) {
			stats.Live++
			return nil
		}
		row, spend, err := historicalRow(log, p.clients)
		if err != nil {
			return err
		}
		key := rowKey(row)
		if seen[key] {
			return errors.New("duplicate source identity")
		}
		seen[key] = true
		// Live rows carry facts LiteLLM never had, so even a replace keeps them.
		if live, ok := p.existing[key]; ok && (live || !p.replace) {
			stats.Existing++
			return nil
		}

		original.Add(original, spend)
		if amount, ok := row["cost_usd"].(string); ok {
			value, _ := new(big.Rat).SetString(amount)
			rounded.Add(rounded, value)
		}
		source := row["cost_usd"]
		price, err := reprice(row, p.book)
		if err != nil {
			return err
		}
		if price != nil {
			stats.Repriced++
			value, _ := new(big.Rat).SetString(source.(string))
			repricedSpend.Add(repricedSpend, value)
			value, _ = new(big.Rat).SetString(row["cost_usd"].(string))
			repricedEstimate.Add(repricedEstimate, value)
			if id := row["price_id"].(string); !priced[id] {
				priced[id] = true
				prices = append(prices, price)
			}
		}
		rows = append(rows, row)
		stats.BySource[row["source"].(string)]++
		return nil
	})
	if err == nil && (expectedRows < 0 || expectedRows != stats.Read) {
		err = errors.New("incomplete export or missing row-count header")
	}
	stats.Prepared, stats.Prices = len(rows), len(prices)
	stats.SpendUSD = rounded.FloatString(12)
	stats.RoundingDeltaUSD = new(big.Rat).Sub(rounded, original).FloatString(18)
	stats.RepricedSpendUSD, stats.RepricedEstimateUSD = repricedSpend.FloatString(12), repricedEstimate.FloatString(12)
	return rows, prices, stats, err
}

func upload[T any](ctx context.Context, client *http.Client, endpoint, token string, rows []T) error {
	for start := 0; start < len(rows); start += 256 {
		batch := rows[start:min(start+256, len(rows))]
		var body bytes.Buffer
		encoder := json.NewEncoder(&body)
		for _, row := range batch {
			if err := encoder.Encode(row); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("batch starting at %d: %w", start, err)
		}
		raw, errRead := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
		_ = response.Body.Close()
		var acknowledged struct {
			Rows int `json:"rows"`
		}
		if response.StatusCode != http.StatusOK || errRead != nil || json.Unmarshal(raw, &acknowledged) != nil || acknowledged.Rows != len(batch) {
			return fmt.Errorf("batch starting at %d not acknowledged, HTTP %d", start, response.StatusCode)
		}
		fmt.Fprintf(os.Stderr, "acknowledged %d/%d rows\n", start+len(batch), len(rows))
	}
	return nil
}

func writeNDJSON[T any](path string, rows []T) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			_ = file.Close()
			return err
		}
	}
	return file.Close()
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("import_litellm_usage", flag.ContinueOnError)
	input := flags.String("input", "", "Whitelisted JSONL export from export.sql")
	output := flags.String("output", "", "New NDJSON file for prepared IDV rows")
	existingPath := flags.String("existing", "", "JSONEachRow snapshot of the existing IDV attempts")
	clientMapPath := flags.String("client-map", "", "Optional JSON map from LiteLLM key hashes to canonical client IDs")
	before := flags.String("proxy-before", "", "UTC cutover timestamp, newer proxy rows are supplied by live export")
	replace := flags.Bool("replace", false, "Re-send existing rows corrected and repair live rows missing totals, default skips existing rows")
	catalogPath := flags.String("catalog", "", "Optional LiteLLM price catalog to reprice history with complete usage")
	pricesOutput := flags.String("prices-output", "", "New NDJSON file for the price records used, required with catalog")
	apply := flags.Bool("apply", false, "Upload prepared rows after validation, default is dry run")
	url := flags.String("url", "https://analytics.blamedevs.com", "IDV base URL")
	dataset := flags.String("dataset", "pawel/llm_usage", "Destination dataset owner/name")
	pricesDataset := flags.String("prices-dataset", "pawel/llm_prices", "Destination prices dataset owner/name")
	tokenEnv := flags.String("token-env", "", "Optional environment variable containing a bearer token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cutoff, err := time.Parse(time.RFC3339Nano, *before)
	if err != nil || *input == "" || *output == "" || *existingPath == "" || !datasetID.MatchString(*dataset) || !datasetID.MatchString(*pricesDataset) {
		return errors.New("input, output, existing, a proxy-before timestamp and owner/name datasets are required")
	}
	if (*catalogPath == "") != (*pricesOutput == "") {
		return errors.New("catalog and prices-output are required together")
	}
	if *tokenEnv != "" && os.Getenv(*tokenEnv) == "" {
		return errors.New("bearer token environment variable is empty")
	}
	p := plan{existing: map[string]bool{}, clients: map[string]string{}, cutoff: cutoff, replace: *replace}
	if *clientMapPath != "" {
		raw, err := os.ReadFile(*clientMapPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &p.clients); err != nil {
			return err
		}
	}
	if *catalogPath != "" {
		raw, err := os.ReadFile(*catalogPath)
		if err != nil {
			return err
		}
		if p.book, err = usage.NewLiteLLMPriceBook(raw); err != nil {
			return err
		}
	}
	repairs := []row{}
	if err := readLines(*existingPath, func(raw []byte) error {
		snapshot := row{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&snapshot); err != nil {
			return err
		}
		if stringValue(snapshot["source"]) == "" || stringValue(snapshot["execution_id"]) == "" {
			return errors.New("invalid existing IDV key")
		}
		p.existing[rowKey(snapshot)] = snapshot["account_id"] != nil
		if !*replace {
			return nil
		}
		repaired, err := repairTotals(snapshot)
		if repaired != nil {
			repairs = append(repairs, repaired)
		}
		return err
	}); err != nil {
		return err
	}
	rows, prices, stats, err := p.prepare(*input)
	if err != nil {
		return err
	}
	rows = append(rows, repairs...)
	stats.RepairedTotals = len(repairs)

	if err := writeNDJSON(*output, rows); err != nil {
		return err
	}
	if *pricesOutput != "" {
		if err := writeNDJSON(*pricesOutput, prices); err != nil {
			return err
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(stats); err != nil {
		return err
	}
	if !*apply {
		return nil
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := strings.TrimRight(*url, "/") + "/api/datasets/"
	// Attempts reference their prices, so prices are delivered first.
	if err := upload(ctx, client, base+*pricesDataset+"/rows", os.Getenv(*tokenEnv), prices); err != nil {
		return err
	}
	return upload(ctx, client, base+*dataset+"/rows", os.Getenv(*tokenEnv), rows)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
