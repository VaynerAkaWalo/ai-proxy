package auth

import (
	"context"
	"errors"
	"testing"
)

func TestInventoryIncludesStoredCredentialsBeforeLoadAndRuntimeCredentialsDuringPartialReads(t *testing.T) {
	ctx := context.Background()
	store := newMemoryAuthTestStore()
	if _, err := store.Save(ctx, &Auth{ID: "stored", Provider: "xai"}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, nil, nil)

	auths, err := manager.ListWithStored(ctx)
	if err != nil || len(auths) != 1 || auths[0].ID != "stored" {
		t.Fatalf("inventory before runtime load: auths=%v err=%v", auths, err)
	}
	if len(manager.List()) != 0 {
		t.Fatal("reading inventory changed runtime credentials")
	}
	if _, err := manager.Register(WithSkipPersist(ctx), &Auth{ID: "runtime", Provider: "xai"}); err != nil {
		t.Fatal(err)
	}
	auths, err = manager.ListWithStored(ctx)
	if err != nil || len(auths) != 2 {
		t.Fatalf("inventory must include stored and runtime credentials: auths=%v err=%v", auths, err)
	}
	if _, err := manager.Register(WithSkipPersist(ctx), &Auth{ID: "stored", Provider: "xai", Attributes: map[string]string{"plan": "SuperGrok"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "stored"); err != nil {
		t.Fatal(err)
	}

	auths, err = manager.ListWithStored(ctx)
	if err != nil || len(auths) != 2 {
		t.Fatalf("a partial stored read must not remove runtime credentials: auths=%v err=%v", auths, err)
	}
	for _, auth := range auths {
		if auth.ID == "stored" && auth.QuotaPlan() != "SuperGrok" {
			t.Fatalf("inventory lost runtime plan metadata: %+v", auth)
		}
	}
}

type failingInventoryStore struct {
	*memoryAuthTestStore
	err error
}

func (s *failingInventoryStore) List(ctx context.Context) ([]*Auth, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.memoryAuthTestStore.List(ctx)
}

func TestInventoryRecoversAfterInitialLoadFailureWithoutReloadingRuntime(t *testing.T) {
	ctx := context.Background()
	store := &failingInventoryStore{memoryAuthTestStore: newMemoryAuthTestStore(), err: errors.New("store unavailable")}
	if _, err := store.Save(ctx, &Auth{ID: "stored", Provider: "xai"}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, nil, nil)
	if err := manager.Load(ctx); !errors.Is(err, store.err) {
		t.Fatalf("expected failed initial load, got %v", err)
	}

	if _, err := manager.ListWithStored(ctx); !errors.Is(err, store.err) {
		t.Fatalf("a failed inventory read must not look empty: %v", err)
	}
	store.err = nil
	auths, err := manager.ListWithStored(ctx)
	if err != nil || len(auths) != 1 || auths[0].ID != "stored" {
		t.Fatalf("inventory did not recover: auths=%v err=%v", auths, err)
	}
	if len(manager.List()) != 0 {
		t.Fatal("inventory recovery reloaded runtime credentials")
	}
}
