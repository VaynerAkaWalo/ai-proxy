package claude

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func setClaudeRateLimitResetHeaders(headers http.Header, status int, now time.Time) {
	if status != http.StatusTooManyRequests || headers.Get("Anthropic-Ratelimit-Unified-Reset") != "" {
		return
	}

	raw := strings.TrimSpace(headers.Get("Retry-After"))
	seconds, errParse := strconv.ParseInt(raw, 10, 64)
	var reset time.Time
	if errParse == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
		reset = now.Add(time.Duration(seconds) * time.Second)
	} else {
		reset, errParse = http.ParseTime(raw)
		if errParse != nil || !reset.After(now) {
			return
		}
	}

	resetSeconds := reset.Unix()
	if reset.Nanosecond() != 0 {
		resetSeconds++
	}

	// Claude SDK usage-limit events read unified reset headers rather than Retry-After.
	headers.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(resetSeconds, 10))
	if headers.Get("Anthropic-Ratelimit-Unified-Status") == "" {
		headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	}
}
