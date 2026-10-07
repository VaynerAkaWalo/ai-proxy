package claude

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeRateLimitResetHeaders(t *testing.T) {
	now := time.Date(2026, time.October, 7, 22, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name       string
		retryAfter string
		status     int
		now        time.Time
		wantReset  string
	}{
		{"delta seconds", "20", http.StatusTooManyRequests, now, "1791410420"},
		{"fractional clock rounds up", "20", http.StatusTooManyRequests, now.Add(time.Millisecond), "1791410421"},
		{"HTTP date", now.Add(time.Hour).Format(http.TimeFormat), http.StatusTooManyRequests, now, "1791414000"},
		{"missing retry", "", http.StatusTooManyRequests, now, ""},
		{"invalid retry", "later", http.StatusTooManyRequests, now, ""},
		{"zero retry", "0", http.StatusTooManyRequests, now, ""},
		{"negative retry", "-20", http.StatusTooManyRequests, now, ""},
		{"overflowing duration", "9223372037", http.StatusTooManyRequests, now, ""},
		{"overflowing integer", "9223372036854775808", http.StatusTooManyRequests, now, ""},
		{"expired date", now.Add(-time.Hour).Format(http.TimeFormat), http.StatusTooManyRequests, now, ""},
		{"current date", now.Format(http.TimeFormat), http.StatusTooManyRequests, now, ""},
		{"concurrency busy", "20", http.StatusServiceUnavailable, now, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{"Retry-After": {tt.retryAfter}}
			setClaudeRateLimitResetHeaders(headers, tt.status, tt.now, nil)

			if got := headers.Get("Anthropic-Ratelimit-Unified-Reset"); got != tt.wantReset {
				t.Fatalf("unified reset = %q, want %q", got, tt.wantReset)
			}
			wantStatus := ""
			if tt.wantReset != "" {
				wantStatus = "rejected"
			}
			if got := headers.Get("Anthropic-Ratelimit-Unified-Status"); got != wantStatus {
				t.Fatalf("unified status = %q, want %q", got, wantStatus)
			}
			if got := headers.Get("Retry-After"); got != tt.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tt.retryAfter)
			}
			if got := headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != "" {
				t.Fatalf("unexpected subscription claim %q", got)
			}
		})
	}
}

func TestClaudeRateLimitResetHeadersPreserveUpstream(t *testing.T) {
	now := time.Date(2026, time.October, 7, 22, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("Retry-After", "20")
	headers.Set("Anthropic-Ratelimit-Unified-Reset", "1791414000")
	headers.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	headers.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")

	setClaudeRateLimitResetHeaders(headers, http.StatusTooManyRequests, now, nil)

	if got := headers.Get("Anthropic-Ratelimit-Unified-Reset"); got != "1791414000" {
		t.Fatalf("unified reset = %q, want upstream reset", got)
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-Status"); got != "allowed" {
		t.Fatalf("unified status = %q, want upstream status", got)
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != "seven_day" {
		t.Fatalf("unified claim = %q, want upstream claim", got)
	}
}

type claudeResetError struct {
	reset time.Time
}

func (e claudeResetError) Error() string { return "Claude usage limit reached" }

func (e claudeResetError) ClaudeRateLimitReset() time.Time { return e.reset }

func TestClaudeRateLimitResetHeadersFromExecutor(t *testing.T) {
	now := time.Date(2026, time.October, 7, 22, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name       string
		reset      time.Time
		status     int
		retryAfter string
		wantReset  string
	}{
		{"future reset", now.Add(time.Hour), 429, "", "1791414000"},
		{"fractional reset rounds up", now.Add(time.Hour + time.Millisecond), 429, "", "1791414001"},
		{"expired reset", now.Add(-time.Hour), 429, "", ""},
		{"missing reset", time.Time{}, 429, "", ""},
		{"other status", now.Add(time.Hour), 503, "", ""},
		{"preserve retry after", now.Add(time.Hour), 429, "20", "1791410420"},
		{"invalid retry falls back", now.Add(time.Hour), 429, "later", "1791414000"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{"Retry-After": {tt.retryAfter}}
			err := fmt.Errorf("dispatch failed: %w", claudeResetError{reset: tt.reset})
			setClaudeRateLimitResetHeaders(headers, tt.status, now, err)

			if got := headers.Get("Anthropic-Ratelimit-Unified-Reset"); got != tt.wantReset {
				t.Fatalf("unified reset = %q, want %q", got, tt.wantReset)
			}
		})
	}
}

func TestWriteClaudeErrorResponseIncludesUnifiedReset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name string
		msg  *interfaces.ErrorMessage
	}{
		{"model cooldown", &interfaces.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      coreauth.NewModelCooldownError("claude-sonnet-5-5", "claude", 20*time.Second),
		}},
		{"wrapped cooldown", &interfaces.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      fmt.Errorf("dispatch failed: %w", coreauth.NewModelCooldownError("claude-sonnet-5-5", "claude", 20*time.Second)),
		}},
		{"direct upstream response", &interfaces.ErrorMessage{
			StatusCode:     http.StatusTooManyRequests,
			DirectResponse: true,
			Headers:        http.Header{"Retry-After": {"20"}},
			Body:           []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"retry later"}}`),
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			handler := &ClaudeCodeAPIHandler{}

			before := time.Now().Add(20 * time.Second).Unix()
			handler.WriteErrorResponse(c, tt.msg)
			after := time.Now().Add(21 * time.Second).Unix()

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", recorder.Code)
			}
			headers := recorder.Result().Header
			if got := headers.Get("Retry-After"); got != "20" {
				t.Fatalf("Retry-After = %q, want 20", got)
			}
			if got := headers.Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
				t.Fatalf("unified status = %q, want rejected", got)
			}
			reset, errParse := strconv.ParseInt(headers.Get("Anthropic-Ratelimit-Unified-Reset"), 10, 64)
			if errParse != nil || reset < before || reset > after {
				t.Fatalf("unified reset = %d, want between %d and %d, error %v", reset, before, after, errParse)
			}
		})
	}
}

func TestWriteClaudeRateLimitWithoutTiming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}

	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit without a reset time"),
	})

	if got := recorder.Result().Header.Get("Anthropic-Ratelimit-Unified-Reset"); got != "" {
		t.Fatalf("unexpected unified reset %q", got)
	}
}
