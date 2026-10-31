package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func analyticsConfig(url string) config.AnalyticsExporterConfig {
	return config.AnalyticsExporterConfig{Enabled: true, URL: url, Dataset: "pawel/llm_usage"}
}

func TestAnalyticsMappingPreservesAccountingAndOmitsInternalPayloads(t *testing.T) {
	event := exportEvent("mapping")
	event.RequestedAlias, event.ReportedServiceTier = "alias", "priority"
	event.TraceID, event.AccountID, event.SessionID = "trace", "account-hash", "session"
	event.StatusCode = 200
	event.Estimate.Snapshot = "snapshot-hash"

	row, err := newAnalyticsRow(event)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}

	expected := map[string]any{
		"source": "ai_proxy", "execution_id": "mapping", "trace_id": "trace",
		"started_at": event.StartedAt.UTC().Format(time.RFC3339Nano), "completed_at": event.CompletedAt.UTC().Format(time.RFC3339Nano),
		"provider": "openai", "requested_model": "alias", "executed_model": "gpt-4o",
		"client_id": "local-client", "account_id": "account-hash", "session_id": "session", "service_tier": "priority",
		"status": "succeeded", "status_code": float64(200),
		"input_tokens": float64(10), "output_tokens": float64(5),
		"input_uncached_tokens": float64(8), "cache_read_tokens": float64(2), "cache_write_tokens": float64(0),
		"output_non_reasoning_tokens": float64(4), "reasoning_tokens": float64(1), "total_tokens": float64(15),
		"unclassified_tokens": float64(0), "usage_quality": "complete",
		"cost_usd": "0.000123456", "cost_kind": "api_equivalent_estimate", "cost_status": "priced", "pricing_snapshot": "snapshot-hash",
	}
	if !reflect.DeepEqual(payload, expected) {
		t.Fatalf("unexpected payload: %s", raw)
	}
}

func TestAnalyticsMappingKeepsUnknownUsageAndPartialCosts(t *testing.T) {
	event := exportEvent("partial")
	event.Status = "interrupted"
	event.Tokens = nil
	event.Estimate.Status = "partial"
	row, err := newAnalyticsRow(event)
	if err != nil || row.Status != "interrupted" || row.InputTokens != nil || row.OutputTokens != nil || row.InputUncachedTokens != nil || row.TotalTokens != nil || row.UsageQuality != "unknown" || row.CostStatus != "partial" || row.CostUSD == nil {
		t.Fatalf("unknown usage or partial cost lost: %+v, %v", row, err)
	}

	event.Tokens = &AccountingTokens{Breakdown: NewUnclassifiedTokenBreakdown(42)}
	row, err = newAnalyticsRow(event)
	if err != nil || row.InputTokens != nil || row.OutputTokens != nil || row.InputUncachedTokens != nil || row.ReasoningTokens != nil || row.TotalTokens == nil || *row.TotalTokens != 42 || row.UnclassifiedTokens == nil || *row.UnclassifiedTokens != 42 || row.UsageQuality != "unclassified" {
		t.Fatalf("unclassified usage fabricated: %+v, %v", row, err)
	}

	for _, amount := range []string{"NaN", "1e-9", "-1", "1000000000000", "0.1234567890123"} {
		event.Estimate.Status, event.Estimate.Amount = "priced", &amount
		row, err = newAnalyticsRow(event)
		if err != nil || row.CostUSD != nil || row.CostStatus != "unpriced" {
			t.Fatalf("invalid USD amount exported: %+v, %v", row, err)
		}
	}
	event.Estimate.Currency = "EUR"
	event.Estimate.Amount = priceString("1")
	row, err = newAnalyticsRow(event)
	if err != nil || row.CostUSD != nil || row.CostStatus != "unpriced" {
		t.Fatal("foreign currency exported as USD")
	}
}

func TestAnalyticsDeliveryOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		body   string
		state  DeliveryState
		status string
	}{
		{"accepted", 200, `{"rows":1}`, DeliveryAcknowledged, "exported"},
		{"wrong-count", 200, `{"rows":0}`, DeliveryRetryable, "response_invalid"},
		{"malformed", 200, `{`, DeliveryRetryable, "response_invalid"},
		{"auth", 403, `secret echoed`, DeliveryRetryable, "authorization_failure"},
		{"missing-dataset", 404, `secret echoed`, DeliveryRetryable, "dataset_or_route_missing"},
		{"invalid-schema", 400, `secret echoed`, DeliveryRetryable, "schema_failure"},
		{"rate", 429, ``, DeliveryRetryable, "rate_limited"},
		{"outage", 503, ``, DeliveryRetryable, "service_failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/datasets/pawel/llm_usage/rows" || r.Header.Get("Content-Type") != "application/x-ndjson" || r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("invalid ingestion request")
				}
				var row analyticsRow
				if err := json.NewDecoder(r.Body).Decode(&row); err != nil || row.ExecutionID != "one" {
					t.Error("invalid row")
				}
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			t.Setenv("TEST_Analytics_TOKEN", "test-token")
			cfg := analyticsConfig(server.URL)
			cfg.TokenEnv = "TEST_Analytics_TOKEN"
			o := openTestOutbox(t, config.AccountingOutboxConfig{})
			if err := o.Insert(exportEvent("one")); err != nil {
				t.Fatal(err)
			}
			e := NewAnalyticsExporter(o, cfg)

			if err := e.ExportOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			delivery, err := o.Delivery("one")
			if err != nil || delivery.State != tc.state || e.Status() != tc.status || strings.Contains(e.Status(), "secret echoed") {
				t.Fatalf("unexpected outcome: %+v, %s, %v", delivery, e.Status(), err)
			}
			if err := e.ExportOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("replayed before backoff or after acknowledgement")
			}
		})
	}
}

func TestAnalyticsBatchRetainsFailedUnpricedAndUnknownAttempts(t *testing.T) {
	var rows []analyticsRow
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		for {
			var row analyticsRow
			err := decoder.Decode(&row)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			rows = append(rows, row)
		}
		_, _ = fmt.Fprintf(w, `{"rows":%d}`, len(rows))
	}))
	defer server.Close()
	o := openTestOutbox(t, config.AccountingOutboxConfig{})
	for _, status := range []string{"succeeded", "failed", "interrupted"} {
		event := exportEvent(status)
		event.Status, event.Tokens, event.Estimate = status, nil, &CostEstimate{Status: "unpriced"}
		if err := o.Insert(event); err != nil {
			t.Fatal(err)
		}
	}
	e := NewAnalyticsExporter(o, analyticsConfig(server.URL))

	if err := e.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("lost attempts: %d", len(rows))
	}
	for _, row := range rows {
		if row.TotalTokens != nil || row.CostUSD != nil || row.CostStatus != "unpriced" {
			t.Fatalf("fabricated facts: %+v", row)
		}
		if d, err := o.Delivery(row.ExecutionID); err != nil || d.State != DeliveryAcknowledged {
			t.Fatalf("not acknowledged: %+v, %v", d, err)
		}
	}
}

func TestAnalyticsCancellationAndReplayAfterLostResponse(t *testing.T) {
	entered := make(chan struct{})
	first := true
	keys := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var row analyticsRow
		if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
			t.Error(err)
			return
		}
		keys[row.ExecutionID]++
		if first {
			first = false
			close(entered)
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `{"rows":1}`)
	}))
	defer server.Close()
	o := openTestOutbox(t, config.AccountingOutboxConfig{})
	now := time.Unix(1800000000, 0)
	o.now = func() time.Time { return now }
	if err := o.Insert(exportEvent("lost-response")); err != nil {
		t.Fatal(err)
	}
	e := NewAnalyticsExporter(o, analyticsConfig(server.URL))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.ExportOnce(ctx) }()
	<-entered
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	if err := e.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if keys["lost-response"] != 2 || len(keys) != 1 {
		t.Fatal("replay changed the attempt identity")
	}
	if d, err := o.Delivery("lost-response"); err != nil || d.State != DeliveryAcknowledged {
		t.Fatalf("retry failed: %+v, %v", d, err)
	}
}

func TestAnalyticsRequeuesClaimsAfterRestart(t *testing.T) {
	cfg := config.AccountingOutboxConfig{Enabled: true, DataPath: t.TempDir(), Analytics: analyticsConfig("http://localhost:3000")}
	o := openTestOutbox(t, cfg)
	if err := o.Insert(exportEvent("crashed")); err != nil {
		t.Fatal(err)
	}
	if ok, err := o.Claim("crashed"); err != nil || !ok {
		t.Fatalf("claim: %v, %v", ok, err)
	}
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	recovered := openTestOutbox(t, cfg)
	ids, err := recovered.ReadyIDs(64)
	if err != nil || len(ids) != 1 || ids[0] != "crashed" {
		t.Fatalf("claim was stranded: %v, %v", ids, err)
	}
	if ok, err := recovered.Claim("crashed"); err != nil || !ok {
		t.Fatalf("reclaim: %v, %v", ok, err)
	}
	if d, err := recovered.Delivery("crashed"); err != nil || d.Attempts != 2 {
		t.Fatalf("attempt counter lost: %+v, %v", d, err)
	}
}

func TestAnalyticsBatchLimit(t *testing.T) {
	var batches []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		count := 0
		for {
			var row analyticsRow
			err := decoder.Decode(&row)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			count++
		}
		batches = append(batches, count)
		_, _ = fmt.Fprintf(w, `{"rows":%d}`, count)
	}))
	defer server.Close()
	o := openTestOutbox(t, config.AccountingOutboxConfig{})
	for i := range 65 {
		if err := o.Insert(exportEvent(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	e := NewAnalyticsExporter(o, analyticsConfig(server.URL))

	for range 3 {
		if err := e.ExportOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(batches, []int{64, 1}) {
		t.Fatalf("unexpected batches: %v", batches)
	}
}

func TestAnalyticsMissingConfiguredTokenDoesNotClaimEvents(t *testing.T) {
	t.Setenv("TEST_Analytics_TOKEN", "")
	cfg := analyticsConfig("http://localhost:3000")
	cfg.TokenEnv = "TEST_Analytics_TOKEN"
	o := openTestOutbox(t, config.AccountingOutboxConfig{})
	if err := o.Insert(exportEvent("pending")); err != nil {
		t.Fatal(err)
	}
	e := NewAnalyticsExporter(o, cfg)

	if err := e.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.Status() != "credential_unavailable" {
		t.Fatalf("unexpected status: %s", e.Status())
	}
	if d, err := o.Delivery("pending"); err != nil || d.State != DeliveryPending || d.Attempts != 0 {
		t.Fatalf("claimed without credentials: %+v, %v", d, err)
	}
}

func TestAnalyticsRefusesRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("ingestion followed a redirect")
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	o := openTestOutbox(t, config.AccountingOutboxConfig{})
	if err := o.Insert(exportEvent("redirect")); err != nil {
		t.Fatal(err)
	}
	e := NewAnalyticsExporter(o, analyticsConfig(server.URL))

	if err := e.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d, err := o.Delivery("redirect"); err != nil || d.State != DeliveryRetryable {
		t.Fatalf("redirect acknowledged: %+v, %v", d, err)
	}
}

func TestAnalyticsParentTotalsKeepKnownZeros(t *testing.T) {
	event := exportEvent("zero-parents")
	event.Tokens = &AccountingTokens{Breakdown: NewSubsetTokenBreakdown(0, 0, 0, 0, 0, 0)}

	row, err := newAnalyticsRow(event)

	if err != nil || row.InputTokens == nil || *row.InputTokens != 0 || row.OutputTokens == nil || *row.OutputTokens != 0 {
		t.Fatalf("lost known zero parent totals: %+v %v", row, err)
	}
}
