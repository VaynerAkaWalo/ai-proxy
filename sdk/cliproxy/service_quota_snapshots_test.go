package cliproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestQuotaResetsShiftingByLessThanAMinuteAreTheSameReset(t *testing.T) {
	reset := time.Unix(1700000000, 0)

	if !quotaResetsClose(reset, reset.Add(59*time.Second)) {
		t.Fatal("drift from reset-after-seconds headers was treated as a new reset")
	}
	if quotaResetsClose(reset, reset.Add(time.Minute)) {
		t.Fatal("a reset a minute later was treated as the same reset")
	}
}

func TestServiceAccountingKeepsTheFirstResetOfAQuotaWindowSoAnalyticsSeesOneWindow(t *testing.T) {
	outbox, err := usage.OpenOutbox(config.AccountingOutboxConfig{DataPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outbox.Close(context.Background()) })

	sampler := newQuotaSnapshotSampler(outbox, nil, config.QuotaSnapshotsConfig{Enabled: true}, func() *config.Config { return &config.Config{} })
	observed := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	for _, step := range []struct {
		after   time.Duration
		percent int
		drift   time.Duration
	}{{0, 10, 0}, {10 * time.Minute, 20, 2 * time.Second}, {20 * time.Minute, 30, -3 * time.Second}, {30 * time.Minute, 5, week}} {
		at := observed.Add(step.after)
		resetIn := week - step.after + step.drift
		sampler.observe(&coreauth.Auth{ID: "codex-account", Provider: "codex", Quota: coreauth.QuotaState{ObservedAt: at, Signals: map[string]string{
			"x-codex-secondary-used-percent":        strconv.Itoa(step.percent),
			"x-codex-secondary-window-minutes":      strconv.Itoa(7 * 24 * 60),
			"x-codex-secondary-reset-after-seconds": strconv.Itoa(int(resetIn.Seconds())),
		}}})
	}

	var resets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "llm_quota_snapshots/rows") {
			_, _ = fmt.Fprint(w, `{"rows":0}`)
			return
		}

		lines := bufio.NewScanner(r.Body)
		for lines.Scan() {
			var row struct {
				ResetsAt string `json:"resets_at"`
			}
			if err := json.Unmarshal(lines.Bytes(), &row); err != nil {
				t.Error(err)
			}
			resets = append(resets, row.ResetsAt)
		}
		_, _ = fmt.Fprintf(w, `{"rows":%d}`, len(resets))
	}))
	defer server.Close()

	exporter := usage.NewAnalyticsExporter(outbox, config.AnalyticsExporterConfig{Enabled: true, URL: server.URL, Dataset: "pawel/llm_usage", QuotaSnapshots: config.QuotaSnapshotsConfig{Enabled: true}})
	if err := exporter.ExportOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	first := observed.Add(week).Format(time.RFC3339)
	next := observed.Add(2 * week).Format(time.RFC3339)
	if fmt.Sprint(resets) != fmt.Sprint([]string{first, first, first, next}) {
		t.Fatalf("unexpected resets: %v", resets)
	}
}
