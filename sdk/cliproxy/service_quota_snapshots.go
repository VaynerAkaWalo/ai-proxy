package cliproxy

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotaprobe"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// quotaSnapshotSampler turns the credentials' existing passive quota
// observations into durable quota snapshot rows. It never calls upstream.
//
// A window is exported when its observation is newer than the last exported one
// and either utilization or reset time changed, or at least the configured
// interval of observation time passed. observed_at is the upstream observation
// time, so idle accounts do not get fabricated fresh timestamps.
type quotaSnapshotSampler struct {
	outbox  *usage.Outbox
	manager *coreauth.Manager
	cfg     config.QuotaSnapshotsConfig
	config  func() *config.Config

	changed chan *coreauth.Auth
	cancel  context.CancelFunc
	done    chan struct{}

	mu       sync.Mutex
	last     map[string]quotaSnapshotMark
	probes   map[string]*quotaProbeState
	failures int
}

// quotaProbeState is the latest upstream probe of one credential and its schedule.
type quotaProbeState struct {
	result  *quotaprobe.Result
	nextAt  time.Time
	backoff time.Duration
}

const (
	quotaProbeInterval   = 15 * time.Minute
	quotaProbeMaxBackoff = time.Hour
)

type quotaSnapshotMark struct {
	utilization string
	resetsAt    time.Time
	observedAt  time.Time
	manual      int    // -1 when unknown
	metadata    string // plan, price and manual resets
}

func newQuotaSnapshotSampler(outbox *usage.Outbox, manager *coreauth.Manager, cfg config.QuotaSnapshotsConfig, current func() *config.Config) *quotaSnapshotSampler {
	return &quotaSnapshotSampler{outbox: outbox, manager: manager, cfg: cfg, config: current, changed: make(chan *coreauth.Auth, 256), last: make(map[string]quotaSnapshotMark), probes: make(map[string]*quotaProbeState)}
}

func (q *quotaSnapshotSampler) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel, q.done = cancel, make(chan struct{})
	q.manager.SetQuotaObserver(q.notify)
	go q.probeLoop(ctx)
	go func() {
		defer close(q.done)
		ticker := time.NewTicker(q.cfg.QuotaSnapshotInterval())
		defer ticker.Stop()
		q.scan()
		for {
			select {
			case <-ctx.Done():
				return
			case auth := <-q.changed:
				q.observe(auth)
			case <-ticker.C:
				q.scan()
			}
		}
	}()
}

func (q *quotaSnapshotSampler) Stop(ctx context.Context) {
	q.manager.SetQuotaObserver(nil)
	if q.cancel == nil {
		return
	}
	q.cancel()
	select {
	case <-q.done:
	case <-ctx.Done():
	}
}

// notify never blocks the request path. A dropped notification is recovered by
// the next periodic scan, which reads the same observation from the manager.
func (q *quotaSnapshotSampler) notify(auth *coreauth.Auth) {
	select {
	case q.changed <- auth:
	default:
	}
}

func (q *quotaSnapshotSampler) scan() {
	for _, auth := range q.manager.List() {
		q.observe(auth)
	}
}

// probeLoop refreshes plan, manual reset counts and (for xAI) usage windows from
// the upstream endpoints the management panel uses. Probes are spaced per
// credential and back off on failure.
func (q *quotaSnapshotSampler) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		q.probeDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (q *quotaSnapshotSampler) probeDue(ctx context.Context) {
	for _, auth := range q.manager.List() {
		if ctx.Err() != nil {
			return
		}
		if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled || !quotaprobe.Supported(auth.Provider) {
			continue
		}
		q.mu.Lock()
		state := q.probes[auth.ID]
		due := state == nil || !time.Now().Before(state.nextAt)
		q.mu.Unlock()
		if !due {
			continue
		}
		result, err := quotaprobe.Probe(ctx, q.config(), auth)
		q.mu.Lock()
		if state == nil {
			state = &quotaProbeState{}
			q.probes[auth.ID] = state
		}
		if err != nil {
			state.backoff = min(max(2*state.backoff, quotaProbeInterval), quotaProbeMaxBackoff)
			state.nextAt = time.Now().Add(state.backoff)
			log.WithFields(log.Fields{"provider": auth.Provider, "error": err.Error()}).Debug("quota probe failed")
		} else {
			state.result, state.backoff, state.nextAt = result, 0, time.Now().Add(quotaProbeInterval)
		}
		q.mu.Unlock()
		if err == nil {
			q.observe(auth)
		}
	}
}

type quotaWindowObservation struct {
	usage      coreauth.QuotaWindowUsage
	observedAt time.Time
}

func (q *quotaSnapshotSampler) observe(auth *coreauth.Auth) {
	if auth == nil || auth.ID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var probe *quotaprobe.Result
	if state := q.probes[auth.ID]; state != nil {
		probe = state.result
	}

	// Header-derived windows come from the passive observation; a probe window
	// is used when there is no header window or the probe is newer.
	byWindow := map[string]quotaWindowObservation{}
	var order []string
	if !auth.Quota.ObservedAt.IsZero() {
		for _, window := range auth.QuotaWindows() {
			byWindow[window.Window] = quotaWindowObservation{window, auth.Quota.ObservedAt}
			order = append(order, window.Window)
		}
	}
	if probe != nil {
		for _, window := range probe.Windows {
			existing, ok := byWindow[window.Window]
			if !ok {
				order = append(order, window.Window)
			}
			if !ok || probe.ObservedAt.After(existing.observedAt) {
				byWindow[window.Window] = quotaWindowObservation{window, probe.ObservedAt}
			}
		}
	}
	if len(order) == 0 {
		return
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	accountID := usage.AccountingAccountID(auth.ID)
	planName := auth.QuotaPlan()
	var manual *int
	if probe != nil {
		if probe.Plan != "" {
			planName = coreauth.DisplayQuotaPlan(probe.Plan)
		}
		manual = probe.ManualResetsLeft
	}
	manualMark := -1
	if manual != nil {
		manualMark = *manual
	}
	var plan, price *string
	if planName != "" {
		plan = &planName
		price = q.planPrice(provider, planName)
	}
	metadata := planName + "|" + strconv.Itoa(manualMark)
	if price != nil {
		metadata += "|" + *price
	}

	var rows []usage.QuotaSnapshot
	marks := make([]quotaSnapshotMark, 0, len(order))
	keys := make([]string, 0, len(order))
	for _, name := range order {
		observation := byWindow[name]
		window, observedAt := observation.usage, observation.observedAt
		key := auth.ID + "|" + window.Window
		mark := quotaSnapshotMark{utilization: formatQuotaUtilization(window.Utilization), resetsAt: window.ResetsAt.Truncate(time.Second), observedAt: observedAt, manual: manualMark, metadata: metadata}
		previous, seen := q.last[key]
		if seen {
			// A probe can learn plan, price or manual resets without a newer usage
			// observation; such a change is exported with the probe time.
			metadataChanged := previous.metadata != mark.metadata
			if metadataChanged && probe != nil && probe.ObservedAt.After(observedAt) {
				observedAt = probe.ObservedAt
				mark.observedAt = observedAt
			}
			if !observedAt.After(previous.observedAt) {
				continue
			}
			unchanged := !metadataChanged && previous.utilization == mark.utilization && quotaResetsClose(previous.resetsAt, mark.resetsAt)
			if unchanged && observedAt.Sub(previous.observedAt) < q.cfg.QuotaSnapshotInterval() {
				continue
			}
		}
		row := usage.QuotaSnapshot{
			Source: usage.QuotaSnapshotSource, ObservedAt: observedAt, Provider: provider, AccountID: accountID,
			Plan: plan, PlanPriceUSD: price, Window: window.Window, Utilization: mark.utilization,
		}
		if !window.ResetsAt.IsZero() {
			resets := window.ResetsAt
			row.ResetsAt = &resets
		}
		if manual != nil {
			value := *manual
			row.ManualResetsLeft = &value
		}
		rows = append(rows, row)
		marks = append(marks, mark)
		keys = append(keys, key)
	}
	if len(rows) == 0 {
		return
	}
	if err := q.outbox.EnqueueQuotaSnapshots(rows); err != nil {
		// Marks stay untouched so the next scan retries; nothing is dropped silently.
		if q.failures++; q.failures == 1 || q.failures%100 == 0 {
			log.WithError(err).WithField("failures", q.failures).Warn("quota snapshot persistence failed, will retry")
		}
		return
	}
	q.failures = 0
	for i, key := range keys {
		q.last[key] = marks[i]
	}
}

// planPrice looks up "provider:plan" first, then "plan", case-insensitively.
func (q *quotaSnapshotSampler) planPrice(provider, plan string) *string {
	for _, key := range []string{provider + ":" + plan, plan} {
		for name, price := range q.cfg.PlanPricesUSD {
			if strings.EqualFold(strings.TrimSpace(name), key) {
				value := price
				return &value
			}
		}
	}
	return nil
}

func formatQuotaUtilization(value float64) string {
	text := strconv.FormatFloat(value, 'f', 6, 64)
	text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	if text == "" {
		return "0"
	}
	return text
}

// quotaResetsClose tolerates the drift of reset times derived from
// reset-after-seconds headers, which shift with every observation.
func quotaResetsClose(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() && b.IsZero()
	}
	diff := a.Sub(b)
	return diff > -time.Minute && diff < time.Minute
}
