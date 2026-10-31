package cliproxy

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
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

	changed chan *coreauth.Auth
	cancel  context.CancelFunc
	done    chan struct{}

	mu       sync.Mutex
	last     map[string]quotaSnapshotMark
	failures int
}

type quotaSnapshotMark struct {
	utilization string
	resetsAt    time.Time
	observedAt  time.Time
}

func newQuotaSnapshotSampler(outbox *usage.Outbox, manager *coreauth.Manager, cfg config.QuotaSnapshotsConfig) *quotaSnapshotSampler {
	return &quotaSnapshotSampler{outbox: outbox, manager: manager, cfg: cfg, changed: make(chan *coreauth.Auth, 256), last: make(map[string]quotaSnapshotMark)}
}

func (q *quotaSnapshotSampler) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel, q.done = cancel, make(chan struct{})
	q.manager.SetQuotaObserver(q.notify)
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

func (q *quotaSnapshotSampler) observe(auth *coreauth.Auth) {
	if auth == nil || auth.ID == "" || auth.Quota.ObservedAt.IsZero() {
		return
	}
	windows := auth.QuotaWindows()
	if len(windows) == 0 {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	accountID := usage.AccountingAccountID(auth.ID)
	observedAt := auth.Quota.ObservedAt
	var plan, price *string
	if value := auth.QuotaPlan(); value != "" {
		plan = &value
		price = q.planPrice(provider, value)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	var rows []usage.QuotaSnapshot
	marks := make([]quotaSnapshotMark, 0, len(windows))
	keys := make([]string, 0, len(windows))
	for _, window := range windows {
		key := auth.ID + "|" + window.Window
		mark := quotaSnapshotMark{utilization: formatQuotaUtilization(window.Utilization), resetsAt: window.ResetsAt.Truncate(time.Second), observedAt: observedAt}
		previous, seen := q.last[key]
		if seen {
			if !observedAt.After(previous.observedAt) {
				continue
			}
			unchanged := previous.utilization == mark.utilization && quotaResetsClose(previous.resetsAt, mark.resetsAt)
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
