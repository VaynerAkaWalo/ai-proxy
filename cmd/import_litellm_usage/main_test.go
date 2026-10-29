package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func number(value int64) *int64 { return &value }
func text(value string) *string { return &value }

func sourceLog() spendLog {
	return spendLog{
		RequestID: "request", CallType: "acompletion", Provider: "openai", Model: "m",
		StartedAt:   time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 8, 12, 12, 0, 1, 0, time.UTC),
		Status:      "success", Spend: text("1.2345678901234e-6"),
		PromptTokens: number(100), CompletionTokens: number(20), TotalTokens: number(120),
	}
}

func writeExport(t *testing.T, logs ...spendLog) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(map[string]int{"export_total_rows": len(logs)}); err != nil {
		t.Fatal(err)
	}
	for _, log := range logs {
		if err := encoder.Encode(log); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHistoricalProxyPreservesUnknownBucketsAndSourceIdentity(t *testing.T) {
	log := sourceLog()
	version := 1
	log.ProxySchema, log.CostKind = &version, "api_equivalent_estimate"
	log.ClientID = strings.Repeat("a", 64)
	mapped, spend, err := historicalRow(log, map[string]string{log.ClientID: "canonical-client"})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(mapped)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["source"] != "ai_proxy" || decoded["execution_id"] != "request" || decoded["client_id"] != "canonical-client" || decoded["cost_kind"] != "api_equivalent_estimate" || decoded["cost_usd"] != "0.000001234568" || spend.Sign() <= 0 {
		t.Fatalf("wrong historical facts: %s", raw)
	}
	for _, key := range []string{"input_uncached_tokens", "output_non_reasoning_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "price_id", "pricing_snapshot"} {
		if decoded[key] != nil {
			t.Fatalf("invented %s", key)
		}
	}
	if decoded["input_tokens"] != float64(100) || decoded["output_tokens"] != float64(20) {
		t.Fatalf("lost known parent totals: %s", raw)
	}
	if decoded["usage_quality"] != "unknown" || decoded["unclassified_tokens"] != float64(120) {
		t.Fatalf("wrong quality: %s", raw)
	}
}

func TestHistoricalNativeDisjointTokenBuckets(t *testing.T) {
	log := sourceLog()
	log.CacheReadTokens, log.CacheWriteTokens, log.ReasoningTokens = number(70), number(10), number(5)
	mapped, _, err := historicalRow(log, nil)
	if err != nil {
		t.Fatal(err)
	}

	if *mapped["input_uncached_tokens"].(*int64) != 20 || *mapped["output_non_reasoning_tokens"].(*int64) != 15 || mapped["usage_quality"] != "complete" || mapped["unclassified_tokens"] != int64(0) || mapped["source"] != "litellm" || mapped["cost_kind"] != "source_reported" {
		t.Fatalf("wrong canonical buckets: %+v", mapped)
	}
	log.ReasoningTokens = number(30)
	mapped, _, err = historicalRow(log, nil)
	if err != nil || mapped["usage_quality"] != "inconsistent" || mapped["output_non_reasoning_tokens"].(*int64) != nil {
		t.Fatalf("invalid breakdown not retained: %+v %v", mapped, err)
	}
}

func TestPrepareSkipsLiveExistingAndAdministrativeRecords(t *testing.T) {
	cutoff := time.Date(2026, 10, 28, 23, 24, 5, 0, time.UTC)
	version := 1
	history := sourceLog()
	history.RequestID = "history"
	history.ProxySchema = &version
	existing := history
	existing.RequestID = "existing"
	live := history
	live.RequestID, live.StartedAt, live.CompletedAt = "live", cutoff, cutoff
	admin := sourceLog()
	admin.CallType, admin.Model = "/team/list", ""
	native := sourceLog()
	native.StartedAt, native.CompletedAt = cutoff, cutoff

	rows, _, stats, err := plan{existing: map[string]bool{"ai_proxy\x00existing": true}, cutoff: cutoff}.prepare(writeExport(t, history, existing, live, admin, native))
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 || stats.Prepared != 2 || stats.Existing != 1 || stats.Live != 1 || stats.NonLLM != 1 || stats.SpendUSD != "0.000002469136" || stats.BySource["ai_proxy"] != 1 || stats.BySource["litellm"] != 1 {
		t.Fatalf("wrong migration plan: %+v", stats)
	}
}

func TestPrepareRejectsIncompleteExportAndDuplicateIdentity(t *testing.T) {
	log := sourceLog()
	for _, content := range []string{
		`{"export_total_rows":2}` + "\n" + `{"call_type":"/team/list"}`,
		`{"call_type":"/team/list"}`,
	} {
		path := filepath.Join(t.TempDir(), "broken.jsonl")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := (plan{cutoff: time.Now()}).prepare(path); err == nil {
			t.Fatal("accepted an incomplete export")
		}
	}
	if _, _, _, err := (plan{cutoff: time.Now()}).prepare(writeExport(t, log, log)); err == nil {
		t.Fatal("accepted a duplicate source identity")
	}
}

func TestHistoricalInvalidFactsFailBeforeUpload(t *testing.T) {
	for _, value := range []string{"NaN", "Infinity", "-1", "1000000000000", "1/2", "0x10"} {
		log := sourceLog()
		log.Spend = &value
		if _, _, err := historicalRow(log, nil); err == nil {
			t.Fatalf("accepted invalid spend %s", value)
		}
	}
	log := sourceLog()
	log.PromptTokens = number(-1)
	if _, _, err := historicalRow(log, nil); err == nil {
		t.Fatal("accepted negative tokens")
	}

	bad := filepath.Join(t.TempDir(), "invalid.jsonl")
	if err := os.WriteFile(bad, []byte(`{"export_total_rows":1}`+"\n"+`{"request_id":"bad","call_type":"acompletion"}`), 0600); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(t.TempDir(), "existing.jsonl")
	if err := os.WriteFile(existing, nil, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "rows.ndjson")
	err := run(context.Background(), []string{"--input", bad, "--existing", existing, "--output", output, "--proxy-before", "2026-10-28T23:24:05Z", "--apply"})
	if err == nil {
		t.Fatal("accepted invalid input")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("wrote output before validation completed")
	}
}

func TestUploadAcknowledgementsAndReplayDeduplication(t *testing.T) {
	stored := map[string]bool{}
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		count := 0
		for {
			var row row
			err := decoder.Decode(&row)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			stored[rowKey(row)] = true
			count++
		}
		if fail {
			fail = false
			w.WriteHeader(503)
			return
		}
		_, _ = fmt.Fprintf(w, `{"rows":%d}`, count)
	}))
	defer server.Close()
	rows := []row{}
	for i := 0; i < 300; i++ {
		rows = append(rows, row{"source": "litellm", "execution_id": fmt.Sprint(i)})
	}

	if err := upload(context.Background(), server.Client(), server.URL, "", rows); err == nil {
		t.Fatal("ignored an unacknowledged batch")
	}
	if err := upload(context.Background(), server.Client(), server.URL, "", rows); err != nil {
		t.Fatal(err)
	}

	if len(stored) != 300 {
		t.Fatalf("replay created duplicates: %d", len(stored))
	}
}

func TestHistoricalProjectionNeverForwardsPayloadsOrCredentials(t *testing.T) {
	log := sourceLog()
	encoded, err := json.Marshal(log)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	fields["messages"], fields["response"], fields["api_key"] = "private-prompt", "private-response", "private-key"
	fields["client_id"] = "private-key"
	encoded, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &log); err != nil {
		t.Fatal(err)
	}

	projected, _, err := historicalRow(log, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}

	for _, private := range []string{"private-prompt", "private-response", "private-key"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("forwarded private source data")
		}
	}
}

func TestHistoricalNormalizesNamesAndDropsFailedCallEstimates(t *testing.T) {
	log := sourceLog()
	log.Model, log.RequestedModel, log.SessionID = "openai/gpt-6.1-sol", "gpt-6.1-sol", "session"
	mapped, _, err := historicalRow(log, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mapped["provider"] != "codex" || mapped["executed_model"] != "gpt-6.1-sol" || mapped["requested_model"] != "gpt-6.1-sol" || mapped["session_id"] != "session" {
		t.Fatalf("names not canonical: %+v", mapped)
	}

	log.Provider, log.Model = "xai", "xai/grok-4.6"
	version := 1
	log.ProxySchema = &version
	mapped, _, err = historicalRow(log, nil)
	if err != nil || mapped["provider"] != "xai" || mapped["executed_model"] != "grok-4.6" || mapped["session_id"] != nil {
		t.Fatalf("proxy row kept LiteLLM session or provider prefix: %+v %v", mapped, err)
	}

	failed := sourceLog()
	failed.Status, failed.Spend = "failure", text("0")
	mapped, spend, err := historicalRow(failed, nil)
	if err != nil || mapped["cost_status"] != "unpriced" || mapped["cost_usd"] != nil || spend.Sign() != 0 {
		t.Fatalf("failed call kept LiteLLM spend: %+v %v", mapped, err)
	}
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens", "unclassified_tokens"} {
		if value, ok := mapped[key].(*int64); (ok && value != nil) || (!ok && mapped[key] != nil) {
			t.Fatalf("failed call kept LiteLLM estimate %s", key)
		}
	}
}

func testCatalog(t *testing.T) *usage.PriceBook {
	t.Helper()
	book, err := usage.NewLiteLLMPriceBook([]byte(`{"gpt-6.1-sol":{"litellm_provider":"openai","mode":"chat","input_cost_per_token":2e-6,"cache_read_input_token_cost":1e-7,"output_cost_per_token":1e-5}}`))
	if err != nil {
		t.Fatal(err)
	}
	return book
}

func TestRepriceReplacesSpendOnlyForCompleteUsage(t *testing.T) {
	book := testCatalog(t)
	log := sourceLog()
	log.Model = "openai/gpt-6.1-sol"
	log.CacheReadTokens, log.CacheWriteTokens, log.ReasoningTokens = number(70), number(0), number(5)
	mapped, _, err := historicalRow(log, nil)
	if err != nil {
		t.Fatal(err)
	}
	price, err := reprice(mapped, book)
	if err != nil || price == nil {
		t.Fatalf("complete usage not repriced: %v", err)
	}
	if mapped["cost_usd"] != "0.000267000" || mapped["cost_kind"] != "api_equivalent_estimate" || mapped["cost_status"] != "priced" || mapped["price_id"] == nil || mapped["pricing_snapshot"] == nil {
		t.Fatalf("wrong estimate: %+v", mapped)
	}
	raw, _ := json.Marshal(price)
	if !strings.Contains(string(raw), `"price_id":"`+mapped["price_id"].(string)+`"`) || !strings.Contains(string(raw), `"input_usd_per_mtok":"2"`) {
		t.Fatalf("price record does not match the attempt: %s", raw)
	}

	log.CacheWriteTokens = nil
	mapped, _, err = historicalRow(log, nil)
	if err != nil {
		t.Fatal(err)
	}
	price, err = reprice(mapped, book)
	if err != nil || price != nil || mapped["cost_kind"] != "source_reported" || mapped["cost_usd"] != "0.000001234568" {
		t.Fatalf("incomplete usage lost LiteLLM spend: %+v %v", mapped, err)
	}
}

func TestReplaceResendsExistingRowsAndRepairsLiveTotals(t *testing.T) {
	version := 1
	proxied := sourceLog()
	proxied.RequestID, proxied.ProxySchema = "live", &version
	export := writeExport(t, sourceLog(), proxied)
	existing := filepath.Join(t.TempDir(), "existing.jsonl")
	snapshot := `{"source":"litellm","execution_id":"request"}
{"source":"ai_proxy","execution_id":"live","account_id":"account","started_at":"2026-10-28 23:46:04.793","completed_at":"2026-10-28 23:46:18.482","usage_quality":"complete","input_tokens":null,"output_tokens":null,"input_uncached_tokens":398,"cache_read_tokens":208384,"cache_write_tokens":0,"output_non_reasoning_tokens":40,"reasoning_tokens":231,"cost_usd":"0.0243444"}
{"source":"ai_proxy","execution_id":"later","usage_quality":"complete","input_tokens":5}
`
	if err := os.WriteFile(existing, []byte(snapshot), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "rows.ndjson")
	if err := run(context.Background(), []string{"--input", export, "--existing", existing, "--output", output, "--proxy-before", "2026-10-28T23:24:05Z", "--replace"}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"execution_id":"request"`) {
		t.Fatalf("existing row not re-sent: %s", raw)
	}
	for _, expected := range []string{`"input_tokens":208782`, `"output_tokens":271`, `"started_at":"2026-10-28T23:46:04.793Z"`, `"cost_usd":"0.0243444"`} {
		if !strings.Contains(lines[1], expected) {
			t.Fatalf("live row repair lacks %s: %s", expected, lines[1])
		}
	}
}
