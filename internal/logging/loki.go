package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	lokiQueueSize     = 1024
	lokiBatchSize     = 100
	lokiFlushInterval = 5 * time.Second
	lokiCloseGrace    = 3 * time.Second
)

var processStart = time.Now()

type lokiLine struct {
	level string
	at    time.Time
	text  string
}

// lokiHook ships warnings, errors and startup logs to Loki without ever blocking the caller.
// Failures are reported on stderr only, since logging them through logrus would feed back into this hook.
type lokiHook struct {
	cfg      config.LokiConfig
	token    string
	client   *http.Client
	minLevel log.Level
	lines    chan lokiLine
	dropped  atomic.Uint64
	failing  atomic.Bool
	stop     chan struct{}
	done     chan struct{}
}

func newLokiHook(cfg config.LokiConfig) *lokiHook {
	cfg = cfg.Defaults()
	minLevel := log.WarnLevel
	switch cfg.MinLevel {
	case "error":
		minLevel = log.ErrorLevel
	case "info":
		minLevel = log.InfoLevel
	}
	return &lokiHook{
		cfg: cfg, token: os.Getenv(cfg.TokenEnv), client: &http.Client{}, minLevel: minLevel,
		lines: make(chan lokiLine, lokiQueueSize), stop: make(chan struct{}), done: make(chan struct{}),
	}
}

func (h *lokiHook) Levels() []log.Level { return log.AllLevels }

func (h *lokiHook) Fire(entry *log.Entry) error {
	inStartup := entry.Level <= log.InfoLevel && time.Since(processStart) < time.Duration(h.cfg.StartupSeconds)*time.Second
	if entry.Level > h.minLevel && !inStartup {
		return nil
	}
	raw, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		return nil
	}
	level := entry.Level.String()
	if level == "warning" {
		level = "warn"
	}
	select {
	case h.lines <- lokiLine{level: level, at: entry.Time, text: strings.TrimRight(string(raw), "\n")}:
	default:
		h.dropped.Add(1)
	}
	return nil
}

func (h *lokiHook) run() {
	defer close(h.done)
	ticker := time.NewTicker(lokiFlushInterval)
	defer ticker.Stop()
	var batch []lokiLine
	for {
		select {
		case line := <-h.lines:
			batch = append(batch, line)
			if len(batch) >= lokiBatchSize {
				h.push(context.Background(), batch)
				batch = nil
			}
		case <-ticker.C:
			if len(batch) > 0 {
				h.push(context.Background(), batch)
				batch = nil
			}
		case <-h.stop:
			for drained := false; !drained; {
				select {
				case line := <-h.lines:
					batch = append(batch, line)
				default:
					drained = true
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), lokiCloseGrace)
			defer cancel()
			if len(batch) > 0 {
				h.push(ctx, batch)
			}
			return
		}
	}
}

func (h *lokiHook) close() {
	close(h.stop)
	<-h.done
}

func (h *lokiHook) push(ctx context.Context, batch []lokiLine) {
	byLevel := map[string][][2]string{}
	for _, line := range batch {
		byLevel[line.level] = append(byLevel[line.level], [2]string{strconv.FormatInt(line.at.UnixNano(), 10), line.text})
	}
	type stream struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	streams := make([]stream, 0, len(byLevel))
	for level, values := range byLevel {
		labels := map[string]string{"service": "ai-proxy", "level": level}
		for k, v := range h.cfg.Labels {
			labels[k] = v
		}
		streams = append(streams, stream{labels, values})
	}
	body, err := json.Marshal(map[string]any{"streams": streams})
	if err == nil {
		err = h.send(ctx, body)
	}
	h.report(err, len(batch))
}

func (h *lokiHook) send(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h.cfg.URL, "/")+"/loki/api/v1/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(h.cfg.UserID, h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

func (h *lokiHook) report(err error, lines int) {
	if err != nil {
		if !h.failing.Swap(true) {
			fmt.Fprintf(os.Stderr, "loki log shipping failed, dropping %d lines: %v\n", lines, err)
		}
		return
	}
	if h.failing.Swap(false) {
		fmt.Fprintf(os.Stderr, "loki log shipping recovered (dropped %d lines while unavailable)\n", h.dropped.Load())
	}
}

var (
	lokiMu   sync.Mutex
	lokiCur  *lokiHook
	lokiConf config.LokiConfig
)

// configureLoki starts, replaces or stops the Loki hook so it matches cfg.
func configureLoki(cfg config.LokiConfig) {
	lokiMu.Lock()
	defer lokiMu.Unlock()

	if lokiCur != nil && reflect.DeepEqual(lokiConf, cfg) {
		return
	}
	if lokiCur != nil {
		log.StandardLogger().ReplaceHooks(removeHook(log.StandardLogger().Hooks, lokiCur))
		lokiCur.close()
		lokiCur = nil
	}
	lokiConf = cfg
	if !cfg.Enabled {
		return
	}
	if err := cfg.Validate(); err != nil {
		log.Warnf("loki log shipping disabled: %v", err)
		return
	}
	hook := newLokiHook(cfg)
	if hook.token == "" {
		log.Warnf("loki log shipping disabled: set %s", hook.cfg.TokenEnv)
		return
	}
	go hook.run()
	log.AddHook(hook)
	lokiCur = hook
}

func removeHook(hooks log.LevelHooks, target log.Hook) log.LevelHooks {
	kept := log.LevelHooks{}
	for level, list := range hooks {
		for _, hook := range list {
			if hook != target {
				kept[level] = append(kept[level], hook)
			}
		}
	}
	return kept
}

func closeLoki() {
	lokiMu.Lock()
	defer lokiMu.Unlock()
	if lokiCur != nil {
		lokiCur.close()
		lokiCur = nil
	}
}
