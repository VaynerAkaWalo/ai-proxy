package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bolt "go.etcd.io/bbolt"
)

func exportEvent(id string) AccountingEvent {
	amount := "0.000123456"
	return AccountingEvent{SchemaVersion: 1, ExecutionID: id, StartedAt: time.Unix(1700000000, 0), CompletedAt: time.Unix(1700000001, 0), ExecutedModel: "gpt-4o", Provider: "openai", ClientKeyID: "local-client", Status: "succeeded", Tokens: &AccountingTokens{Breakdown: NewSubsetTokenBreakdown(10, 2, 0, 5, 1, 15), Evidence: map[string]int64{"usage.prompt_tokens": 10}}, Estimate: &CostEstimate{Status: "priced", Kind: "api_equivalent_estimate", Currency: "USD", Amount: &amount}}
}

func legacyOutboxConfig(t *testing.T) config.AccountingOutboxConfig {
	t.Helper()
	return config.AccountingOutboxConfig{
		Enabled: true, DataPath: t.TempDir(), RetentionHours: 1,
		Analytics: analyticsConfig("http://localhost:3000"),
	}
}

func reopenAsLegacyLiteLLMPrimary(t *testing.T, cfg config.AccountingOutboxConfig) *Outbox {
	t.Helper()
	cfg.Analytics.Enabled = false
	o := openTestOutbox(t, cfg)
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(cfg.DataPath, "accounting.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(destinationsBucket).Put(primaryDestinationKey, []byte("litellm"))
	})
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	cfg.Analytics.Enabled = true
	return openTestOutbox(t, cfg)
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

func TestLegacyLiteLLMPrimaryKeepsAnalyticsIndependent(t *testing.T) {
	cfg := legacyOutboxConfig(t)
	cfg.Analytics.Enabled = false
	o := openTestOutbox(t, cfg)
	for _, id := range []string{"history", "pending", "uncertain"} {
		if err := o.Insert(exportEvent(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(cfg.DataPath, "accounting.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(destinationsBucket).Put(primaryDestinationKey, []byte("litellm"))
	})
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}

	cfg.Analytics.Enabled = false
	legacy := openTestOutbox(t, cfg)
	resolveQueue(t, legacy.deliveriesFor("litellm"), "history", DeliveryAcknowledged)
	if claimed, err := legacy.Claim("uncertain"); err != nil || !claimed {
		t.Fatal(err)
	}
	if err := legacy.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	cfg.Analytics.Enabled = true
	recovered := openTestOutbox(t, cfg)
	acked, err := recovered.deliveriesFor("litellm").Delivery("history")
	if err != nil || acked.State != DeliveryAcknowledged {
		t.Fatalf("legacy acknowledgement changed: %+v %v", acked, err)
	}
	if _, err := recovered.deliveriesFor("analytics").Delivery("history"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("history automatically replayed: %v", err)
	}
	uncertain, err := recovered.deliveriesFor("litellm").Delivery("uncertain")
	if err != nil || uncertain.State != DeliveryAcknowledged {
		t.Fatalf("retired destination still blocks retention: %+v %v", uncertain, err)
	}
	ids, err := recovered.deliveriesFor("analytics").ReadyIDs(64)
	if err != nil || len(ids) != 2 {
		t.Fatalf("missing independent pending deliveries: %v %v", ids, err)
	}
}

func TestLegacyLiteLLMRestartRequeuesOnlyAnalytics(t *testing.T) {
	cfg := legacyOutboxConfig(t)
	o := reopenAsLegacyLiteLLMPrimary(t, cfg)
	if err := o.Insert(exportEvent("crashed")); err != nil {
		t.Fatal(err)
	}
	if claimed, err := o.deliveriesFor("analytics").Claim("crashed"); err != nil || !claimed {
		t.Fatal(err)
	}
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	recovered := openTestOutbox(t, cfg)
	retired, err := recovered.deliveriesFor("litellm").Delivery("crashed")
	if err != nil || retired.State != DeliveryAcknowledged {
		t.Fatalf("retired destination: %+v %v", retired, err)
	}
	analytics, err := recovered.deliveriesFor("analytics").Delivery("crashed")
	if err != nil || analytics.State != DeliveryRetryable || analytics.Attempts != 1 {
		t.Fatalf("analytics: %+v %v", analytics, err)
	}
}

func TestDisabledLegacyDestinationDoesNotAddNewObligations(t *testing.T) {
	cfg := legacyOutboxConfig(t)
	o := reopenAsLegacyLiteLLMPrimary(t, cfg)
	if err := o.Insert(exportEvent("new")); err != nil {
		t.Fatal(err)
	}
	primary, err := o.deliveriesFor("litellm").Delivery("new")
	if err != nil || primary.State != DeliveryAcknowledged {
		t.Fatalf("retired destination stayed pending: %+v %v", primary, err)
	}
	ids, err := o.deliveriesFor("analytics").ReadyIDs(64)
	if err != nil || len(ids) != 1 || ids[0] != "new" {
		t.Fatalf("analytics backlog: %v %v", ids, err)
	}
}

func TestRepricingUpdatesDeliveredAnalyticsFact(t *testing.T) {
	o := openTestOutbox(t, legacyOutboxConfig(t))
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

func TestAnalyticsExportRepricesADeliveredUnpricedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"rows":1}`)
	}))
	t.Cleanup(server.Close)

	cfg := legacyOutboxConfig(t)
	cfg.Analytics.URL = server.URL
	o := openTestOutbox(t, cfg)
	for i := range 64 {
		failed := exportEvent(fmt.Sprintf("fail-%02d", i))
		failed.Estimate = &CostEstimate{Status: "unpriced", Missing: []string{"usage"}}
		if err := o.Insert(failed); err != nil {
			t.Fatal(err)
		}
	}
	event := exportEvent("later")
	event.Estimate = &CostEstimate{Status: "unpriced", Missing: []string{"model_tier_context_price"}}
	if err := o.Insert(event); err != nil {
		t.Fatal(err)
	}
	exporter := NewAnalyticsExporter(o, cfg.Analytics)
	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.prices.Store(testPriceBook(t, config.PriceRate{
		Provider: "openai", Model: "gpt-4o", Currency: "USD",
		Input: priceString("1"), CacheRead: priceString("1"), Output: priceString("1"),
	}))
	exporter.lastReprice = time.Time{}
	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := o.Event("later")
	if err != nil || stored.Estimate == nil || stored.Estimate.Status != "priced" {
		t.Fatalf("delivered event stayed unpriced: %+v %v", stored.Estimate, err)
	}
	delivery, err := o.deliveriesFor("analytics").Delivery("later")
	if err != nil || delivery.State != DeliveryAcknowledged || delivery.Attempts != 1 {
		t.Fatalf("repriced fact was not sent: %+v %v", delivery, err)
	}
}
