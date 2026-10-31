package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func archivedPriceEvent(id string) AccountingEvent {
	event := exportEvent(id)
	event.Estimate.Rounding = "half_up_9_decimal_places"
	event.Estimate.Source = "litellm"
	event.Estimate.Rates = &config.PriceRate{
		Provider: "openai", Model: "m", Currency: "USD", MinContext: 500001,
		Input: priceString("0.000002500000000001"), Output: priceString("0.000015"),
	}
	return event
}

func TestAnalyticsPriceIdentityAndExactRates(t *testing.T) {
	event := archivedPriceEvent("one")
	row, err := newAnalyticsPriceRow(event.Estimate)
	if err != nil {
		t.Fatal(err)
	}
	if row.InputUSDPerMTok == nil || *row.InputUSDPerMTok != "2.500000000001" || row.ReasoningUSDPerMTok == nil || *row.ReasoningUSDPerMTok != "15" || row.MaxContextTokens != nil || row.ServiceTier != nil || len(row.PriceID) != 64 {
		t.Fatalf("incorrect price: %+v", row)
	}

	event.Estimate.Snapshot = "another-catalog"
	event.Estimate.Rates.Output = priceString("0.000015000")
	same, err := newAnalyticsPriceRow(event.Estimate)
	if err != nil || !reflect.DeepEqual(row, same) {
		t.Fatalf("equivalent rate changed identity: %+v %v", same, err)
	}
	event.Estimate.Rates.MinContext++
	changed, err := newAnalyticsPriceRow(event.Estimate)
	if err != nil || changed.PriceID == row.PriceID {
		t.Fatal("context band did not change identity")
	}
	event.Estimate.Rates.Input = priceString("0.0000025000000000001")
	if _, err := newAnalyticsPriceRow(event.Estimate); err == nil {
		t.Fatal("silently rounded an unrepresentable rate")
	}
}

func TestAnalyticsArchivesPricesBeforeUsageAndRetriesBoth(t *testing.T) {
	for _, failAt := range []string{"prices", "usage"} {
		t.Run(failAt, func(t *testing.T) {
			var calls []string
			var priceIDs []string
			fail := true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dataset := "usage"
				if r.URL.Path == "/api/datasets/pawel/prices/rows" {
					dataset = "prices"
				}
				calls = append(calls, dataset)
				decoder := json.NewDecoder(r.Body)
				rows := 0
				for {
					var row map[string]any
					err := decoder.Decode(&row)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Error(err)
						return
					}
					priceID, ok := row["price_id"].(string)
					if !ok || priceID == "" {
						t.Error("missing price reference")
					}
					priceIDs = append(priceIDs, priceID)
					rows++
				}
				if dataset == "prices" && rows != 1 {
					t.Errorf("prices not deduplicated: %d", rows)
				}
				if dataset == failAt && fail {
					fail = false
					w.WriteHeader(503)
					return
				}
				_, _ = fmt.Fprintf(w, `{"rows":%d}`, rows)
			}))
			defer server.Close()

			o := openTestOutbox(t, config.AccountingOutboxConfig{})
			now := time.Unix(1800000000, 0)
			o.now = func() time.Time { return now }
			for _, id := range []string{"one", "two"} {
				if err := o.Insert(archivedPriceEvent(id)); err != nil {
					t.Fatal(err)
				}
			}
			cfg := analyticsConfig(server.URL)
			cfg.PricesDataset = "pawel/prices"
			exporter := NewAnalyticsExporter(o, cfg)

			if err := exporter.ExportOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"one", "two"} {
				delivery, err := o.Delivery(id)
				if err != nil || delivery.State != DeliveryRetryable {
					t.Fatalf("failed archive/export was acknowledged: %+v %v", delivery, err)
				}
			}
			now = now.Add(time.Hour)
			if err := exporter.ExportOnce(context.Background()); err != nil {
				t.Fatal(err)
			}

			expected := []string{"prices", "prices", "usage"}
			if failAt == "usage" {
				expected = []string{"prices", "usage", "prices", "usage"}
			}
			if !reflect.DeepEqual(calls, expected) {
				t.Fatalf("incorrect write order: %v", calls)
			}
			for _, id := range priceIDs {
				if id != priceIDs[0] {
					t.Fatal("unstable price identity across replay or usage")
				}
			}
			for _, id := range []string{"one", "two"} {
				delivery, err := o.Delivery(id)
				if err != nil || delivery.State != DeliveryAcknowledged {
					t.Fatalf("not acknowledged after retry: %+v %v", delivery, err)
				}
			}
		})
	}
}

func TestAnalyticsInvalidPriceRemainsRetryable(t *testing.T) {
	o := openTestOutbox(t, config.AccountingOutboxConfig{})
	event := archivedPriceEvent("one")
	event.Estimate.Rates.Input = priceString("0.0000000000000000001")
	if err := o.Insert(event); err != nil {
		t.Fatal(err)
	}
	cfg := analyticsConfig("http://unused.invalid")
	cfg.PricesDataset = "pawel/prices"
	exporter := NewAnalyticsExporter(o, cfg)

	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	delivery, err := o.Delivery("one")
	if err != nil || delivery.State != DeliveryRetryable || exporter.Status() != "invalid_price_record" {
		t.Fatalf("invalid price lost: %+v %v", delivery, err)
	}
}

func TestAnalyticsUnknownPriceExportsUsageWithoutReference(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/datasets/pawel/llm_usage/rows" {
			t.Error("attempted to archive an unknown price")
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{"rows":1}`)
	}))
	defer server.Close()
	o := openTestOutbox(t, config.AccountingOutboxConfig{})
	if err := o.Insert(exportEvent("unknown")); err != nil {
		t.Fatal(err)
	}
	cfg := analyticsConfig(server.URL)
	cfg.PricesDataset = "pawel/prices"

	if err := NewAnalyticsExporter(o, cfg).ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, exists := payload["price_id"]; exists {
		t.Fatal("invented an unknown price")
	}
}
