package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	log "github.com/sirupsen/logrus"
)

const (
	modelPricesDatasetName = "llm_model_prices"
	modelPricesImplements  = "llm_prices"
	modelPricesBatchSize   = 500
	modelPricesRetryDelay  = time.Minute
	modelPricesHoldDelay   = time.Hour
)

var modelPricesBucket = []byte("model-prices-v1")

func ModelPricesDataset(attemptsDataset string) string {
	owner, _, _ := strings.Cut(attemptsDataset, "/")
	return owner + "/" + modelPricesDatasetName
}

type analyticsModelPriceRow struct {
	Provider             string    `json:"provider"`
	Model                string    `json:"model"`
	ServiceTier          string    `json:"service_tier"`
	MinContextTokens     int64     `json:"min_context_tokens"`
	EffectiveAt          time.Time `json:"effective_at"`
	MaxContextTokens     *int64    `json:"max_context_tokens"`
	InputUSDPerMTok      *string   `json:"input_usd_per_mtok"`
	CacheReadUSDPerMTok  *string   `json:"cache_read_usd_per_mtok"`
	CacheWriteUSDPerMTok *string   `json:"cache_write_usd_per_mtok"`
	OutputUSDPerMTok     *string   `json:"output_usd_per_mtok"`
	ReasoningUSDPerMTok  *string   `json:"reasoning_usd_per_mtok"`
	Source               *string   `json:"source"`
}

func (r analyticsModelPriceRow) identity() string {
	return strings.Join([]string{r.Provider, r.Model, r.ServiceTier, strconv.FormatInt(r.MinContextTokens, 10)}, "\x00")
}

func (r analyticsModelPriceRow) fingerprint() string {
	r.EffectiveAt = time.Time{}
	raw, _ := json.Marshal(r)
	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:])
}

func newAnalyticsModelPriceRow(price modelPrice) (analyticsModelPriceRow, error) {
	rate := price.rate
	row := analyticsModelPriceRow{
		Provider: rate.Provider, Model: rate.Model, ServiceTier: rate.Tier,
		MinContextTokens: rate.MinContext, Source: nullableString(price.source),
	}
	if rate.MaxContext > 0 {
		row.MaxContextTokens = &rate.MaxContext
	}

	for _, bucket := range []struct {
		rate   *string
		target **string
	}{
		{rate.Input, &row.InputUSDPerMTok},
		{rate.CacheRead, &row.CacheReadUSDPerMTok},
		{rate.CacheCreation, &row.CacheWriteUSDPerMTok},
		{rate.Output, &row.OutputUSDPerMTok},
		{rate.Reasoning, &row.ReasoningUSDPerMTok},
	} {
		value, err := analyticsRatePerMillion(bucket.rate)
		if err != nil {
			return analyticsModelPriceRow{}, err
		}
		*bucket.target = value
	}

	return row, nil
}

func (b *PriceBook) analyticsModelPrices() []analyticsModelPriceRow {
	var rows []analyticsModelPriceRow
	for _, price := range b.modelPrices() {
		if price.rate.Currency != "USD" {
			continue
		}

		row, err := newAnalyticsModelPriceRow(price)
		if err != nil {
			log.WithError(err).Warnf("skipping price of %s/%s: not exportable", price.rate.Provider, price.rate.Model)
			continue
		}
		rows = append(rows, row)
	}

	return rows
}

type modelPriceState struct {
	Fingerprint  string    `json:"fingerprint"`
	EffectiveAt  time.Time `json:"effective_at"`
	Acknowledged bool      `json:"acknowledged"`
}

func (o *Outbox) planModelPrices(rows []analyticsModelPriceRow, now time.Time) ([]analyticsModelPriceRow, error) {
	var pending []analyticsModelPriceRow
	err := o.update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(modelPricesBucket)
		for _, row := range rows {
			key := []byte(row.identity())
			fingerprint := row.fingerprint()

			var state modelPriceState
			known := false
			if raw := bucket.Get(key); raw != nil {
				if err := json.Unmarshal(raw, &state); err != nil {
					return err
				}
				known = true
			}

			switch {
			case !known:
				state = modelPriceState{Fingerprint: fingerprint, EffectiveAt: time.Unix(0, 0).UTC()}
			case state.Fingerprint != fingerprint:
				state = modelPriceState{Fingerprint: fingerprint, EffectiveAt: now.UTC()}
			case state.Acknowledged:
				continue
			}

			if err := o.checkDisk(tx); err != nil {
				return err
			}
			raw, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err := bucket.Put(key, raw); err != nil {
				return err
			}

			row.EffectiveAt = state.EffectiveAt
			pending = append(pending, row)
		}

		return nil
	})

	return pending, err
}

func (o *Outbox) ackModelPrices(rows []analyticsModelPriceRow) error {
	return o.update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(modelPricesBucket)
		for _, row := range rows {
			key := []byte(row.identity())

			var state modelPriceState
			if err := json.Unmarshal(bucket.Get(key), &state); err != nil {
				return err
			}

			state.Acknowledged = true
			updated, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err := bucket.Put(key, updated); err != nil {
				return err
			}
		}

		return nil
	})
}

func (e *AnalyticsExporter) publishModelPrices(ctx context.Context) error {
	book := e.outbox.prices.Load()
	now := e.outbox.now()
	if !e.cfg.ModelPrices.Enabled || book == nil || book.snapshot == e.publishedPrices || now.Before(e.modelPricesRetryAt) {
		return nil
	}

	pending, err := e.outbox.planModelPrices(book.analyticsModelPrices(), now)
	if err != nil {
		return err
	}

	dataset := ModelPricesDataset(e.cfg.Dataset)
	for start := 0; start < len(pending); start += modelPricesBatchSize {
		batch := pending[start:min(start+modelPricesBatchSize, len(pending))]

		var body bytes.Buffer
		encoder := json.NewEncoder(&body)
		for _, row := range batch {
			if err := encoder.Encode(row); err != nil {
				return err
			}
		}

		state, hold := e.sendDataset(ctx, dataset, body.Bytes(), len(batch))
		if e.Status() == "dataset_or_route_missing" && e.registerDataset(ctx, dataset, modelPricesImplements) {
			state, hold = e.sendDataset(ctx, dataset, body.Bytes(), len(batch))
		}
		if state != DeliveryAcknowledged {
			e.modelPricesRetryAt = now.Add(modelPricesRetryDelay)
			if hold {
				e.modelPricesRetryAt = now.Add(modelPricesHoldDelay)
			}

			return nil
		}

		if err := e.outbox.ackModelPrices(batch); err != nil {
			return err
		}
	}

	e.publishedPrices = book.snapshot

	return nil
}

func (e *AnalyticsExporter) priceKeyOf(event AccountingEvent) PriceKey {
	key, found := e.outbox.prices.Load().Key(event)
	if !found && event.Tokens != nil && key.Model != "" {
		if _, warned := e.warnedMissingPrices.LoadOrStore(key.Provider+"/"+key.Model, true); !warned {
			log.Warnf("no price for %s/%s: its spend stays empty until the price is added", key.Provider, key.Model)
		}
	}

	return key
}

func analyticsRatePerMillion(rate *string) (*string, error) {
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
	if !ok || exact.Cmp(value) != 0 || !analyticsAmountPattern.MatchString(decimal) {
		return nil, fmt.Errorf("price rate cannot be represented as Decimal(24,12) per million tokens")
	}

	decimal = strings.TrimRight(strings.TrimRight(decimal, "0"), ".")

	return &decimal, nil
}
