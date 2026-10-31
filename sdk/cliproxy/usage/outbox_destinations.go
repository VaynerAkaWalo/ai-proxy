package usage

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

var secondaryDeliveriesBucket = []byte("deliveries-secondary-v1")
var destinationsBucket = []byte("destinations-v1")
var primaryDestinationKey = []byte("primary")

type deliveryQueue struct {
	outbox *Outbox
	bucket []byte
}

func (o *Outbox) destinationEnabled(destination string) bool {
	if destination == "analytics" {
		return o.cfg.Analytics.Enabled
	}
	return o.cfg.LiteLLM.Enabled
}

func (o *Outbox) secondaryDestination() string {
	if o.primaryDestination == "analytics" {
		return "litellm"
	}
	return "analytics"
}

func (o *Outbox) deliveriesFor(destination string) deliveryQueue {
	bucket := deliveriesBucket
	if o.secondaryDeliveries && destination != o.primaryDestination {
		bucket = secondaryDeliveriesBucket
	}
	return deliveryQueue{outbox: o, bucket: bucket}
}

func (q deliveryQueue) ReadyIDs(limit int) ([]string, error) {
	return q.outbox.readyIDs(q.bucket, limit)
}

func (q deliveryQueue) Claim(id string) (bool, error) {
	return q.outbox.claim(q.bucket, id)
}

func (q deliveryQueue) Delivery(id string) (Delivery, error) {
	return q.outbox.delivery(q.bucket, id)
}

func (q deliveryQueue) Resolve(id string, attempt uint64, state DeliveryState, next time.Time) error {
	return q.outbox.resolve(q.bucket, id, attempt, state, next)
}

func deliveryTerminal(delivery Delivery) bool {
	return delivery.State == DeliveryAcknowledged || delivery.State == DeliveryRejected
}

func putDelivery(bucket *bolt.Bucket, key []byte, delivery Delivery) error {
	raw, err := json.Marshal(delivery)
	if err != nil {
		return err
	}
	return bucket.Put(key, raw)
}

func secondaryDelivery(tx *bolt.Tx, key []byte) (*Delivery, error) {
	bucket := tx.Bucket(secondaryDeliveriesBucket)
	if bucket == nil || bucket.Get(key) == nil {
		return nil, nil
	}
	var delivery Delivery
	if err := json.Unmarshal(bucket.Get(key), &delivery); err != nil {
		return nil, err
	}
	return &delivery, nil
}

func (o *Outbox) initializeDestinations(tx *bolt.Tx) error {
	metadata, err := tx.CreateBucketIfNotExists(destinationsBucket)
	if err != nil {
		return err
	}
	primary := string(metadata.Get(primaryDestinationKey))
	if primary == "" {
		primary = "litellm"
		if o.cfg.Analytics.Enabled && !o.cfg.LiteLLM.Enabled {
			primary = "analytics"
		}
		if err := metadata.Put(primaryDestinationKey, []byte(primary)); err != nil {
			return err
		}
	}
	if primary != "litellm" && primary != "analytics" {
		return errors.New("invalid primary accounting destination")
	}
	o.primaryDestination = primary

	bucket := tx.Bucket(secondaryDeliveriesBucket)
	if bucket == nil && !o.destinationEnabled(o.secondaryDestination()) {
		return nil
	}
	if bucket == nil {
		bucket, err = tx.CreateBucketIfNotExists(secondaryDeliveriesBucket)
		if err != nil {
			return err
		}
	}
	o.secondaryDeliveries = true
	if !o.destinationEnabled(o.secondaryDestination()) {
		return nil
	}

	return tx.Bucket(deliveriesBucket).ForEach(func(key, raw []byte) error {
		if bucket.Get(key) != nil {
			return nil
		}
		var delivery Delivery
		if err := json.Unmarshal(raw, &delivery); err != nil {
			return err
		}
		// Historical terminal events need a separate import, not an automatic replay.
		if deliveryTerminal(delivery) {
			return nil
		}
		if err := o.checkDisk(tx); err != nil {
			return err
		}
		return putDelivery(bucket, key, Delivery{State: DeliveryPending, UpdatedAt: o.now()})
	})
}
