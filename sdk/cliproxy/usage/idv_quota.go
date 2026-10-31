package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const (
	quotaSnapshotsDatasetName = "llm_quota_snapshots"
	quotaSnapshotsImplements  = "llm_usage@0.1/quota_snapshots"
	quotaSnapshotsBatchSize   = 256
)

// QuotaSnapshotsDataset puts snapshots under the attempts dataset's owner.
func QuotaSnapshotsDataset(attemptsDataset string) string {
	owner, _, _ := strings.Cut(attemptsDataset, "/")
	return owner + "/" + quotaSnapshotsDatasetName
}

// exportQuotaSnapshots deletes rows only after the receiver acks the exact count.
// IDV replaces rows by key, so resending is safe.
func (e *IDVExporter) exportQuotaSnapshots(ctx context.Context) error {
	if !e.quotaRegistered && !e.registerQuotaDataset(ctx) {
		return nil
	}
	keys, rows, err := e.outbox.pendingQuotaSnapshots(quotaSnapshotsBatchSize)
	if err != nil || len(rows) == 0 {
		return err
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	state, _ := e.sendDataset(ctx, QuotaSnapshotsDataset(e.cfg.Dataset), body.Bytes(), len(rows))
	if state == DeliveryAcknowledged {
		return e.outbox.ackQuotaSnapshots(keys, rows)
	}
	if e.Status() == "dataset_or_route_missing" {
		e.quotaRegistered = false
	}
	return nil
}

// registerQuotaDataset creates the dataset; 409 means it already exists.
func (e *IDVExporter) registerQuotaDataset(ctx context.Context) bool {
	payload, err := json.Marshal(map[string]string{"id": QuotaSnapshotsDataset(e.cfg.Dataset), "implements": quotaSnapshotsImplements})
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
		e.quotaRegistered = true
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
