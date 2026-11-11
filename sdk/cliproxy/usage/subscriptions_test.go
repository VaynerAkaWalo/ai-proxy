package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bolt "go.etcd.io/bbolt"
)

func TestSubscriptionsRetireRemovedAccountsAfterDeliveryAndRestart(t *testing.T) {
	cfg := config.AccountingOutboxConfig{DataPath: t.TempDir()}
	outbox, err := OpenOutbox(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outbox.Close(context.Background()) })

	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	price, plan := "30", "SuperGrok"
	account := QuotaSnapshot{Source: QuotaSnapshotSource, Provider: "xai", AccountID: "old-grok", Window: "subscription", Utilization: "0", Plan: &plan, PlanPriceUSD: &price}
	if err := outbox.ReconcileSubscriptions([]QuotaSnapshot{account}, at); err != nil {
		t.Fatal(err)
	}
	keys, rows, err := outbox.pendingQuotaSnapshots(10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("initial inventory: rows=%v err=%v", rows, err)
	}
	if err := outbox.ackQuotaSnapshots(keys, rows); err != nil {
		t.Fatal(err)
	}
	if err := outbox.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	outbox, err = OpenOutbox(cfg)
	if err != nil {
		t.Fatal(err)
	}
	account.AccountID = "new-grok"
	if err := outbox.ReconcileSubscriptions([]QuotaSnapshot{account}, at); err != nil {
		t.Fatal(err)
	}
	keys, rows, err = outbox.pendingQuotaSnapshots(10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("replacement inventory: rows=%v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.AccountID == "old-grok" && (*row.PlanPriceUSD != "0" || !row.ObservedAt.After(at)) {
			t.Fatalf("retired account must have a later zero price: %+v", row)
		}
		if row.AccountID == "new-grok" && *row.PlanPriceUSD != "30" {
			t.Fatalf("replacement price: %+v", row)
		}
	}
	if err := outbox.ackQuotaSnapshots(keys, rows); err != nil {
		t.Fatal(err)
	}

	if err := outbox.ReconcileSubscriptions(nil, at); err != nil {
		t.Fatal(err)
	}
	_, rows, err = outbox.pendingQuotaSnapshots(10)
	if err != nil || len(rows) != 1 || rows[0].AccountID != "new-grok" || *rows[0].PlanPriceUSD != "0" {
		t.Fatalf("empty inventory must retire the final account once: rows=%v err=%v", rows, err)
	}
	keys, rows, err = outbox.pendingQuotaSnapshots(10)
	if err != nil {
		t.Fatal(err)
	}
	retiredAt := rows[0].ObservedAt
	if err := outbox.ackQuotaSnapshots(keys, rows); err != nil {
		t.Fatal(err)
	}
	if err := outbox.ReconcileSubscriptions(nil, at); err != nil {
		t.Fatal(err)
	}
	_, rows, err = outbox.pendingQuotaSnapshots(10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("retirement must not repeat: rows=%v err=%v", rows, err)
	}

	account.Plan, account.PlanPriceUSD = nil, nil
	if err := outbox.ReconcileSubscriptions([]QuotaSnapshot{account}, at); err != nil {
		t.Fatal(err)
	}
	_, rows, err = outbox.pendingQuotaSnapshots(10)
	if err != nil || len(rows) != 1 || *rows[0].PlanPriceUSD != "30" || !rows[0].ObservedAt.After(retiredAt) {
		t.Fatalf("re-added account must supersede retirement: rows=%v err=%v", rows, err)
	}
}

func TestSubscriptionsRetryRetirementWhenTheOutboxWasFull(t *testing.T) {
	outbox := openTestOutbox(t, config.AccountingOutboxConfig{})
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	price, plan := "30", "SuperGrok"
	account := QuotaSnapshot{Provider: "xai", AccountID: "grok", Plan: &plan, PlanPriceUSD: &price}
	if err := outbox.ReconcileSubscriptions([]QuotaSnapshot{account}, at); err != nil {
		t.Fatal(err)
	}
	keys, rows, err := outbox.pendingQuotaSnapshots(10)
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.ackQuotaSnapshots(keys, rows); err != nil {
		t.Fatal(err)
	}
	if err := outbox.update(func(tx *bolt.Tx) error { return tx.Bucket(quotaSnapshotsBucket).SetSequence(MaxQuotaSnapshots) }); err != nil {
		t.Fatal(err)
	}

	if err := outbox.ReconcileSubscriptions(nil, at); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("expected full outbox, got %v", err)
	}
	if err := outbox.update(func(tx *bolt.Tx) error { return tx.Bucket(quotaSnapshotsBucket).SetSequence(0) }); err != nil {
		t.Fatal(err)
	}
	if err := outbox.ReconcileSubscriptions(nil, at); err != nil {
		t.Fatal(err)
	}

	_, rows, err = outbox.pendingQuotaSnapshots(10)
	if err != nil || len(rows) != 1 || *rows[0].PlanPriceUSD != "0" {
		t.Fatalf("retirement was lost after retry: rows=%v err=%v", rows, err)
	}
}
