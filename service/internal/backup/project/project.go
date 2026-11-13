// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package project — operator-edited project config, persisted separately
// from the plugin-driven tenant.Record.
//
// Slice 1e.3 of sc-components-platform. The SPA's NewProject / Settings
// pages produce a ProjectConfig the operator wants to persist; the
// plugin's push-register call produces a tenant.Record that describes
// the deployed consumer's resource surface. These are two concerns:
//
//   - tenant.Record  — immutable from an operator POV, set by the plugin
//     on each deploy, says "this consumer exists and
//     exposes these DB resources".
//   - ProjectConfig  — operator-editable, says "back up this consumer
//     (or an ad-hoc URI) on this schedule with this
//     retention". Carries the secret URI for engines
//     that need direct connection (ad-hoc projects
//     without a sibling tenant.Record).
//
// A ProjectConfig row CAN exist without a matching tenant.Record (an
// operator typing in a one-off backup URI); a tenant.Record can exist
// without a ProjectConfig (a deployed consumer that hasn't been opted
// into backup yet). The GET /api/v1/projects/{name} handler merges both
// — tenant.Record gives engine + databases, ProjectConfig overrides with
// interval/retention/etc.
package project

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Config mirrors service/internal/ui/src/api/types.ts:153 ProjectConfig
// with bson tags added for Mongo persistence. A Revision + timestamps
// ride alongside for optimistic concurrency + audit.
//
// SecretURI carries the DB connection URI for ad-hoc projects (no
// matching tenant.Record). Stored plaintext in v1 — MVP trade-off, see
// the TODO on SetSecret below. Not surfaced via GET.
type Config struct {
	Name              string    `bson:"_id" json:"name"`
	Engine            string    `bson:"engine" json:"engine"`
	Databases         any       `bson:"databases" json:"databases"` // "all" | []string
	Interval          string    `bson:"interval" json:"interval"`
	Timeout           string    `bson:"timeout" json:"timeout"`
	Retention         Retention `bson:"retention" json:"retention"`
	Retries           int       `bson:"retries" json:"retries"`
	ShrinkWarnPercent int       `bson:"shrinkWarnPercent" json:"shrink_warn_percent"`
	DumpArgs          []string  `bson:"dumpArgs" json:"dump_args"`
	Globals           bool      `bson:"globals" json:"globals"`
	StartAt           *string   `bson:"startAt,omitempty" json:"start_at,omitempty"`

	// Operator-facing fields — not serialized to the SPA's ProjectConfig
	// response.
	Revision    string    `bson:"revision" json:"-"`
	SecretURI   string    `bson:"secretUri,omitempty" json:"-"`
	CreatedAt   time.Time `bson:"createdAt" json:"-"`
	UpdatedAt   time.Time `bson:"updatedAt" json:"-"`
	SoftDeleted bool      `bson:"softDeleted,omitempty" json:"-"`
}

// Retention mirrors the SPA's Retention shape + our own
// backup/retention.Retention JSON tags.
type Retention struct {
	Last    int `bson:"last" json:"last"`
	Daily   int `bson:"daily" json:"daily"`
	Weekly  int `bson:"weekly" json:"weekly"`
	Monthly int `bson:"monthly" json:"monthly"`
}

// Store is the ProjectConfig repository. Two implementations: Mongo
// (production) + in-memory (tests), mirroring tenant.Store's shape.
type Store interface {
	// Create inserts a new config. Returns ErrConflict if a non-soft-
	// deleted row with the same Name exists.
	Create(ctx context.Context, cfg Config) (Config, error)
	// Get fetches by Name. ok=false when absent OR soft-deleted.
	Get(ctx context.Context, name string) (Config, bool, error)
	// Update replaces an existing row IF Revision matches current.
	// Returns ErrRevisionMismatch on stale revision; ErrNotFound on
	// missing row.
	Update(ctx context.Context, cfg Config) (Config, error)
	// SetSecret updates only the SecretURI field (keeps revision
	// unchanged on purpose — the SPA treats the secret as out-of-band
	// from the config revision check).
	SetSecret(ctx context.Context, name, secretURI string) error
	// SoftDelete marks a project removed. Requires the current revision
	// (optimistic concurrency); returns ErrRevisionMismatch on stale.
	SoftDelete(ctx context.Context, name, revision string) error
	// List returns every non-soft-deleted row. For an operator admin
	// view a separate ListIncludingDeleted would be a follow-up.
	List(ctx context.Context) ([]Config, error)
	Close(ctx context.Context) error
}

// Sentinel errors — caller-facing (handlers map to HTTP codes).
type storeError string

func (e storeError) Error() string { return string(e) }

const (
	ErrNotFound         = storeError("project not found")
	ErrConflict         = storeError("project already exists")
	ErrRevisionMismatch = storeError("project revision mismatch")
)

// NewRevision returns a short hex token used as the optimistic-
// concurrency marker. 8 bytes = 2^64 space; collisions on update
// against the same row are the thing we care about (one per revision
// bump) + are vanishingly unlikely at this width.
func NewRevision() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// -------------------- In-memory implementation --------------------

// InMemoryStore backs the handler tests. Map behind a RWMutex; no
// persistence.
type InMemoryStore struct {
	mu   sync.RWMutex
	rows map[string]Config
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{rows: map[string]Config{}}
}

func (s *InMemoryStore) Create(_ context.Context, cfg Config) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[cfg.Name]; ok && !r.SoftDeleted {
		return Config{}, ErrConflict
	}
	now := time.Now().UTC()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now
	cfg.SoftDeleted = false
	if cfg.Revision == "" {
		cfg.Revision = NewRevision()
	}
	s.rows[cfg.Name] = cfg
	return cfg, nil
}

func (s *InMemoryStore) Get(_ context.Context, name string) (Config, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rows[name]
	if !ok || r.SoftDeleted {
		return Config{}, false, nil
	}
	return r, true, nil
}

func (s *InMemoryStore) Update(_ context.Context, cfg Config) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.rows[cfg.Name]
	if !ok || existing.SoftDeleted {
		return Config{}, ErrNotFound
	}
	if existing.Revision != cfg.Revision {
		return Config{}, ErrRevisionMismatch
	}
	cfg.CreatedAt = existing.CreatedAt
	cfg.UpdatedAt = time.Now().UTC()
	cfg.SoftDeleted = false
	cfg.SecretURI = existing.SecretURI // not replaced via Update — SetSecret is the sole path
	cfg.Revision = NewRevision()
	s.rows[cfg.Name] = cfg
	return cfg, nil
}

func (s *InMemoryStore) SetSecret(_ context.Context, name, secretURI string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[name]
	if !ok || r.SoftDeleted {
		return ErrNotFound
	}
	r.SecretURI = secretURI
	r.UpdatedAt = time.Now().UTC()
	s.rows[name] = r
	return nil
}

func (s *InMemoryStore) SoftDelete(_ context.Context, name, revision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[name]
	if !ok || r.SoftDeleted {
		return ErrNotFound
	}
	if r.Revision != revision {
		return ErrRevisionMismatch
	}
	r.SoftDeleted = true
	r.UpdatedAt = time.Now().UTC()
	s.rows[name] = r
	return nil
}

func (s *InMemoryStore) List(_ context.Context) ([]Config, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Config, 0, len(s.rows))
	for _, r := range s.rows {
		if r.SoftDeleted {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *InMemoryStore) Close(_ context.Context) error { return nil }
