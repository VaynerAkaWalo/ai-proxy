package usage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestAnalyticsQuotaDeliveryWithIngestionToken(t *testing.T) {
	cases := []struct {
		name         string
		rowStatus    int
		registration int
		body         string
		requests     []string
		remaining    int
		status       string
	}{
		{"existing", 200, 403, `{"rows":1}`, []string{"rows"}, 0, "exported"},
		{"create-missing", 404, 201, `{"rows":1}`, []string{"rows", "register", "rows"}, 0, "exported"},
		{"creation-denied", 404, 403, ``, []string{"rows", "register"}, 1, "authorization_failure"},
		{"unauthorized", 401, 403, ``, []string{"rows"}, 1, "authorization_failure"},
		{"wrong-count", 200, 403, `{"rows":0}`, []string{"rows"}, 1, "response_invalid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			created := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing ingestion token")
				}

				if r.URL.Path == "/api/datasets" {
					requests = append(requests, "register")
					var dataset struct {
						ID         string `json:"id"`
						Implements string `json:"implements"`
					}
					if err := json.NewDecoder(r.Body).Decode(&dataset); err != nil || dataset.ID != "pawel/llm_quota_snapshots" || dataset.Implements != "llm_usage_quota_snapshots" {
						t.Errorf("unexpected registration: %+v %v", dataset, err)
					}

					created = tc.registration == http.StatusCreated
					w.WriteHeader(tc.registration)
					return
				}

				requests = append(requests, "rows")
				if r.URL.Path != "/api/datasets/pawel/llm_quota_snapshots/rows" {
					t.Errorf("unexpected ingestion path: %s", r.URL.Path)
				}

				status := tc.rowStatus
				if created {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			t.Setenv("TEST_ANALYTICS_TOKEN", "test-token")
			cfg := analyticsConfig(server.URL)
			cfg.TokenEnv = "TEST_ANALYTICS_TOKEN"
			cfg.QuotaSnapshots.Enabled = true
			outbox := openTestOutbox(t, config.AccountingOutboxConfig{})
			if err := outbox.EnqueueQuotaSnapshots([]QuotaSnapshot{{
				Source: QuotaSnapshotSource, ObservedAt: time.Unix(1800000000, 0),
				Provider: "claude", AccountID: "account", Window: "5h", Utilization: "20",
			}}); err != nil {
				t.Fatal(err)
			}
			exporter := NewAnalyticsExporter(outbox, cfg)

			if err := exporter.ExportOnce(context.Background()); err != nil {
				t.Fatal(err)
			}

			_, pending, err := outbox.pendingQuotaSnapshots(10)
			if err != nil || len(pending) != tc.remaining || exporter.Status() != tc.status {
				t.Fatalf("unexpected delivery: pending=%d status=%s err=%v", len(pending), exporter.Status(), err)
			}
			if !reflect.DeepEqual(requests, tc.requests) {
				t.Fatalf("unexpected requests: %v", requests)
			}
		})
	}
}

func TestSubscriptionCostsKeepOneRowPerAccountAndMonthWithTheLatestPlan(t *testing.T) {
	price := func(value string) *string { return &value }
	at := func(day string) time.Time {
		parsed, err := time.Parse(time.RFC3339, day+"T12:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	snapshot := func(day, account string, plan, usd *string) QuotaSnapshot {
		return QuotaSnapshot{Source: QuotaSnapshotSource, ObservedAt: at(day), Provider: "xai", AccountID: account, Plan: plan, PlanPriceUSD: usd, Window: "weekly", Utilization: "0.2"}
	}

	costs := subscriptionCosts([]QuotaSnapshot{
		snapshot("2026-11-30", "a", price("SuperGrok"), price("30")),
		snapshot("2026-12-01", "a", price("SuperGrok"), price("30")),
		snapshot("2026-12-02", "a", price("SuperGrok Heavy"), price("300")),
		snapshot("2026-12-02", "b", nil, nil),
	})

	december := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	november := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	want := []SubscriptionCost{
		{Source: QuotaSnapshotSource, Provider: "xai", AccountID: "a", Plan: price("SuperGrok"), Month: november, USD: "30"},
		{Source: QuotaSnapshotSource, Provider: "xai", AccountID: "a", Plan: price("SuperGrok Heavy"), Month: december, USD: "300"},
	}
	if !reflect.DeepEqual(costs, want) {
		t.Fatalf("unexpected costs: %+v", costs)
	}
}

func TestQuotaSnapshotsAreDeliveredEvenWhenTheirSubscriptionCostsAreRefused(t *testing.T) {
	var costs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/datasets/pawel/llm_quota_snapshots/rows":
			_, _ = io.WriteString(w, `{"rows":1}`)
		case "/api/datasets/pawel/llm_subscription_costs/rows":
			raw, _ := io.ReadAll(r.Body)
			costs = append(costs, string(raw))
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()

	cfg := analyticsConfig(server.URL)
	cfg.QuotaSnapshots.Enabled = true
	outbox := openTestOutbox(t, config.AccountingOutboxConfig{})
	plan, usd := "Pro", "20"
	if err := outbox.EnqueueQuotaSnapshots([]QuotaSnapshot{{
		Source: QuotaSnapshotSource, ObservedAt: time.Date(2026, 12, 3, 8, 0, 0, 0, time.UTC),
		Provider: "claude", AccountID: "account", Plan: &plan, PlanPriceUSD: &usd, Window: "weekly", Utilization: "0.4",
	}}); err != nil {
		t.Fatal(err)
	}
	exporter := NewAnalyticsExporter(outbox, cfg)

	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, pending, err := outbox.pendingQuotaSnapshots(10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("quota snapshot still pending: %d %v", len(pending), err)
	}
	want := `{"source":"ai_proxy","provider":"claude","account_id":"account","plan":"Pro","month":"2026-12-01T00:00:00Z","usd":"20"}` + "\n"
	if !reflect.DeepEqual(costs, []string{want}) {
		t.Fatalf("unexpected subscription costs: %q", costs)
	}
	if exporter.Status() != "authorization_failure" {
		t.Fatalf("refused subscription costs should show in the status, got %s", exporter.Status())
	}
}

func TestRetiringAnAccountKeepsWhatItCostThatMonth(t *testing.T) {
	plan, price, zero := "Pro", "20", "0"
	retired := func(day int, usd *string) QuotaSnapshot {
		return QuotaSnapshot{Source: QuotaSnapshotSource, ObservedAt: time.Date(2026, 12, day, 9, 0, 0, 0, time.UTC), Provider: "claude", AccountID: "a", Plan: &plan, PlanPriceUSD: usd, Window: SubscriptionWindow, Utilization: "0"}
	}

	costs := subscriptionCosts([]QuotaSnapshot{retired(3, &price), retired(10, &zero)})

	if len(costs) != 1 || costs[0].USD != "20" {
		t.Fatalf("a retired account should keep its paid month, got %+v", costs)
	}
}
