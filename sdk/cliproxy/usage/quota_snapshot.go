package usage

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

// MaxQuotaSnapshots bounds undelivered snapshots so an unreachable receiver
// cannot grow the store without limit.
const MaxQuotaSnapshots = 20000

var quotaSnapshotsBucket = []byte("quota-snapshots-v1")

// QuotaSnapshot is one llm_usage@0.1/quota_snapshots row. Nullable fields are
// omitted when unknown because the receiver rejects unknown keys but accepts
// missing nullable ones.
type QuotaSnapshot struct {
	Source           string     `json:"source"`
	ObservedAt       time.Time  `json:"observed_at"`
	Provider         string     `json:"provider"`
	AccountID        string     `json:"account_id"`
	Plan             *string    `json:"plan,omitempty"`
	PlanPriceUSD     *string    `json:"plan_price_usd,omitempty"`
	Window           string     `json:"window"`
	Utilization      string     `json:"utilization"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
	ManualResetsLeft *int       `json:"manual_resets_left,omitempty"`
}

// QuotaSnapshotSource is the row source value.
const QuotaSnapshotSource = "ai_proxy"

func (s QuotaSnapshot) key() []byte {
	// Time first so the oldest undelivered snapshots drain first.
	return []byte(s.ObservedAt.UTC().Format("20060102T150405Z") + "|" + s.AccountID + "|" + s.Window)
}

func (s QuotaSnapshot) valid() bool {
	return s.Source != "" && !s.ObservedAt.IsZero() && s.Provider != "" && s.AccountID != "" && s.Window != "" && idvAmountPattern.MatchString(s.Utilization)
}

// EnqueueQuotaSnapshots durably stores snapshots for delivery. It returns only
// after the transaction is committed, and refuses rows rather than evicting
// undelivered ones when the store is full. Re-enqueuing a key replaces the row.
func (o *Outbox) EnqueueQuotaSnapshots(rows []QuotaSnapshot) error {
	if o == nil || o.unavailable || o.db == nil {
		return errors.New("accounting store unavailable")
	}
	if len(rows) == 0 {
		return nil
	}
	return o.update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(quotaSnapshotsBucket)
		if err := o.checkDisk(tx); err != nil {
			return err
		}
		for _, row := range rows {
			row.ObservedAt = row.ObservedAt.UTC().Truncate(time.Second)
			if row.ResetsAt != nil {
				resets := row.ResetsAt.UTC().Truncate(time.Second)
				row.ResetsAt = &resets
			}
			if !row.valid() {
				return errors.New("invalid quota snapshot")
			}
			raw, err := json.Marshal(row)
			if err != nil {
				return err
			}
			key := row.key()
			if bucket.Get(key) == nil {
				if bucket.Sequence() >= MaxQuotaSnapshots {
					return ErrOutboxFull
				}
				if err := bucket.SetSequence(bucket.Sequence() + 1); err != nil {
					return err
				}
			}
			if err := bucket.Put(key, raw); err != nil {
				return err
			}
		}
		return nil
	})
}

// pendingQuotaSnapshots returns up to limit undelivered snapshots, oldest first.
func (o *Outbox) pendingQuotaSnapshots(limit int) (keys [][]byte, rows []QuotaSnapshot, err error) {
	if o == nil || o.db == nil {
		return nil, nil, nil
	}
	err = o.view(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(quotaSnapshotsBucket)
		if bucket == nil {
			return nil
		}
		cursor := bucket.Cursor()
		for key, raw := cursor.First(); key != nil && len(keys) < limit; key, raw = cursor.Next() {
			var row QuotaSnapshot
			if err := json.Unmarshal(raw, &row); err != nil {
				return err
			}
			keys = append(keys, append([]byte(nil), key...))
			rows = append(rows, row)
		}
		return nil
	})
	return keys, rows, err
}

// ackQuotaSnapshots removes delivered snapshots. A key whose row was replaced
// after it was read is kept so the newer content is still delivered.
func (o *Outbox) ackQuotaSnapshots(keys [][]byte, rows []QuotaSnapshot) error {
	if len(keys) == 0 {
		return nil
	}
	return o.update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(quotaSnapshotsBucket)
		for i, key := range keys {
			current := bucket.Get(key)
			if current == nil {
				continue
			}
			sent, err := json.Marshal(rows[i])
			if err != nil {
				return err
			}
			if string(current) != string(sent) {
				continue
			}
			if err := bucket.Delete(key); err != nil {
				return err
			}
			if bucket.Sequence() > 0 {
				if err := bucket.SetSequence(bucket.Sequence() - 1); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
