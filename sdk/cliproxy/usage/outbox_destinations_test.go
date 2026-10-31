package usage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bolt "go.etcd.io/bbolt"
)

func dualExporterConfig(t *testing.T) config.AccountingOutboxConfig {
	t.Helper()

	return config.AccountingOutboxConfig{
		Enabled: true, DataPath: t.TempDir(), RetentionHours: 1,
		LiteLLM:   exporterConfig("http://localhost:4000"),
		Analytics: analyticsConfig("http://localhost:3000"),
	}
}

func resolveQueue(t *testing.T, queue deliveryQueue, id string, state DeliveryState) {
	t.Helper()

	if claimed, err := queue.Claim(id); err != nil || !claimed {
		t.Fatalf("claim %s: %t %v", id, claimed, err)
	}
	delivery, err := queue.Delivery(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Resolve(id, delivery.Attempts, state, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestDualExportersRetryIndependently(t *testing.T) {
	t.Setenv("TEST_LITELLM_ADMIN", "test-admin")
	for _, failAt := range []string{"analytics", "litellm"} {
		t.Run(failAt, func(t *testing.T) {
			calls := map[string]int{}
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				destination := "analytics"
				if r.URL.Path == "/v1/rust_control_plane/logs" {
					destination = "litellm"
				}
				mu.Lock()
				calls[destination]++
				attempt := calls[destination]
				mu.Unlock()

				if destination == failAt && attempt == 1 {
					w.WriteHeader(503)
					return
				}
				if destination == "analytics" {
					_, _ = io.WriteString(w, `{"rows":1}`)
				} else {
					_, _ = io.WriteString(w, `{"processed":1,"failed":0,"failures":[]}`)
				}
			}))
			defer server.Close()

			cfg := dualExporterConfig(t)
			cfg.Analytics.URL, cfg.LiteLLM.URL = server.URL, server.URL
			o := openTestOutbox(t, cfg)
			now := time.Unix(1800000000, 0)
			o.now = func() time.Time { return now }
			if err := o.Insert(exportEvent("both")); err != nil {
				t.Fatal(err)
			}
			analytics := NewAnalyticsExporter(o, cfg.Analytics)
			litellm := NewLiteLLMExporter(o, cfg.LiteLLM)
			results := make(chan error, 2)
			go func() { results <- analytics.ExportOnce(context.Background()) }()
			go func() { results <- litellm.ExportOnce(context.Background()) }()
			for i := 0; i < 2; i++ {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}

			stats, err := o.Stats()
			if err != nil || stats.Backlog != 1 {
				t.Fatalf("missing failed destination backlog: %+v %v", stats, err)
			}
			now = now.Add(2 * time.Hour)
			if err := o.Prune(); err != nil {
				t.Fatal(err)
			}
			if _, err := o.Event("both"); err != nil {
				t.Fatalf("pruned event before both deliveries completed: %v", err)
			}
			if err := analytics.ExportOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := litellm.ExportOnce(context.Background()); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			for destination, count := range calls {
				expected := 1
				if destination == failAt {
					expected = 2
				}
				if count != expected {
					t.Errorf("%s delivered %d times, wanted %d", destination, count, expected)
				}
			}
			mu.Unlock()
			stats, err = o.Stats()
			if err != nil || stats.Backlog != 0 {
				t.Fatalf("backlog not cleared: %+v %v", stats, err)
			}
			now = now.Add(2 * time.Hour)
			if err := o.Prune(); err != nil {
				t.Fatal(err)
			}
			if _, err := o.Event("both"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed event not pruned: %v", err)
			}
		})
	}
}

func TestDualExporterMigrationPreservesLegacyAcknowledgements(t *testing.T) {
	cfg := dualExporterConfig(t)
	cfg.Analytics.Enabled = false
	o := openTestOutbox(t, cfg)
	for _, id := range []string{"history", "pending", "uncertain"} {
		if err := o.Insert(exportEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	resolveQueue(t, o.deliveriesFor("litellm"), "history", DeliveryAcknowledged)
	if claimed, err := o.Claim("uncertain"); err != nil || !claimed {
		t.Fatal(err)
	}
	if err := o.update(func(tx *bolt.Tx) error { return tx.DeleteBucket(destinationsBucket) }); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	cfg.Analytics.Enabled = true
	recovered := openTestOutbox(t, cfg)
	legacy, err := recovered.deliveriesFor("litellm").Delivery("history")
	if err != nil || legacy.State != DeliveryAcknowledged {
		t.Fatalf("legacy acknowledgement changed: %+v %v", legacy, err)
	}
	if _, err := recovered.deliveriesFor("analytics").Delivery("history"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("history automatically replayed: %v", err)
	}
	legacy, err = recovered.deliveriesFor("litellm").Delivery("uncertain")
	if err != nil || legacy.State != DeliveryAmbiguous {
		t.Fatalf("unsafe LiteLLM retry: %+v %v", legacy, err)
	}
	ids, err := recovered.deliveriesFor("analytics").ReadyIDs(64)
	if err != nil || len(ids) != 2 {
		t.Fatalf("missing independent pending deliveries: %v %v", ids, err)
	}
}

func TestDualExporterRestartRequeuesOnlyAnalytics(t *testing.T) {
	cfg := dualExporterConfig(t)
	o := openTestOutbox(t, cfg)
	if err := o.Insert(exportEvent("crashed")); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{"litellm", "analytics"} {
		if claimed, err := o.deliveriesFor(destination).Claim("crashed"); err != nil || !claimed {
			t.Fatal(err)
		}
	}
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	recovered := openTestOutbox(t, cfg)
	for _, destination := range []string{"litellm", "analytics"} {
		delivery, err := recovered.deliveriesFor(destination).Delivery("crashed")
		expected := DeliveryAmbiguous
		if destination == "analytics" {
			expected = DeliveryRetryable
		}
		if err != nil || delivery.State != expected || delivery.Attempts != 1 {
			t.Fatalf("%s: %+v %v", destination, delivery, err)
		}
	}
}

func TestDualExporterDestinationOwnershipSurvivesConfigChanges(t *testing.T) {
	for _, primary := range []string{"analytics", "litellm"} {
		t.Run(primary, func(t *testing.T) {
			cfg := dualExporterConfig(t)
			cfg.Analytics.Enabled = primary == "analytics"
			cfg.LiteLLM.Enabled = primary == "litellm"
			o := openTestOutbox(t, cfg)
			if err := o.Insert(exportEvent("old")); err != nil {
				t.Fatal(err)
			}
			resolveQueue(t, o.deliveriesFor(primary), "old", DeliveryAcknowledged)
			if err := o.Close(context.Background()); err != nil {
				t.Fatal(err)
			}

			cfg.Analytics.Enabled, cfg.LiteLLM.Enabled = true, true
			both := openTestOutbox(t, cfg)
			if err := both.Insert(exportEvent("new")); err != nil {
				t.Fatal(err)
			}
			other := both.secondaryDestination()
			resolveQueue(t, both.deliveriesFor(primary), "new", DeliveryAcknowledged)
			resolveQueue(t, both.deliveriesFor(other), "new", DeliveryRetryable)
			if err := both.Close(context.Background()); err != nil {
				t.Fatal(err)
			}

			cfg.Analytics.Enabled, cfg.LiteLLM.Enabled = other == "analytics", other == "litellm"
			recovered := openTestOutbox(t, cfg)
			ids, err := recovered.deliveriesFor(other).ReadyIDs(64)
			if err != nil || len(ids) != 1 || ids[0] != "new" {
				t.Fatalf("wrong destination backlog after config change: %v %v", ids, err)
			}
			if err := recovered.Insert(exportEvent("only-secondary")); err != nil {
				t.Fatal(err)
			}
			delivery, err := recovered.deliveriesFor(primary).Delivery("only-secondary")
			if err != nil || delivery.State != DeliveryAcknowledged {
				t.Fatalf("disabled destination added a new obligation: %+v %v", delivery, err)
			}
		})
	}
}

func TestDualExporterRepricingUpdatesDeliveredAnalyticsFact(t *testing.T) {
	o := openTestOutbox(t, dualExporterConfig(t))
	event := exportEvent("unpriced")
	event.Estimate = &CostEstimate{Status: "unpriced"}
	if err := o.Insert(event); err != nil {
		t.Fatal(err)
	}
	analytics := o.deliveriesFor("analytics")
	if claimed, err := analytics.Claim(event.ExecutionID); err != nil || !claimed {
		t.Fatal(err)
	}
	o.prices.Store(testPriceBook(t, config.PriceRate{
		Provider: "openai", Model: "gpt-4o", Currency: "USD",
		Input: priceString("1"), CacheRead: priceString("1"), Output: priceString("1"),
	}))

	inflight, err := o.repriceUnpricedEvent(event.ExecutionID)
	if err != nil || inflight.Estimate.Status != "unpriced" {
		t.Fatalf("changed an in-flight payload: %+v %v", inflight.Estimate, err)
	}
	delivery, err := analytics.Delivery(event.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := analytics.Resolve(event.ExecutionID, delivery.Attempts, DeliveryAcknowledged, time.Time{}); err != nil {
		t.Fatal(err)
	}
	updated, err := o.repriceUnpricedEvent(event.ExecutionID)
	if err != nil || updated.Estimate.Status != "priced" {
		t.Fatalf("failed to reprice: %+v %v", updated.Estimate, err)
	}

	ids, err := analytics.ReadyIDs(64)
	if err != nil || len(ids) != 1 || ids[0] != event.ExecutionID {
		raw, _ := json.Marshal(updated)
		t.Fatalf("updated fact not requeued: %v %v %s", ids, err, raw)
	}
}
