package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	quotaSnapshotsDatasetName = "llm_quota_snapshots"
	quotaSnapshotsImplements  = "llm_usage_quota_snapshots"
	quotaSnapshotsBatchSize   = 256

	subscriptionCostsDatasetName = "llm_subscription_costs"
	subscriptionCostsImplements  = "llm_usage_subscription_costs"
)

type SubscriptionCost struct {
	Source    string    `json:"source"`
	Provider  string    `json:"provider"`
	AccountID string    `json:"account_id"`
	Plan      *string   `json:"plan,omitempty"`
	Month     time.Time `json:"month"`
	USD       string    `json:"usd"`
}

func QuotaSnapshotsDataset(attemptsDataset string) string {
	return siblingDataset(attemptsDataset, quotaSnapshotsDatasetName)
}

func SubscriptionCostsDataset(attemptsDataset string) string {
	return siblingDataset(attemptsDataset, subscriptionCostsDatasetName)
}

func siblingDataset(attemptsDataset, name string) string {
	owner, _, _ := strings.Cut(attemptsDataset, "/")
	return owner + "/" + name
}

func subscriptionCosts(snapshots []QuotaSnapshot) []SubscriptionCost {
	var costs []SubscriptionCost
	at := map[string]int{}
	for _, snapshot := range snapshots {
		if !paid(snapshot.PlanPriceUSD) {
			continue
		}

		observed := snapshot.ObservedAt.UTC()
		cost := SubscriptionCost{
			Source:    snapshot.Source,
			Provider:  snapshot.Provider,
			AccountID: snapshot.AccountID,
			Plan:      snapshot.Plan,
			Month:     time.Date(observed.Year(), observed.Month(), 1, 0, 0, 0, 0, time.UTC),
			USD:       *snapshot.PlanPriceUSD,
		}
		key := cost.Provider + "|" + cost.AccountID + "|" + cost.Month.Format(time.RFC3339)

		if index, seen := at[key]; seen {
			costs[index] = cost
			continue
		}
		at[key] = len(costs)
		costs = append(costs, cost)
	}
	return costs
}

func paid(price *string) bool {
	if price == nil {
		return false
	}
	amount, err := strconv.ParseFloat(*price, 64)
	return err == nil && amount > 0
}

func (e *AnalyticsExporter) exportQuotaSnapshots(ctx context.Context) error {
	keys, rows, err := e.outbox.pendingQuotaSnapshots(quotaSnapshotsBatchSize)
	if err != nil || len(rows) == 0 {
		return err
	}

	state, err := deliverRows(ctx, e, QuotaSnapshotsDataset(e.cfg.Dataset), quotaSnapshotsImplements, rows)
	if err != nil || state != DeliveryAcknowledged {
		return err
	}
	if err := e.outbox.ackQuotaSnapshots(keys, rows); err != nil {
		return err
	}

	costs := subscriptionCosts(rows)
	if len(costs) == 0 {
		return nil
	}
	_, err = deliverRows(ctx, e, SubscriptionCostsDataset(e.cfg.Dataset), subscriptionCostsImplements, costs)
	return err
}

func deliverRows[T any](ctx context.Context, e *AnalyticsExporter, dataset, implements string, rows []T) (DeliveryState, error) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return DeliveryRetryable, err
		}
	}

	state, _ := e.sendDataset(ctx, dataset, body.Bytes(), len(rows))
	if e.Status() == "dataset_or_route_missing" && e.registerDataset(ctx, dataset, implements) {
		state, _ = e.sendDataset(ctx, dataset, body.Bytes(), len(rows))
	}
	return state, nil
}

func (e *AnalyticsExporter) registerDataset(ctx context.Context, id, implements string) bool {
	payload, err := json.Marshal(map[string]string{"id": id, "implements": implements})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.cfg.URL, "/")+"/api/datasets", bytes.NewReader(payload))
	if err != nil {
		e.setStatus("invalid_configuration")
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.setStatus("receiver_unavailable")
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024*1024))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300, resp.StatusCode == http.StatusConflict:
		return true
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		e.setStatus("authorization_failure")
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity:
		e.setStatus("schema_failure")
	default:
		e.setStatus("service_failure")
	}
	return false
}
