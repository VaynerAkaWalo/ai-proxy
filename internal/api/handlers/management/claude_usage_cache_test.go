package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const cachedClaudeUsage = `{"five_hour":{"utilization":42,"resets_at":null},"seven_day":null}`

func TestClaudeUsageCacheRefreshAndStaleFallback(t *testing.T) {
	now := time.Date(2026, 10, 7, 20, 0, 0, 0, time.UTC)
	cache := claudeUsageCache{nowFunc: func() time.Time { return now }}
	calls := 0
	result := apiCallResponse{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: cachedClaudeUsage}
	var fetchErr error
	fetch := func(context.Context) (apiCallResponse, error) { calls++; return result, fetchErr }
	read := func() apiCallResponse {
		t.Helper()
		response, err := cache.fetch(context.Background(), "account", fetch)
		cache.mu.Lock()
		done := cache.entries["account"].done
		cache.mu.Unlock()
		if done != nil {
			<-done
			response, err = cache.fetch(context.Background(), "account", fetch)
		}
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := read()
	if first.Cache == nil || first.Cache.Stale || !first.Cache.FetchedAt.Equal(now) {
		t.Fatalf("unexpected cache info: %+v", first.Cache)
	}
	first.Header["Content-Type"][0] = "mutated"
	if response := read(); calls != 1 || response.Header["Content-Type"][0] != "application/json" {
		t.Fatalf("fresh response wasn't safely reused: %+v, calls=%d", response, calls)
	}

	now = now.Add(claudeUsageRefreshInterval)
	result = apiCallResponse{StatusCode: 429, Header: http.Header{"Retry-After": {"900"}}, Body: `{"error":"rate limited"}`}
	response := read()
	if !response.Cache.Stale || response.StatusCode != 200 || response.Body != cachedClaudeUsage || !response.Cache.NextRefreshAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("expected stale success with backoff: %+v", response)
	}
	now = now.Add(14 * time.Minute)
	read()
	if calls != 2 {
		t.Fatalf("retried during cooldown: calls=%d", calls)
	}
	now = now.Add(time.Minute)
	fetchErr = errors.New("network unavailable")
	response = read()
	if !response.Cache.Stale || response.Body != cachedClaudeUsage {
		t.Fatalf("network failure lost cached usage: %+v", response)
	}

	now = now.Add(claudeUsageRefreshInterval)
	fetchErr = nil
	result = apiCallResponse{StatusCode: 200, Body: `{"five_hour":{"utilization":55,"resets_at":null}}`}
	response = read()
	if response.Cache.Stale || response.Body != result.Body || !response.Cache.FetchedAt.Equal(now) {
		t.Fatalf("refresh didn't replace stale data: %+v", response)
	}
}

func TestClaudeUsageCacheFailures(t *testing.T) {
	for _, status := range []int{429, 500, 200, 401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			now := time.Now()
			cache := claudeUsageCache{nowFunc: func() time.Time { return now }}
			_, _ = cache.fetch(context.Background(), "account", func(context.Context) (apiCallResponse, error) {
				return apiCallResponse{StatusCode: 200, Body: cachedClaudeUsage}, nil
			})
			now = now.Add(claudeUsageRefreshInterval)
			fetchFailure := func(context.Context) (apiCallResponse, error) {
				return apiCallResponse{StatusCode: status, Body: `{"error":"failed"}`}, nil
			}
			_, _ = cache.fetch(context.Background(), "account", fetchFailure)
			cache.mu.Lock()
			done := cache.entries["account"].done
			cache.mu.Unlock()
			if done != nil {
				<-done
			}
			response, err := cache.fetch(context.Background(), "account", fetchFailure)
			if err != nil {
				t.Fatal(err)
			}
			if status == 401 || status == 403 {
				if response.StatusCode != status || response.Cache != nil {
					t.Fatalf("auth failure was masked: %+v", response)
				}
			} else if response.StatusCode != 200 || response.Body != cachedClaudeUsage || !response.Cache.Stale {
				t.Fatalf("lost stale success: %+v", response)
			}
		})
	}
}

func TestClaudeUsageCacheColdRateLimitBackoff(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cache := claudeUsageCache{nowFunc: func() time.Time { return now }}
	calls := 0
	fetch := func(context.Context) (apiCallResponse, error) {
		calls++
		return apiCallResponse{StatusCode: 429, Header: http.Header{"Retry-After": {now.Add(10 * time.Minute).Format(http.TimeFormat)}}}, nil
	}
	response, _ := cache.fetch(context.Background(), "account", fetch)
	if response.StatusCode != 429 || response.Cache != nil {
		t.Fatalf("invented quota data: %+v", response)
	}
	now = now.Add(9 * time.Minute)
	_, _ = cache.fetch(context.Background(), "account", fetch)
	if calls != 1 {
		t.Fatalf("cold 429 wasn't throttled: %d", calls)
	}
	now = now.Add(time.Minute)
	_, _ = cache.fetch(context.Background(), "account", fetch)
	if calls != 2 {
		t.Fatalf("didn't retry after HTTP date: %d", calls)
	}
}

func TestClaudeUsageCacheConcurrentRequests(t *testing.T) {
	var cache claudeUsageCache
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) (apiCallResponse, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return apiCallResponse{StatusCode: 200, Body: cachedClaudeUsage}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			response, err := cache.fetch(context.Background(), "account", fetch)
			if err != nil || response.Body != cachedClaudeUsage {
				t.Errorf("unexpected response %+v, error %v", response, err)
			}
		})
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.fetch(ctx, "account", fetch); !errors.Is(err, context.Canceled) {
		t.Errorf("waiting request wasn't canceled: %v", err)
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent probes=%d, want 1", calls.Load())
	}
}

func TestClaudeUsageCacheRequestScope(t *testing.T) {
	auth := &coreauth.Auth{ID: "claude.json", Provider: "claude"}
	request := func(method, target, bearer string) *http.Request {
		req, _ := http.NewRequest(method, target, nil)
		req.Header.Set("Authorization", bearer)
		return req
	}
	base := request("GET", "https://api.anthropic.com/api/oauth/usage", "Bearer token")
	key, ok := claudeUsageCacheKey(base, auth)
	if !ok {
		t.Fatal("usage request excluded")
	}
	base.Header.Set("User-Agent", "other-client")
	if other, _ := claudeUsageCacheKey(base, auth); key != other {
		t.Fatal("clients cannot share usage cache")
	}
	for _, req := range []*http.Request{
		request("POST", base.URL.String(), "Bearer token"),
		request("GET", "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1", "Bearer token"),
		request("GET", "https://api.anthropic.com/api/oauth/profile", "Bearer token"),
		request("GET", "https://other.example/api/oauth/usage", "Bearer token"),
		request("GET", base.URL.String(), ""),
	} {
		if _, ok := claudeUsageCacheKey(req, auth); ok {
			t.Errorf("cached unrelated request %s %s", req.Method, req.URL)
		}
	}
	base.Host = "other.example"
	if _, ok := claudeUsageCacheKey(base, auth); ok {
		t.Fatal("cached overridden host")
	}
	base.Host = "api.anthropic.com"
	base.Header.Set("Authorization", "Bearer different-token")
	if other, _ := claudeUsageCacheKey(base, auth); other == key {
		t.Fatal("tokens share cache")
	}
	base.Header.Set("Authorization", "Bearer token")
	auth.ID = "other-account"
	if other, _ := claudeUsageCacheKey(base, auth); other == key {
		t.Fatal("accounts share cache")
	}
}

func TestAPICallClaudeUsageCacheBothManagementVersions(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	auth := &coreauth.Auth{ID: "claude.json", Provider: "claude", Metadata: map[string]any{"access_token": "test-token"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: manager}
	calls := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		if req.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("token wasn't resolved")
		}
		_, _ = w.Write([]byte(cachedClaudeUsage))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target.Host)
	}
	original := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = original }()
	router := gin.New()
	router.POST("/v0/management/api-call", h.APICall)
	router.POST("/v8/management/requests/api-call", h.APICall)
	for _, path := range []string{"/v0/management/api-call", "/v8/management/requests/api-call"} {
		recorder := httptest.NewRecorder()
		body := fmt.Sprintf(`{"auth_index":%q,"method":"GET","url":"https://api.anthropic.com/api/oauth/usage","header":{"Authorization":"Bearer $TOKEN$"}}`, auth.EnsureIndex())
		router.ServeHTTP(recorder, httptest.NewRequest("POST", path, strings.NewReader(body)))
		var response apiCallResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != 200 || response.Cache == nil || response.Body != cachedClaudeUsage {
			t.Fatalf("%s: %s", path, recorder.Body)
		}
	}
	if calls != 1 {
		t.Fatalf("versions didn't share cache: %d calls", calls)
	}
}

func TestClaudeUsageCacheBoundedEviction(t *testing.T) {
	now := time.Now()
	cache := claudeUsageCache{nowFunc: func() time.Time { return now }}
	fetch := func(context.Context) (apiCallResponse, error) {
		return apiCallResponse{StatusCode: 200, Body: cachedClaudeUsage}, nil
	}
	for i := 0; i < claudeUsageCacheCapacity; i++ {
		_, _ = cache.fetch(context.Background(), fmt.Sprint(i), fetch)
		now = now.Add(time.Second)
	}
	_, _ = cache.fetch(context.Background(), "new", fetch)
	if len(cache.entries) != claudeUsageCacheCapacity || cache.entries["0"] != nil || cache.entries["new"] == nil {
		t.Fatal("cache didn't evict the least recently used entry")
	}
}

func TestClaudeUsageCacheRepeatedRateLimitsBackOff(t *testing.T) {
	now := time.Now()
	cache := claudeUsageCache{nowFunc: func() time.Time { return now }}
	fetch := func(context.Context) (apiCallResponse, error) { return apiCallResponse{StatusCode: 429}, nil }
	for _, backoff := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		_, _ = cache.fetch(context.Background(), "account", fetch)
		if got := cache.entries["account"].nextRefreshAt.Sub(now); got != backoff {
			t.Fatalf("backoff=%s, want %s", got, backoff)
		}
		now = now.Add(backoff)
	}
	for _, value := range []string{"", "invalid", "-1", "0", "9223372036854775807"} {
		if got := claudeUsageRetryAt(value, now); !got.IsZero() {
			t.Errorf("invalid Retry-After %q produced %s", value, got)
		}
	}
}

func TestClaudeUsageCacheServesStaleWhileRevalidating(t *testing.T) {
	now := time.Now()
	cache := claudeUsageCache{nowFunc: func() time.Time { return now }}
	_, _ = cache.fetch(context.Background(), "account", func(context.Context) (apiCallResponse, error) {
		return apiCallResponse{StatusCode: 200, Body: cachedClaudeUsage}, nil
	})
	now = now.Add(claudeUsageRefreshInterval)
	started, release := make(chan struct{}), make(chan struct{})
	fetch := func(ctx context.Context) (apiCallResponse, error) {
		close(started)
		<-release
		if ctx.Err() != nil {
			t.Error("background refresh inherited caller cancellation")
		}
		return apiCallResponse{StatusCode: 200, Body: cachedClaudeUsage}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, err := cache.fetch(ctx, "account", fetch)
	if err != nil || response.Cache == nil || !response.Cache.Stale || response.Body != cachedClaudeUsage {
		t.Fatalf("didn't immediately serve cached quota: %+v, %v", response, err)
	}
	<-started
	cancel()
	next, err := cache.fetch(context.Background(), "account", fetch)
	if err != nil || !next.Cache.Stale || next.Body != cachedClaudeUsage {
		t.Fatalf("concurrent client couldn't read stale quota: %+v, %v", next, err)
	}
	cache.mu.Lock()
	done := cache.entries["account"].done
	cache.mu.Unlock()
	close(release)
	<-done
	refreshed, err := cache.fetch(context.Background(), "account", fetch)
	if err != nil || refreshed.Cache.Stale {
		t.Fatalf("revalidation didn't finish: %+v, %v", refreshed, err)
	}
}
