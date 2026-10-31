package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

type AnalyticsExporter struct {
	deliveries deliveryQueue

	outbox   *Outbox
	cfg      config.AnalyticsExporterConfig
	client   *http.Client
	token    string
	mu       sync.RWMutex
	exportMu sync.Mutex
	// quotaRegistered is guarded by exportMu.
	quotaRegistered bool
	status          string
	cancel          context.CancelFunc
	done            chan struct{}
}

func NewAnalyticsExporter(outbox *Outbox, cfg config.AnalyticsExporterConfig) *AnalyticsExporter {
	// A redirect must not forward credentials or change ingestion POST semantics.
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{}).DialContext, ForceAttemptHTTP2: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &AnalyticsExporter{deliveries: outbox.deliveriesFor("analytics"), outbox: outbox, cfg: cfg, client: client, token: os.Getenv(cfg.TokenEnv), status: "disabled"}
}

func (e *AnalyticsExporter) Status() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.status
}

func (e *AnalyticsExporter) setStatus(status string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status != status {
		log.WithField("accounting_exporter_status", status).Infof("Analytics accounting status: %s", status)
	}
	e.status = status
}

func (e *AnalyticsExporter) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan struct{})
	go func() {
		defer close(e.done)
		if !e.cfg.Enabled {
			return
		}
		if e.outbox.db == nil {
			e.setStatus("local_store_unavailable")
			return
		}
		if e.cfg.TokenEnv != "" && e.token == "" {
			e.setStatus("credential_unavailable")
			return
		}

		e.setStatus("ready")
		delay := time.Second
		for {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				if err := e.ExportOnce(ctx); err != nil {
					e.setStatus("local_store_failure")
				}
				if e.Status() == "exported" || e.Status() == "ready" {
					delay = time.Second
				} else {
					delay = min(delay*2, time.Minute)
				}
			}
		}
	}()
}

func (e *AnalyticsExporter) Stop(ctx context.Context) error {
	if e.cancel == nil {
		return nil
	}
	e.cancel()
	e.client.CloseIdleConnections()
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ExportOnce retries uncertain outcomes safely because Analytics replaces rows by their stable attempt key.
func (e *AnalyticsExporter) ExportOnce(ctx context.Context) error {
	e.exportMu.Lock()
	defer e.exportMu.Unlock()
	if !e.cfg.Enabled {
		return nil
	}
	if e.cfg.TokenEnv != "" && e.token == "" {
		e.setStatus("credential_unavailable")
		return nil
	}
	if err := e.exportAttempts(ctx); err != nil {
		return err
	}
	if e.cfg.QuotaSnapshots.Enabled {
		return e.exportQuotaSnapshots(ctx)
	}
	return nil
}

func (e *AnalyticsExporter) exportAttempts(ctx context.Context) error {
	ids, err := e.deliveries.ReadyIDs(64)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	claimed := make(map[string]uint64)

	var pricesBody bytes.Buffer
	pricesEncoder := json.NewEncoder(&pricesBody)
	priceIDs := make(map[string]bool)

	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		ok, err := e.deliveries.Claim(id)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		delivery, err := e.deliveries.Delivery(id)
		if err != nil {
			return err
		}
		event, err := e.outbox.Event(id)
		if err != nil {
			return err
		}
		row, errRow := newAnalyticsRow(event)
		if e.cfg.ExportProject {
			row.Project = nullableString(event.Project)
		}
		var price *analyticsPriceRow
		var errPrice error
		if errRow == nil && e.cfg.PricesDataset != "" {
			price, errPrice = newAnalyticsPriceRow(event.Estimate)
		}
		if errRow != nil {
			e.setStatus("invalid_event")
			if err := e.deliveries.Resolve(id, delivery.Attempts, DeliveryRejected, time.Time{}); err != nil {
				return err
			}
			continue
		}
		if errPrice != nil {
			e.setStatus("invalid_price_record")
			if err := e.deliveries.Resolve(id, delivery.Attempts, DeliveryRetryable, e.outbox.now().Add(time.Hour)); err != nil {
				return err
			}
			continue
		}
		if price != nil {
			row.PriceID = &price.PriceID
			if !priceIDs[price.PriceID] {
				if err := pricesEncoder.Encode(price); err != nil {
					return err
				}
				priceIDs[price.PriceID] = true
			}
		}
		if err := encoder.Encode(row); err != nil {
			return err
		}
		claimed[id] = delivery.Attempts
	}
	if len(claimed) == 0 {
		return nil
	}

	state, hold := DeliveryAcknowledged, false
	if len(priceIDs) > 0 {
		state, hold = e.sendDataset(ctx, e.cfg.PricesDataset, pricesBody.Bytes(), len(priceIDs))
	}
	if state == DeliveryAcknowledged {
		state, hold = e.send(ctx, body.Bytes(), len(claimed))
	}
	for id, attempt := range claimed {
		delay := min(time.Second*time.Duration(1<<min(attempt, uint64(6))), time.Minute)
		if hold {
			delay = time.Hour
		}
		if err := e.deliveries.Resolve(id, attempt, state, e.outbox.now().Add(delay)); err != nil {
			return err
		}
	}
	return nil
}

func (e *AnalyticsExporter) send(ctx context.Context, body []byte, rows int) (DeliveryState, bool) {
	return e.sendDataset(ctx, e.cfg.Dataset, body, rows)
}

func (e *AnalyticsExporter) sendDataset(ctx context.Context, dataset string, body []byte, rows int) (DeliveryState, bool) {
	endpoint := strings.TrimRight(e.cfg.URL, "/") + "/api/datasets/" + dataset + "/rows"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		e.setStatus("invalid_configuration")
		return DeliveryRetryable, true
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.setStatus("receiver_unavailable")
		return DeliveryRetryable, false
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
		var result struct {
			Rows int `json:"rows"`
		}
		if err != nil || len(raw) > 1024*1024 || json.Unmarshal(raw, &result) != nil || result.Rows != rows {
			e.setStatus("response_invalid")
			return DeliveryRetryable, false
		}
		e.setStatus("exported")
		return DeliveryAcknowledged, false
	case http.StatusUnauthorized, http.StatusForbidden:
		e.setStatus("authorization_failure")
		return DeliveryRetryable, true
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		e.setStatus("dataset_or_route_missing")
		return DeliveryRetryable, true
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		e.setStatus("schema_failure")
		return DeliveryRetryable, true
	case http.StatusTooManyRequests:
		e.setStatus("rate_limited")
		return DeliveryRetryable, false
	default:
		e.setStatus("service_failure")
		return DeliveryRetryable, false
	}
}
