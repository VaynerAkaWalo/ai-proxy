package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

type idvPriceRow struct {
	PriceID              string  `json:"price_id,omitempty"`
	Source               *string `json:"source"`
	Provider             string  `json:"provider"`
	Model                string  `json:"model"`
	ServiceTier          *string `json:"service_tier"`
	MinContextTokens     int64   `json:"min_context_tokens"`
	MaxContextTokens     *int64  `json:"max_context_tokens"`
	InputUSDPerMTok      *string `json:"input_usd_per_mtok"`
	CacheReadUSDPerMTok  *string `json:"cache_read_usd_per_mtok"`
	CacheWriteUSDPerMTok *string `json:"cache_write_usd_per_mtok"`
	OutputUSDPerMTok     *string `json:"output_usd_per_mtok"`
	ReasoningUSDPerMTok  *string `json:"reasoning_usd_per_mtok"`
	Rounding             string  `json:"rounding"`
}

func idvRatePerMillion(rate *string) (*string, error) {
	if rate == nil {
		return nil, nil
	}

	value, ok := decimalRate(*rate)
	if !ok {
		return nil, fmt.Errorf("invalid price rate")
	}
	value.Mul(value, big.NewRat(1000000, 1))
	decimal := value.FloatString(12)
	exact, ok := new(big.Rat).SetString(decimal)
	if !ok || exact.Cmp(value) != 0 || !idvAmountPattern.MatchString(decimal) {
		return nil, fmt.Errorf("price rate cannot be represented as Decimal(24,12) per million tokens")
	}

	decimal = strings.TrimRight(strings.TrimRight(decimal, "0"), ".")

	return &decimal, nil
}

func newIDVPriceRow(estimate *CostEstimate) (*idvPriceRow, error) {
	if estimate == nil || estimate.Rates == nil || estimate.Currency != "USD" {
		return nil, nil
	}
	rate := estimate.Rates
	if rate.Currency != "USD" || rate.Provider == "" || rate.Model == "" || rate.MinContext < 0 || rate.MaxContext < 0 || (rate.MaxContext > 0 && rate.MaxContext < rate.MinContext) || estimate.Rounding == "" {
		return nil, fmt.Errorf("invalid selected price")
	}

	row := &idvPriceRow{
		Source: nullableString(estimate.Source), Provider: rate.Provider, Model: rate.Model,
		ServiceTier: nullableString(rate.Tier), MinContextTokens: rate.MinContext,
		Rounding: estimate.Rounding,
	}
	if rate.MaxContext > 0 {
		row.MaxContextTokens = &rate.MaxContext
	}
	reasoning := rate.Reasoning
	if reasoning == nil {
		reasoning = rate.Output
	}
	for _, bucket := range []struct {
		rate   *string
		target **string
	}{
		{rate.Input, &row.InputUSDPerMTok},
		{rate.CacheRead, &row.CacheReadUSDPerMTok},
		{rate.CacheCreation, &row.CacheWriteUSDPerMTok},
		{rate.Output, &row.OutputUSDPerMTok},
		{reasoning, &row.ReasoningUSDPerMTok},
	} {
		value, err := idvRatePerMillion(bucket.rate)
		if err != nil {
			return nil, err
		}
		*bucket.target = value
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	row.PriceID = hex.EncodeToString(hash[:])

	return row, nil
}
