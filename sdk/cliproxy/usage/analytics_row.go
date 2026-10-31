package usage

import (
	"errors"
	"regexp"
	"time"
)

type analyticsRow struct {
	InputTokens              *int64    `json:"input_tokens"`
	OutputTokens             *int64    `json:"output_tokens"`
	PriceID                  *string   `json:"price_id,omitempty"`
	Source                   string    `json:"source"`
	ExecutionID              string    `json:"execution_id"`
	TraceID                  *string   `json:"trace_id"`
	StartedAt                time.Time `json:"started_at"`
	CompletedAt              time.Time `json:"completed_at"`
	Provider                 *string   `json:"provider"`
	RequestedModel           *string   `json:"requested_model"`
	ExecutedModel            *string   `json:"executed_model"`
	ClientID                 *string   `json:"client_id"`
	AccountID                *string   `json:"account_id"`
	SessionID                *string   `json:"session_id"`
	Project                  *string   `json:"project,omitempty"`
	ServiceTier              *string   `json:"service_tier"`
	Status                   string    `json:"status"`
	StatusCode               *int      `json:"status_code"`
	InputUncachedTokens      *int64    `json:"input_uncached_tokens"`
	CacheReadTokens          *int64    `json:"cache_read_tokens"`
	CacheWriteTokens         *int64    `json:"cache_write_tokens"`
	OutputNonReasoningTokens *int64    `json:"output_non_reasoning_tokens"`
	ReasoningTokens          *int64    `json:"reasoning_tokens"`
	TotalTokens              *int64    `json:"total_tokens"`
	UnclassifiedTokens       *int64    `json:"unclassified_tokens"`
	UsageQuality             string    `json:"usage_quality"`
	CostUSD                  *string   `json:"cost_usd"`
	CostKind                 *string   `json:"cost_kind"`
	CostStatus               string    `json:"cost_status"`
	PricingSnapshot          *string   `json:"pricing_snapshot"`
}

var analyticsAmountPattern = regexp.MustCompile(`^(0|[1-9]\d{0,11})(\.\d{1,12})?$`)

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func analyticsTokenCount(value int64, knownZero bool) *int64 {
	if value < 0 || (value == 0 && !knownZero) {
		return nil
	}
	return &value
}

func newAnalyticsRow(event AccountingEvent) (analyticsRow, error) {
	if event.ExecutionID == "" || event.StartedAt.IsZero() || event.CompletedAt.Before(event.StartedAt) {
		return analyticsRow{}, errors.New("invalid attempt identity or timestamps")
	}
	if event.Status != "succeeded" && event.Status != "failed" && event.Status != "interrupted" {
		return analyticsRow{}, errors.New("invalid attempt status")
	}

	row := analyticsRow{
		Source: "ai_proxy", ExecutionID: event.ExecutionID,
		TraceID:   nullableString(event.TraceID),
		StartedAt: event.StartedAt.UTC(), CompletedAt: event.CompletedAt.UTC(),
		Provider: nullableString(event.Provider), RequestedModel: nullableString(event.RequestedAlias),
		ExecutedModel: nullableString(event.ExecutedModel), ClientID: nullableString(event.ClientKeyID),
		AccountID: nullableString(event.AccountID), SessionID: nullableString(event.SessionID),
		ServiceTier: nullableString(event.ReportedServiceTier), Status: event.Status,
		UsageQuality: "unknown", CostStatus: "unpriced",
	}
	if event.StatusCode != 0 {
		row.StatusCode = &event.StatusCode
	}
	if event.Tokens != nil {
		b := event.Tokens.Breakdown
		if b.Valid() {
			complete := b.Quality == TokenAccountingQualityComplete
			if complete {
				row.InputTokens = analyticsTokenCount(b.Input.TotalTokens, true)
				row.OutputTokens = analyticsTokenCount(b.Output.TotalTokens, true)
			}
			row.UsageQuality = string(b.Quality)
			row.InputUncachedTokens = analyticsTokenCount(b.Input.UncachedTokens, complete)
			row.CacheReadTokens = analyticsTokenCount(b.Input.CacheReadTokens, complete)
			row.CacheWriteTokens = analyticsTokenCount(b.Input.CacheWriteTokens, complete)
			row.OutputNonReasoningTokens = analyticsTokenCount(b.Output.NonReasoningTokens, complete)
			row.ReasoningTokens = analyticsTokenCount(b.Output.ReasoningTokens, complete)
			row.TotalTokens = analyticsTokenCount(b.TotalTokens, true)
			row.UnclassifiedTokens = analyticsTokenCount(b.UnclassifiedTokens, true)
		} else {
			row.UsageQuality = "inconsistent"
			row.TotalTokens = analyticsTokenCount(event.Tokens.Total, false)
		}
	}
	if estimate := event.Estimate; estimate != nil {
		row.CostKind = nullableString(estimate.Kind)
		row.PricingSnapshot = nullableString(estimate.Snapshot)
		if estimate.Currency == "USD" && (estimate.Status == "priced" || estimate.Status == "partial") && estimate.Amount != nil && analyticsAmountPattern.MatchString(*estimate.Amount) {
			row.CostUSD = estimate.Amount
			row.CostStatus = estimate.Status
		}
	}
	return row, nil
}
