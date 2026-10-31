package auth

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Quota window names understood by the analytics quota snapshot dataset.
const (
	QuotaWindow5h     = "5h"
	QuotaWindowWeekly = "weekly"
)

// QuotaObserver is notified with a credential snapshot after a response updated
// its passive quota observation.
type QuotaObserver func(*Auth)

// SetQuotaObserver registers the callback invoked after a response's quota
// headers update a credential. Passing nil removes it. The callback runs on the
// result-reporting goroutine and must not block.
func (m *Manager) SetQuotaObserver(observer QuotaObserver) {
	if m == nil {
		return
	}
	if observer == nil {
		m.quotaObserver.Store(nil)
		return
	}
	m.quotaObserver.Store(&observer)
}

func (m *Manager) notifyQuotaObserved(auth *Auth) {
	if observer := m.quotaObserver.Load(); observer != nil && *observer != nil {
		(*observer)(auth)
	}
}

// QuotaWindowUsage is one subscription usage window decoded from the passive
// quota snapshot of a credential.
type QuotaWindowUsage struct {
	// Window is QuotaWindow5h or QuotaWindowWeekly.
	Window string
	// Utilization is the used fraction in [0, 1].
	Utilization float64
	// ResetsAt is zero when the upstream did not report a reset time.
	ResetsAt time.Time
}

// QuotaWindows decodes the 5h and weekly usage windows from the credential's
// existing quota observation (Anthropic unified headers, Codex primary and
// secondary windows, Devin weekly quota). It never calls upstream.
func (a *Auth) QuotaWindows() []QuotaWindowUsage {
	if a == nil || len(a.Quota.Signals) == 0 {
		return nil
	}
	signals := make(map[string]string, len(a.Quota.Signals))
	for name, value := range a.Quota.Signals {
		signals[strings.ToLower(strings.TrimSpace(name))] = value
	}
	observedAt := a.Quota.ObservedAt
	switch strings.ToLower(strings.TrimSpace(a.Provider)) {
	case "claude":
		var windows []QuotaWindowUsage
		for _, w := range []struct{ prefix, window string }{
			{"anthropic-ratelimit-unified-5h-", QuotaWindow5h},
			{"anthropic-ratelimit-unified-7d-", QuotaWindowWeekly},
		} {
			utilization, ok := parseQuotaNumber(signals[w.prefix+"utilization"])
			if !ok {
				continue
			}
			windows = append(windows, newQuotaWindowUsage(w.window, utilization, signals, w.prefix+"reset", observedAt))
		}
		return windows
	case "codex":
		var windows []QuotaWindowUsage
		for _, prefix := range []string{"x-codex-primary-", "x-codex-secondary-"} {
			percent, ok := parseQuotaNumber(signals[prefix+"used-percent"])
			if !ok {
				continue
			}
			minutes, ok := parseQuotaNumber(signals[prefix+"window-minutes"])
			if !ok {
				continue
			}
			window := ""
			switch int64(minutes) {
			case 300:
				window = QuotaWindow5h
			case 7 * 24 * 60:
				window = QuotaWindowWeekly
			default:
				continue
			}
			usage := newQuotaWindowUsage(window, percent/100, signals, prefix+"reset-at", observedAt)
			if usage.ResetsAt.IsZero() {
				usage.ResetsAt = quotaResetFromSignal(prefix+"reset-after-seconds", signals[prefix+"reset-after-seconds"], observedAt)
			}
			windows = append(windows, usage)
		}
		return windows
	case "devin":
		remaining, ok := parseQuotaNumber(strings.TrimSuffix(strings.TrimSpace(signals["weekly_quota_remaining_percent"]), "%"))
		if !ok {
			return nil
		}
		return []QuotaWindowUsage{newQuotaWindowUsage(QuotaWindowWeekly, 1-remaining/100, signals, "weekly_quota_reset_at", observedAt)}
	}
	return nil
}

// QuotaPlan returns the subscription plan the proxy knows for the credential,
// or an empty string when none is recorded.
func (a *Auth) QuotaPlan() string {
	if a == nil {
		return ""
	}
	candidates := []string{a.Attributes["plan"], a.Attributes["plan_type"]}
	for _, key := range []string{"plan", "plan_type"} {
		if value, ok := a.Metadata[key].(string); ok {
			candidates = append(candidates, value)
		}
	}
	for name, value := range a.Quota.Signals {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "plan", "x-codex-plan-type":
			candidates = append(candidates, value)
		}
	}
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return DisplayQuotaPlan(candidate)
		}
	}
	return ""
}

// DisplayQuotaPlan capitalizes plan names the upstream reports in lower case
// ("pro" becomes "Pro"). Other spellings are kept as reported.
func DisplayQuotaPlan(plan string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" || plan != strings.ToLower(plan) || strings.ContainsAny(plan, " _-:") {
		return plan
	}
	return strings.ToUpper(plan[:1]) + plan[1:]
}

func newQuotaWindowUsage(window string, utilization float64, signals map[string]string, resetKey string, observedAt time.Time) QuotaWindowUsage {
	return QuotaWindowUsage{
		Window:      window,
		Utilization: math.Min(1, math.Max(0, utilization)),
		ResetsAt:    quotaResetFromSignal(resetKey, signals[resetKey], observedAt),
	}
}

func quotaResetFromSignal(name, value string, observedAt time.Time) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	reset, ok := quotaSignalResetTime(name, value, observedAt)
	if !ok {
		return time.Time{}
	}
	return reset
}

func parseQuotaNumber(value string) (float64, bool) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}
