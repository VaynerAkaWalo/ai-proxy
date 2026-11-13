package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type modelPricesServer struct {
	*httptest.Server
	prices   []map[string]any
	attempts []map[string]any
	failing  bool
}

func newModelPricesServer(t *testing.T) *modelPricesServer {
	t.Helper()

	s := &modelPricesServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := &s.attempts
		if r.URL.Path == "/api/datasets/pawel/llm_model_prices/rows" {
			target = &s.prices
			if s.failing {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}

		decoder := json.NewDecoder(r.Body)
		rows := 0
		for {
			var row map[string]any
			if err := decoder.Decode(&row); err == io.EOF {
				break
			} else if err != nil {
				t.Error(err)
				return
			}
			*target = append(*target, row)
			rows++
		}
		_, _ = fmt.Fprintf(w, `{"rows":%d}`, rows)
	}))
	t.Cleanup(s.Close)

	return s
}

func modelPricesConfig(url string) config.AnalyticsExporterConfig {
	cfg := analyticsConfig(url)
	cfg.ModelPrices.Enabled = true

	return cfg
}

func openModelPricesOutbox(t *testing.T, now *time.Time, rates ...config.PriceRate) *Outbox {
	t.Helper()

	o := openTestOutbox(t, config.AccountingOutboxConfig{})
	o.now = func() time.Time { return *now }
	o.prices.Store(testPriceBook(t, rates...))

	return o
}

func gpt5Rate(input, output string) config.PriceRate {
	return config.PriceRate{
		Provider: "openai", Model: "gpt-5", Currency: "USD",
		Input: priceString(input), Output: priceString(output),
	}
}

func eventOfSize(id, provider, model string, input int64) AccountingEvent {
	event := exportEvent(id)
	event.Provider, event.ExecutedModel = provider, model
	event.Tokens = &AccountingTokens{Breakdown: NewSubsetTokenBreakdown(input, 0, 0, 10, 0, input+10)}

	return event
}

func TestPriceKeyDoesNotChangeWhenThePriceIsAddedLater(t *testing.T) {
	event := eventOfSize("one", "codex", "gpt-5", 100)

	before, found := (*PriceBook)(nil).Key(event)
	if found || before != (PriceKey{Provider: "openai", Model: "gpt-5"}) {
		t.Fatalf("unpriced key: %+v %t", before, found)
	}

	after, found := testPriceBook(t, gpt5Rate("0.000001", "0.00001")).Key(event)
	if !found || after != before {
		t.Fatalf("key moved when the price appeared: %+v %t", after, found)
	}
}

func TestUnpricedKeyUsesTheExecutedModelNotTheDatedResponseModel(t *testing.T) {
	event := eventOfSize("one", "codex", "gpt-5", 100)
	event.ResponseModel = "gpt-5-2025-08-07"

	before, found := (*PriceBook)(nil).Key(event)
	if found || before != (PriceKey{Provider: "openai", Model: "gpt-5"}) {
		t.Fatalf("unpriced key: %+v %t", before, found)
	}

	after, found := testPriceBook(t, gpt5Rate("0.000001", "0.00001")).Key(event)
	if !found || after != before {
		t.Fatalf("key moved when the price appeared: %+v %t", after, found)
	}
}

func TestPriceKeyPicksTheContextBandAndTier(t *testing.T) {
	low, high := gpt5Rate("0.000001", "0.00001"), gpt5Rate("0.000002", "0.00002")
	low.MaxContext, high.MinContext = 200000, 200001
	priority := gpt5Rate("0.000003", "0.00003")
	priority.Tier = "priority"
	book := testPriceBook(t, low, high, priority)

	key, found := book.Key(eventOfSize("one", "openai", "gpt-5", 300000))
	if !found || key.MinContext != 200001 {
		t.Fatalf("large context: %+v %t", key, found)
	}

	event := eventOfSize("two", "openai", "gpt-5", 100)
	event.ReportedServiceTier = "priority"
	key, found = book.Key(event)
	if !found || key != (PriceKey{Provider: "openai", Model: "gpt-5", Tier: "priority"}) {
		t.Fatalf("priority tier: %+v %t", key, found)
	}
}

func TestAttemptsCarryThePriceKeyEvenWithoutAPrice(t *testing.T) {
	server := newModelPricesServer(t)
	now := time.Unix(1800000000, 0)
	o := openModelPricesOutbox(t, &now)
	if err := o.Insert(eventOfSize("one", "codex", "gpt-5", 100)); err != nil {
		t.Fatal(err)
	}

	if err := NewAnalyticsExporter(o, modelPricesConfig(server.URL)).ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	row := server.attempts[0]
	if row["price_provider"] != "openai" || row["price_model"] != "gpt-5" || row["price_service_tier"] != "" || row["price_min_context_tokens"] != float64(0) {
		t.Fatalf("incorrect price key: %+v", row)
	}
}

func TestAttemptsOmitThePriceKeyUntilModelPricesAreEnabled(t *testing.T) {
	server := newModelPricesServer(t)
	now := time.Unix(1800000000, 0)
	o := openModelPricesOutbox(t, &now, gpt5Rate("0.000001", "0.00001"))
	if err := o.Insert(eventOfSize("one", "openai", "gpt-5", 100)); err != nil {
		t.Fatal(err)
	}

	if err := NewAnalyticsExporter(o, analyticsConfig(server.URL)).ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	for name := range server.attempts[0] {
		if strings.HasPrefix(name, "price_") {
			t.Fatalf("exported %s before the table has it", name)
		}
	}
	if len(server.prices) != 0 {
		t.Fatal("published prices while disabled")
	}
}

func TestModelPricesAreVersionedByWhenTheyChange(t *testing.T) {
	server := newModelPricesServer(t)
	now := time.Unix(1800000000, 0)
	o := openModelPricesOutbox(t, &now, gpt5Rate("0.00000125", "0.00001"))
	exporter := NewAnalyticsExporter(o, modelPricesConfig(server.URL))

	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(server.prices) != 1 {
		t.Fatalf("first publish: %+v", server.prices)
	}
	first := server.prices[0]
	if first["effective_at"] != "1970-01-01T00:00:00Z" || first["input_usd_per_mtok"] != "1.25" || first["output_usd_per_mtok"] != "10" || first["service_tier"] != "" || first["min_context_tokens"] != float64(0) || first["cache_write_usd_per_mtok"] != nil {
		t.Fatalf("incorrect first price: %+v", first)
	}

	now = now.Add(time.Hour)
	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(server.prices) != 1 {
		t.Fatalf("republished an unchanged price: %+v", server.prices)
	}

	now = now.Add(time.Hour)
	o.prices.Store(testPriceBook(t, gpt5Rate("0.000001", "0.000008")))
	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(server.prices) != 2 {
		t.Fatalf("changed price not published: %+v", server.prices)
	}
	changed := server.prices[1]
	if changed["effective_at"] != now.UTC().Format(time.RFC3339) || changed["input_usd_per_mtok"] != "1" || changed["output_usd_per_mtok"] != "8" {
		t.Fatalf("incorrect changed price: %+v", changed)
	}
}

func TestFailedPublishRetriesWithTheSameEffectiveTime(t *testing.T) {
	server := newModelPricesServer(t)
	now := time.Unix(1800000000, 0)
	o := openModelPricesOutbox(t, &now, gpt5Rate("0.00000125", "0.00001"))
	exporter := NewAnalyticsExporter(o, modelPricesConfig(server.URL))
	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Hour)
	o.prices.Store(testPriceBook(t, gpt5Rate("0.000001", "0.000008")))
	server.failing = true
	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(server.prices) != 1 {
		t.Fatalf("a failed publish stored a row: %+v", server.prices)
	}

	now = now.Add(6 * time.Hour)
	server.failing = false
	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(server.prices) != 2 || server.prices[1]["effective_at"] != now.Add(-6*time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("retry changed the effective time: %+v", server.prices)
	}
}

func TestModelPricesSkipRatesThatCannotBeExactlyRepresented(t *testing.T) {
	server := newModelPricesServer(t)
	now := time.Unix(1800000000, 0)
	unrepresentable := gpt5Rate("0.0000000000000000001", "0.00001")
	unrepresentable.Model = "odd"
	o := openModelPricesOutbox(t, &now, unrepresentable, gpt5Rate("0.000001", "0.00001"))

	if err := NewAnalyticsExporter(o, modelPricesConfig(server.URL)).ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(server.prices) != 1 || server.prices[0]["model"] != "gpt-5" {
		t.Fatalf("incorrect published prices: %+v", server.prices)
	}
}
