package test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/access"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const gatewayClientKey = "harness-client-key"

type accountingMode string

const (
	accountingDisabled accountingMode = "disabled"
	accountingEnabled  accountingMode = "enabled"
)

var allAccountingModes = []accountingMode{accountingDisabled, accountingEnabled}

var newServerMu sync.Mutex

func clientKeyID(key string) string {
	sum := sha256.Sum256([]byte("client\x00" + key))
	return hex.EncodeToString(sum[:])
}

type upstreamAccount struct {
	id         string
	provider   string
	apiKey     string
	models     []string
	priority   int
	websockets bool
}

type gatewayOptions struct {
	mode        accountingMode
	upstreamURL string
	accounts    []upstreamAccount
	dataPath    string
}

type gateway struct {
	t        *testing.T
	URL      string
	manager  *cliproxyauth.Manager
	outbox   *usage.Outbox
	front    *httptest.Server
	detach   func()
	stopped  bool
	dataPath string
}

func fixturePricing() config.PricingConfig {
	rate := func(provider, model string) config.PriceRate {
		input, output, cacheRead, cacheCreate := "0.000001", "0.000004", "0.0000001", "0.00000125"
		return config.PriceRate{Provider: provider, Model: model, Currency: "USD", Input: &input, Output: &output, CacheRead: &cacheRead, CacheCreation: &cacheCreate}
	}
	return config.PricingConfig{Overrides: []config.PriceRate{
		rate("fixture-chat", "grok-fixture-chat"),
		rate("codex", "gpt-5.4"),
		rate("claude", "claude-sonnet-fixture"),
	}}
}

func newGateway(t *testing.T, opts gatewayOptions) *gateway {
	t.Helper()
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{gatewayClientKey}}}
	dir := t.TempDir()
	cfg.AuthDir = filepath.Join(dir, "auth")
	if err := os.MkdirAll(cfg.AuthDir, 0o700); err != nil {
		t.Fatal(err)
	}

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor("fixture-chat", cfg))
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	for _, account := range opts.accounts {
		registerFixtureAccount(t, manager, account, opts.upstreamURL)
	}

	accessManager := sdkaccess.NewManager()
	if _, err := access.ApplyAccessProviders(accessManager, nil, cfg); err != nil {
		t.Fatal(err)
	}
	newServerMu.Lock()
	gin.SetMode(gin.TestMode)
	server := api.NewServer(cfg, manager, accessManager, filepath.Join(dir, "config.yaml"))
	newServerMu.Unlock()
	front := httptest.NewServer(server.Handler())

	g := &gateway{t: t, URL: front.URL, manager: manager, front: front}
	t.Cleanup(g.shutdown)
	if opts.dataPath == "" {
		opts.dataPath = filepath.Join(dir, "accounting")
	}
	g.dataPath = opts.dataPath
	g.attachAccounting(opts)
	return g
}

func registerFixtureAccount(t *testing.T, manager *cliproxyauth.Manager, account upstreamAccount, baseURL string) {
	t.Helper()
	models := make([]*registry.ModelInfo, 0, len(account.models))
	for _, model := range account.models {
		models = append(models, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(account.id, account.provider, models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(account.id) })

	attrs := map[string]string{
		"priority": fmt.Sprint(account.priority),
		"base_url": baseURL,
		"api_key":  account.apiKey,
	}
	if account.websockets {
		attrs["websockets"] = "true"
	}
	if _, err := manager.Register(context.Background(), &cliproxyauth.Auth{
		ID: account.id, Provider: account.provider, Status: cliproxyauth.StatusActive, Attributes: attrs,
	}); err != nil {
		t.Fatal(err)
	}
}

func (g *gateway) attachAccounting(opts gatewayOptions) {
	t := g.t
	if opts.mode == accountingDisabled {
		return
	}
	cfg := sdkconfig.AccountingOutboxConfig{Enabled: true, DataPath: opts.dataPath, Pricing: fixturePricing()}
	if err := os.MkdirAll(cfg.DataPath, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg = cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	outbox, err := usage.OpenOutbox(cfg)
	if err != nil {
		t.Fatal(err)
	}
	detach, err := usage.DefaultManager().AttachOutbox(outbox)
	if err != nil {
		t.Fatal(err)
	}
	g.outbox, g.detach = outbox, detach
}

func (g *gateway) shutdown() {
	if g.stopped {
		return
	}
	g.stopped = true
	g.front.Close()
	if g.detach == nil {
		return
	}
	g.detach()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.outbox.Close(ctx); err != nil {
		g.t.Errorf("outbox did not close within the shutdown bound: %v", err)
	}
}

type accountingRecorder struct {
	mu     sync.Mutex
	events []usage.AccountingEvent
}

var activeRecorder atomic.Pointer[accountingRecorder]

type recorderPlugin struct{}

func (recorderPlugin) HandleUsage(ctx context.Context, record usage.Record) {
	recorder := activeRecorder.Load()
	if recorder == nil || record.AdditionalModel {
		return
	}
	event := usage.NewAccountingEvent(ctx, record)
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
}

func init() { usage.RegisterNamedPlugin("harness-recorder", recorderPlugin{}) }

func recordAccounting(t *testing.T) *accountingRecorder {
	t.Helper()
	recorder := &accountingRecorder{}
	activeRecorder.Store(recorder)
	t.Cleanup(func() { activeRecorder.Store(nil) })
	return recorder
}

func (r *accountingRecorder) waitEvents(t *testing.T, want int) []usage.AccountingEvent {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		r.mu.Lock()
		got := append([]usage.AccountingEvent(nil), r.events...)
		r.mu.Unlock()
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("accounting events = %d, want at least %d", len(got), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *accountingRecorder) tryEvents(want int, timeout time.Duration) []usage.AccountingEvent {
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		got := append([]usage.AccountingEvent(nil), r.events...)
		r.mu.Unlock()
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (g *gateway) durableEvents(t *testing.T, events []usage.AccountingEvent) []usage.AccountingEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	stored := make([]usage.AccountingEvent, 0, len(events))
	for _, event := range events {
		for {
			durable, err := g.outbox.Event(event.ExecutionID)
			if err == nil {
				stored = append(stored, durable)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("event %s never became durable: %v", event.ExecutionID, err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return stored
}
