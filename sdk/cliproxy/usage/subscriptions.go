package usage

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

const SubscriptionWindow = "subscription"

var subscriptionsBucket = []byte("subscriptions-v1")

type subscriptionState struct {
	Snapshot QuotaSnapshot `json:"snapshot"`
	Active   bool          `json:"active"`
}

func (o *Outbox) ReconcileSubscriptions(rows []QuotaSnapshot, observedAt time.Time) error {
	if o == nil || o.unavailable || o.db == nil {
		return errors.New("accounting store unavailable")
	}

	return o.update(func(tx *bolt.Tx) error {
		if err := o.checkDisk(tx); err != nil {
			return err
		}
		inventory := tx.Bucket(subscriptionsBucket)
		previous := make(map[string]subscriptionState)
		if err := inventory.ForEach(func(key, raw []byte) error {
			var state subscriptionState
			if err := json.Unmarshal(raw, &state); err != nil {
				return err
			}
			previous[string(key)] = state
			return nil
		}); err != nil {
			return err
		}

		current := make(map[string]subscriptionState, len(rows))
		for _, row := range rows {
			row.Source, row.Window, row.Utilization = QuotaSnapshotSource, SubscriptionWindow, "0"
			if row.Plan == nil {
				row.Plan = previous[row.AccountID].Snapshot.Plan
				row.PlanPriceUSD = previous[row.AccountID].Snapshot.PlanPriceUSD
			}
			current[row.AccountID] = subscriptionState{Snapshot: row, Active: true}
		}
		for id, state := range previous {
			if _, present := current[id]; present || !state.Active {
				continue
			}
			state.Active = false
			current[id] = state
		}

		for id, state := range current {
			at := observedAt.UTC().Truncate(time.Second)
			if before := previous[id].Snapshot.ObservedAt; !at.After(before) {
				at = before.Add(time.Second)
			}
			state.Snapshot.ObservedAt = at
			row := state.Snapshot
			if !state.Active {
				zero := "0"
				row.PlanPriceUSD = &zero
			}
			if err := enqueueQuotaSnapshot(tx.Bucket(quotaSnapshotsBucket), row); err != nil {
				return err
			}
			raw, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err := inventory.Put([]byte(id), raw); err != nil {
				return err
			}
		}
		return nil
	})
}
