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
						ID string `json:"id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&dataset); err != nil || dataset.ID != "pawel/llm_quota_snapshots" {
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
