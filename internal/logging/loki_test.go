package logging

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

func TestLokiHookShipsWarningsAndSkipsInfoAfterStartup(t *testing.T) {
	t.Setenv("TEST_LOKI_TOKEN", "secret")
	var mu sync.Mutex
	var body []byte
	var user, pass string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ = io.ReadAll(r.Body)
		user, pass, _ = r.BasicAuth()
		if r.URL.Path != "/loki/api/v1/push" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	old := processStart
	processStart = time.Now().Add(-time.Hour)
	defer func() { processStart = old }()

	hook := newLokiHook(config.LokiConfig{Enabled: true, URL: server.URL, UserID: "42", TokenEnv: "TEST_LOKI_TOKEN", Labels: map[string]string{"env": "test"}})
	go hook.run()

	logger := log.New()
	logger.SetOutput(io.Discard)
	logger.AddHook(hook)
	logger.Info("routine request")
	logger.Warn("export held")
	hook.close()

	mu.Lock()
	defer mu.Unlock()
	if user != "42" || pass != "secret" {
		t.Fatalf("auth %q %q", user, pass)
	}
	text := string(body)
	if !strings.Contains(text, "export held") || strings.Contains(text, "routine request") {
		t.Fatalf("unexpected payload %s", text)
	}
	var payload struct {
		Streams []struct {
			Stream map[string]string `json:"stream"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Streams) != 1 || payload.Streams[0].Stream["level"] != "warn" || payload.Streams[0].Stream["env"] != "test" || payload.Streams[0].Stream["service"] != "ai-proxy" {
		t.Fatalf("labels %s %v", text, err)
	}
}

func TestLokiHookShipsInfoDuringStartup(t *testing.T) {
	t.Setenv("TEST_LOKI_TOKEN", "secret")
	hook := newLokiHook(config.LokiConfig{Enabled: true, URL: "http://127.0.0.1:1", UserID: "42", TokenEnv: "TEST_LOKI_TOKEN"})
	old := processStart
	processStart = time.Now()
	defer func() { processStart = old }()

	logger := log.New()
	logger.SetOutput(io.Discard)
	logger.AddHook(hook)
	logger.Info("listening")
	logger.Debug("noise")

	if len(hook.lines) != 1 {
		t.Fatalf("queued %d lines", len(hook.lines))
	}
}

func TestLokiConfigValidation(t *testing.T) {
	if err := (config.LokiConfig{Enabled: true, URL: "https://logs.example", UserID: "1"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []config.LokiConfig{{Enabled: true, UserID: "1"}, {Enabled: true, URL: "https://logs.example"}, {Enabled: true, URL: "https://u:p@logs.example", UserID: "1"}, {Enabled: true, URL: "https://logs.example", UserID: "1", MinLevel: "debug"}} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
}
