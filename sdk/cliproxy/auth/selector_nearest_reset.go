package auth

import (
	"context"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// NearestResetSelector prefers the credential whose quota window resets soonest, so
// quota that is about to expire is spent before quota that has plenty of runway.
//
// Reset times come from the passive quota snapshot (QuotaState.Signals). Credentials
// without a usable future reset time are only chosen when no candidate has one, and
// are then rotated by the fallback selector.
type NearestResetSelector struct {
	fallback RoundRobinSelector
}

// Pick selects the available credential with the earliest upcoming quota reset.
func (s *NearestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	var picked *Auth
	var pickedReset time.Time
	for _, candidate := range available {
		reset, ok := nextQuotaReset(candidate, now)
		if !ok {
			continue
		}
		if picked == nil || reset.Before(pickedReset) {
			picked, pickedReset = candidate, reset
		}
	}
	if picked != nil {
		return picked, nil
	}

	return s.fallback.Pick(ctx, provider, model, opts, available)
}

// nextQuotaReset returns the earliest reset time after now across all quota windows
// observed for the credential.
func nextQuotaReset(auth *Auth, now time.Time) (time.Time, bool) {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return time.Time{}, false
	}

	var earliest time.Time
	for name, value := range auth.Quota.Signals {
		reset, ok := quotaSignalResetTime(name, value, auth.Quota.ObservedAt)
		if !ok || !reset.After(now) {
			continue
		}
		if earliest.IsZero() || reset.Before(earliest) {
			earliest = reset
		}
	}

	return earliest, !earliest.IsZero()
}

// quotaSignalResetTime decodes a single quota signal into an absolute reset time.
// Signals that do not describe a reset are ignored.
func quotaSignalResetTime(name, value string, observedAt time.Time) (time.Time, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	value = strings.TrimSpace(value)

	switch {
	case strings.HasSuffix(name, "-reset-after-seconds"):
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds < 0 || observedAt.IsZero() {
			return time.Time{}, false
		}
		return observedAt.Add(time.Duration(seconds) * time.Second), true
	case strings.HasPrefix(name, "anthropic-ratelimit-unified-") && strings.HasSuffix(name, "-reset"),
		strings.HasSuffix(name, "-reset-at"),
		strings.HasSuffix(name, "_reset_at"):
		if unix, err := strconv.ParseInt(value, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0), true
		}
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			return parsed, true
		}
	}

	return time.Time{}, false
}
