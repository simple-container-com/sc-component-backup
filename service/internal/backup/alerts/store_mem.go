// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package alerts

import (
	"context"
	"sync"
	"time"
)

// InMemoryDismissals is the Dismissals impl used in tests + when the
// service runs with BACKUP_PROJECTS_MODE unset (local dev MVP; the
// production mode is Mongo). Dismissals don't survive a Lambda cold
// start — which is the right behavior in local dev and a documented
// trade-off until the Mongo impl takes over.
type InMemoryDismissals struct {
	mu     sync.RWMutex
	ids    map[int64]struct{}
	before time.Time
}

// NewInMemoryDismissals returns a ready-to-use empty store.
func NewInMemoryDismissals() *InMemoryDismissals {
	return &InMemoryDismissals{ids: map[int64]struct{}{}}
}

// Dismiss records one alert id. Idempotent.
func (s *InMemoryDismissals) Dismiss(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids[id] = struct{}{}
	return nil
}

// DismissAll advances the before-cutoff. Moving it backward is a
// no-op (see interface doc).
func (s *InMemoryDismissals) DismissAll(_ context.Context, before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if before.After(s.before) {
		s.before = before
	}
	return nil
}

// State returns a defensive copy — callers mutate freely without
// touching the store.
func (s *InMemoryDismissals) State(_ context.Context) (State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make(map[int64]struct{}, len(s.ids))
	for id := range s.ids {
		ids[id] = struct{}{}
	}
	return State{IDs: ids, Before: s.before}, nil
}
