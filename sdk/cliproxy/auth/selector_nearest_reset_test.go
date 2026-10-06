package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func codexAuthWithResets(id string, resetsAt ...time.Time) *Auth {
	signals := make(map[string]string, len(resetsAt))
	names := []string{"X-Codex-Primary-Reset-At", "X-Codex-Secondary-Reset-At"}
	for i, reset := range resetsAt {
		signals[names[i]] = strconv.FormatInt(reset.Unix(), 10)
	}

	return &Auth{
		ID:       id,
		Provider: "codex",
		Quota:    QuotaState{ObservedAt: time.Now(), Signals: signals},
	}
}

func TestNearestResetSelectorPrefersSoonestReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{
		codexAuthWithResets("a", now.Add(5*time.Hour), now.Add(72*time.Hour)),
		codexAuthWithResets("b", now.Add(4*time.Hour), now.Add(30*time.Minute)),
		codexAuthWithResets("c", now.Add(1*time.Hour), now.Add(48*time.Hour)),
	}

	selector := &NearestResetSelector{}
	got, err := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %q, want %q", got.ID, "b")
	}
}

func TestNearestResetSelectorIgnoresElapsedResets(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{
		codexAuthWithResets("a", now.Add(-time.Hour), now.Add(10*time.Hour)),
		codexAuthWithResets("b", now.Add(2*time.Hour)),
	}

	selector := &NearestResetSelector{}
	got, err := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %q, want %q", got.ID, "b")
	}
}

func TestNearestResetSelectorPrefersCredentialsWithResetData(t *testing.T) {
	t.Parallel()

	auths := []*Auth{
		{ID: "a", Provider: "codex"},
		codexAuthWithResets("b", time.Now().Add(6*time.Hour)),
	}

	selector := &NearestResetSelector{}
	got, err := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %q, want %q", got.ID, "b")
	}
}

func TestNearestResetSelectorRotatesWithoutResetData(t *testing.T) {
	t.Parallel()

	auths := []*Auth{
		{ID: "a", Provider: "codex"},
		{ID: "b", Provider: "codex"},
	}

	selector := &NearestResetSelector{}
	var picked []string
	for range 4 {
		got, err := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		picked = append(picked, got.ID)
	}

	want := []string{"a", "b", "a", "b"}
	for i := range want {
		if picked[i] != want[i] {
			t.Fatalf("picks = %v, want %v", picked, want)
		}
	}
}

func TestQuotaSignalResetTime(t *testing.T) {
	t.Parallel()

	observedAt := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name  string
		key   string
		value string
		want  time.Time
		ok    bool
	}{
		{name: "claude unix reset", key: "Anthropic-Ratelimit-Unified-5h-Reset", value: "1700003600", want: time.Unix(1_700_003_600, 0), ok: true},
		{name: "codex reset at", key: "X-Codex-Primary-Reset-At", value: "1700007200", want: time.Unix(1_700_007_200, 0), ok: true},
		{name: "codex reset after", key: "X-Codex-Secondary-Reset-After-Seconds", value: "600", want: observedAt.Add(10 * time.Minute), ok: true},
		{name: "devin rfc3339", key: "weekly_quota_reset_at", value: "2023-11-14T23:13:20Z", want: time.Date(2023, 11, 14, 23, 13, 20, 0, time.UTC), ok: true},
		{name: "unrelated signal", key: "X-Codex-Primary-Used-Percent", value: "90"},
		{name: "garbage value", key: "X-Codex-Primary-Reset-At", value: "soon"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := quotaSignalResetTime(tt.key, tt.value, observedAt)
			if ok != tt.ok || !got.Equal(tt.want) {
				t.Fatalf("quotaSignalResetTime() = (%v, %v), want (%v, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}
