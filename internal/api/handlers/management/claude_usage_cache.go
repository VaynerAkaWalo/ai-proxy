package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	claudeUsageRefreshInterval = 5 * time.Minute
	claudeUsageMaxBackoff      = time.Hour
	claudeUsageCacheCapacity   = 256
)

type claudeUsageCacheInfo struct {
	FetchedAt     time.Time `json:"fetched_at"`
	NextRefreshAt time.Time `json:"next_refresh_at"`
	Stale         bool      `json:"stale"`
}

type claudeUsageCacheEntry struct {
	success       *apiCallResponse
	result        apiCallResponse
	err           error
	fetchedAt     time.Time
	nextRefreshAt time.Time
	lastUsed      time.Time
	backoff       time.Duration
	done          chan struct{}
}

type claudeUsageCache struct {
	mu      sync.Mutex
	entries map[string]*claudeUsageCacheEntry
	nowFunc func() time.Time
}

func claudeUsageCacheKey(req *http.Request, auth *coreauth.Auth) (string, bool) {
	if auth == nil || !strings.EqualFold(auth.Provider, "claude") || auth.ID == "" {
		return "", false
	}
	if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Host != "api.anthropic.com" || req.URL.EscapedPath() != "/api/oauth/usage" || req.URL.RawQuery != "" || req.URL.ForceQuery || req.Body != nil {
		return "", false
	}
	if req.Host != "" && !strings.EqualFold(req.Host, "api.anthropic.com") {
		return "", false
	}
	authorization := req.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")) == "" {
		return "", false
	}
	// Different clients use different user agents for the same account's usage.
	identity, _ := json.Marshal([]string{auth.ID, authorization, req.Header.Get("Anthropic-Beta"), req.Header.Get("Anthropic-Organization-Id"), req.Header.Get("Anthropic-Version")})
	sum := sha256.Sum256(identity)
	return hex.EncodeToString(sum[:]), true
}

func (cache *claudeUsageCache) now() time.Time {
	if cache.nowFunc != nil {
		return cache.nowFunc()
	}
	return time.Now()
}

func (cache *claudeUsageCache) fetch(ctx context.Context, key string, fetch func(context.Context) (apiCallResponse, error)) (apiCallResponse, error) {
	for {
		cache.mu.Lock()
		now := cache.now()
		if cache.entries == nil {
			cache.entries = make(map[string]*claudeUsageCacheEntry)
		}
		entry := cache.entries[key]
		if entry == nil {
			cache.evict()
			if len(cache.entries) >= claudeUsageCacheCapacity {
				cache.mu.Unlock()
				return fetch(ctx)
			}
			entry = &claudeUsageCacheEntry{}
			cache.entries[key] = entry
		}
		entry.lastUsed = now
		if entry.done != nil {
			if entry.success != nil {
				response, err := entry.response(now)
				cache.mu.Unlock()
				return response, err
			}
			done := entry.done
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return apiCallResponse{}, ctx.Err()
			case <-done:
				continue
			}
		}
		if now.Before(entry.nextRefreshAt) {
			response, err := entry.response(now)
			cache.mu.Unlock()
			return response, err
		}
		entry.done = make(chan struct{})
		if entry.success != nil {
			response, err := entry.response(now)
			go cache.refresh(context.WithoutCancel(ctx), entry, fetch)
			cache.mu.Unlock()
			return response, err
		}
		cache.mu.Unlock()
		return cache.refresh(ctx, entry, fetch)
	}
}

func (cache *claudeUsageCache) refresh(ctx context.Context, entry *claudeUsageCacheEntry, fetch func(context.Context) (apiCallResponse, error)) (apiCallResponse, error) {
	result, err := fetch(ctx)
	cache.mu.Lock()
	defer cache.mu.Unlock()

	now := cache.now()
	entry.update(result, err, now)
	close(entry.done)
	entry.done = nil
	return entry.response(now)
}

func (cache *claudeUsageCache) evict() {
	if len(cache.entries) < claudeUsageCacheCapacity {
		return
	}
	var oldestKey string
	var oldest *claudeUsageCacheEntry
	for key, entry := range cache.entries {
		if entry.done == nil && (oldest == nil || entry.lastUsed.Before(oldest.lastUsed)) {
			oldestKey, oldest = key, entry
		}
	}
	if oldest != nil {
		delete(cache.entries, oldestKey)
	}
}

func (entry *claudeUsageCacheEntry) update(result apiCallResponse, err error, now time.Time) {
	entry.result, entry.err = result, err
	entry.nextRefreshAt = now.Add(claudeUsageRefreshInterval)
	if err == nil && result.StatusCode == http.StatusOK && validClaudeUsage(result.Body) {
		entry.success = &result
		entry.fetchedAt = now
		entry.backoff = 0
		return
	}

	if err == nil && (result.StatusCode == http.StatusUnauthorized || result.StatusCode == http.StatusForbidden) {
		entry.success = nil
	}
	if err == nil && result.StatusCode == http.StatusTooManyRequests {
		entry.backoff = max(claudeUsageRefreshInterval, min(entry.backoff*2, claudeUsageMaxBackoff))
		entry.nextRefreshAt = now.Add(entry.backoff)
		if retryAt := claudeUsageRetryAt(http.Header(result.Header).Get("Retry-After"), now); retryAt.After(entry.nextRefreshAt) {
			entry.nextRefreshAt = retryAt
		}
	}
}

func validClaudeUsage(body string) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &payload) != nil {
		return false
	}
	for _, key := range []string{"five_hour", "seven_day", "limits", "extra_usage"} {
		if _, ok := payload[key]; ok {
			return true
		}
	}
	return false
}

func claudeUsageRetryAt(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= int64((1<<63-1)/int64(time.Second)) {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		return retryAt
	}
	return time.Time{}
}

func (entry *claudeUsageCacheEntry) response(now time.Time) (apiCallResponse, error) {
	if entry.success == nil {
		return entry.result, entry.err
	}
	result := *entry.success
	result.Header = http.Header(result.Header).Clone()
	result.Cache = &claudeUsageCacheInfo{
		FetchedAt:     entry.fetchedAt,
		NextRefreshAt: entry.nextRefreshAt,
		Stale:         entry.err != nil || entry.result.StatusCode != http.StatusOK || !validClaudeUsage(entry.result.Body),
	}
	if entry.done != nil {
		result.Cache.Stale = true
		result.Cache.NextRefreshAt = now.Add(30 * time.Second)
	}
	return result, nil
}
