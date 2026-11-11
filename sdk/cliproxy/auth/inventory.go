package auth

import (
	"context"
	"math"
)

func (m *Manager) ListWithStored(ctx context.Context) ([]*Auth, error) {
	if err := m.authLoadGate.Acquire(ctx, math.MaxInt64); err != nil {
		return nil, err
	}
	defer m.authLoadGate.Release(math.MaxInt64)

	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return m.List(), nil
	}
	stored, err := store.List(ctx)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]*Auth, len(stored))
	for _, auth := range stored {
		if auth != nil && auth.ID != "" {
			byID[auth.ID] = auth.Clone()
		}
	}
	for _, auth := range m.List() {
		byID[auth.ID] = auth
	}
	auths := make([]*Auth, 0, len(byID))
	for _, auth := range byID {
		auths = append(auths, auth)
	}
	return auths, nil
}
