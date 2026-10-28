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
	Read             int            `json:"read"`
	Prepared         int            `json:"prepared"`
	Existing         int            `json:"skipped_existing"`
	Live             int            `json:"skipped_live_proxy"`
	NonLLM           int            `json:"skipped_non_llm"`
	BySource         map[string]int `json:"by_source"`
	SpendUSD         string         `json:"spend_usd"`
	RoundingDeltaUSD string         `json:"rounding_delta_usd"`
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
	for _, count := range []*int64{log.PromptTokens, log.CompletionTokens, log.TotalTokens, log.CacheReadTokens, log.CacheWriteTokens, log.ReasoningTokens} {
		if count != nil && (*count < 0 || *count > 9007199254740991) {
			return nil, nil, errors.New("invalid token count")
		}
	}
	source := "litellm"
	if log.ProxySchema != nil {
		if *log.ProxySchema != 1 {
			return nil, nil, errors.New("unknown proxy accounting schema")
		}
		source = "ai_proxy"
	}
	client := ""
	if keyHash.MatchString(log.ClientID) {
		client = clients[log.ClientID]
		if client == "" {
			client = "litellm_key:" + log.ClientID
		}
	}
	requested := log.RequestedModel
	if requested == "" {
		requested = log.Model
	}
	kind := log.CostKind
	if kind == "" {
		kind = "source_reported"
	}

	result := row{
		"source": source, "execution_id": log.RequestID,
		"started_at": log.StartedAt.UTC(), "completed_at": log.CompletedAt.UTC(),
		"provider": nullable(log.Provider), "executed_model": nullable(log.Model), "requested_model": nullable(requested),
		"client_id": nullable(client), "session_id": nullable(log.SessionID),
		"status": status, "usage_quality": "unknown", "cost_status": "unpriced", "cost_kind": kind,
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
		result["cost_usd"], result["cost_status"] = amount, "priced"
	}
	return result, spend, nil
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

func prepare(input string, existing map[string]bool, clients map[string]string, cutoff time.Time) ([]row, report, error) {
	rows := []row{}
	stats := report{BySource: map[string]int{}}
	seen := map[string]bool{}
	original, rounded := new(big.Rat), new(big.Rat)
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
		if log.ProxySchema != nil && !log.StartedAt.Before(cutoff) {
			stats.Live++
			return nil
		}
		row, spend, err := historicalRow(log, clients)
		if err != nil {
			return err
		}
		key := rowKey(row)
		if seen[key] {
			return errors.New("duplicate source identity")
		}
		seen[key] = true
		if existing[key] {
			stats.Existing++
			return nil
		}

		rows = append(rows, row)
		stats.BySource[row["source"].(string)]++
		original.Add(original, spend)
		if amount, ok := row["cost_usd"].(string); ok {
			value, _ := new(big.Rat).SetString(amount)
			rounded.Add(rounded, value)
		}
		return nil
	})
	if err == nil && (expectedRows < 0 || expectedRows != stats.Read) {
		err = errors.New("incomplete export or missing row-count header")
	}
	stats.Prepared = len(rows)
	stats.SpendUSD = rounded.FloatString(12)
	stats.RoundingDeltaUSD = new(big.Rat).Sub(rounded, original).FloatString(18)
	return rows, stats, err
}

func upload(ctx context.Context, client *http.Client, endpoint, token string, rows []row) error {
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

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("import_litellm_usage", flag.ContinueOnError)
	input := flags.String("input", "", "Whitelisted JSONL export from export.sql")
	output := flags.String("output", "", "New NDJSON file for prepared IDV rows")
	existingPath := flags.String("existing", "", "JSONL snapshot of existing IDV source/execution_id keys")
	clientMapPath := flags.String("client-map", "", "Optional JSON map from LiteLLM key hashes to canonical client IDs")
	before := flags.String("proxy-before", "", "UTC cutover timestamp, newer proxy rows are supplied by live export")
	apply := flags.Bool("apply", false, "Upload prepared rows after validation, default is dry run")
	url := flags.String("url", "https://analytics.blamedevs.com", "IDV base URL")
	dataset := flags.String("dataset", "pawel/llm_usage", "Destination dataset owner/name")
	tokenEnv := flags.String("token-env", "", "Optional environment variable containing a bearer token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cutoff, err := time.Parse(time.RFC3339Nano, *before)
	if err != nil || *input == "" || *output == "" || *existingPath == "" || !datasetID.MatchString(*dataset) {
		return errors.New("input, output, existing, a proxy-before timestamp and an owner/name dataset are required")
	}
	if *tokenEnv != "" && os.Getenv(*tokenEnv) == "" {
		return errors.New("bearer token environment variable is empty")
	}
	clients := map[string]string{}
	if *clientMapPath != "" {
		raw, err := os.ReadFile(*clientMapPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &clients); err != nil {
			return err
		}
	}
	existing := map[string]bool{}
	if err := readLines(*existingPath, func(raw []byte) error {
		var key struct {
			Source      string `json:"source"`
			ExecutionID string `json:"execution_id"`
		}
		if err := json.Unmarshal(raw, &key); err != nil {
			return err
		}
		if key.Source == "" || key.ExecutionID == "" {
			return errors.New("invalid existing IDV key")
		}
		existing[key.Source+"\x00"+key.ExecutionID] = true
		return nil
	}); err != nil {
		return err
	}
	rows, stats, err := prepare(*input, existing, clients, cutoff)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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
	if err := file.Close(); err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(stats); err != nil {
		return err
	}
	if !*apply {
		return nil
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return upload(ctx, client, strings.TrimRight(*url, "/")+"/api/datasets/"+*dataset+"/rows", os.Getenv(*tokenEnv), rows)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
