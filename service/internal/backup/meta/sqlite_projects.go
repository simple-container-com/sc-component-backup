// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
)

// sqliteProjectStore satisfies project.Store. The row carries a JSON
// blob of project.Config; the typed revision column is what UPDATE
// WHERE revision=? consults — same optimistic-concurrency shape the
// Mongo backend uses.
type sqliteProjectStore struct {
	db *sql.DB
}

// storedConfig mirrors project.Config but with explicit JSON tags on
// the two fields that project.Config marks `json:"-"` — Revision and
// SecretURI. We need them in the JSON blob to survive the marshal/
// unmarshal roundtrip. Tests read them back from the typed columns
// (Get overlays the typed revision), but SecretURI ONLY lives in the
// JSON blob so it needs its own JSON tag here.
type storedConfig struct {
	project.Config
	RevisionJSON  string `json:"_revision,omitempty"`
	SecretURIJSON string `json:"_secret_uri,omitempty"`
}

func marshalConfig(cfg project.Config) ([]byte, error) {
	sc := storedConfig{Config: cfg, RevisionJSON: cfg.Revision, SecretURIJSON: cfg.SecretURI}
	return json.Marshal(sc)
}

func unmarshalConfig(raw []byte) (project.Config, error) {
	var sc storedConfig
	if err := json.Unmarshal(raw, &sc); err != nil {
		return project.Config{}, err
	}
	sc.Config.Revision = sc.RevisionJSON
	sc.Config.SecretURI = sc.SecretURIJSON
	return sc.Config, nil
}

// Create inserts a new row. ErrConflict if a live row shares the name;
// soft-deleted rows are resurrected with a fresh revision.
func (s *sqliteProjectStore) Create(ctx context.Context, cfg project.Config) (project.Config, error) {
	now := time.Now().UTC()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now
	cfg.SoftDeleted = false
	if cfg.Revision == "" {
		cfg.Revision = project.NewRevision()
	}
	raw, err := marshalConfig(cfg)
	if err != nil {
		return project.Config{}, fmt.Errorf("sqlite projects create: marshal: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return project.Config{}, fmt.Errorf("sqlite projects create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingSoftDeleted sql.NullBool
	err = tx.QueryRowContext(
		ctx,
		`SELECT soft_deleted FROM projects WHERE name = ?`,
		cfg.Name,
	).Scan(&existingSoftDeleted)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO projects(name, revision, data, soft_deleted, updated_at) VALUES (?, ?, ?, 0, ?)`,
			cfg.Name, cfg.Revision, string(raw), now.Format(time.RFC3339Nano)); err != nil {
			return project.Config{}, fmt.Errorf("sqlite projects create insert: %w", err)
		}
	case err != nil:
		return project.Config{}, fmt.Errorf("sqlite projects create check: %w", err)
	default:
		if !existingSoftDeleted.Bool {
			return project.Config{}, project.ErrConflict
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET revision = ?, data = ?, soft_deleted = 0, updated_at = ? WHERE name = ?`,
			cfg.Revision, string(raw), now.Format(time.RFC3339Nano), cfg.Name); err != nil {
			return project.Config{}, fmt.Errorf("sqlite projects create resurrect: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return project.Config{}, fmt.Errorf("sqlite projects create commit: %w", err)
	}
	return cfg, nil
}

func (s *sqliteProjectStore) Get(ctx context.Context, name string) (project.Config, bool, error) {
	var (
		raw      string
		revision string
	)
	// project.Config.Revision has `json:"-"` so we read it from the
	// typed column instead of the JSON blob.
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data, revision FROM projects WHERE name = ? AND soft_deleted = 0`,
		name,
	).Scan(&raw, &revision)
	if err == sql.ErrNoRows {
		return project.Config{}, false, nil
	}
	if err != nil {
		return project.Config{}, false, fmt.Errorf("sqlite projects get: %w", err)
	}
	cfg, err := unmarshalConfig([]byte(raw))
	if err != nil {
		return project.Config{}, false, fmt.Errorf("sqlite projects get unmarshal: %w", err)
	}
	cfg.Revision = revision
	return cfg, true, nil
}

// Update mints a new revision; conditional on caller's revision
// matching. RowsAffected == 0 disambiguates between ErrNotFound (row
// disappeared) and ErrRevisionMismatch (stale).
func (s *sqliteProjectStore) Update(ctx context.Context, cfg project.Config) (project.Config, error) {
	// Hold on to the caller's claimed revision BEFORE we overwrite it
	// with the new one — the WHERE clause below needs the caller's
	// revision to refuse stale updates.
	callerRev := cfg.Revision
	newRev := project.NewRevision()
	// Preserve CreatedAt + SecretURI from the existing row — both are
	// off-limits to Update.
	existing, ok, err := s.Get(ctx, cfg.Name)
	if err != nil {
		return project.Config{}, err
	}
	if !ok {
		return project.Config{}, project.ErrNotFound
	}
	if existing.Revision != callerRev {
		return project.Config{}, project.ErrRevisionMismatch
	}
	cfg.CreatedAt = existing.CreatedAt
	cfg.SecretURI = existing.SecretURI
	cfg.UpdatedAt = time.Now().UTC()
	cfg.SoftDeleted = false
	cfg.Revision = newRev
	raw, err := marshalConfig(cfg)
	if err != nil {
		return project.Config{}, fmt.Errorf("sqlite projects update marshal: %w", err)
	}
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE projects SET revision = ?, data = ?, updated_at = ? WHERE name = ? AND revision = ? AND soft_deleted = 0`,
		newRev, string(raw), cfg.UpdatedAt.Format(time.RFC3339Nano),
		cfg.Name, callerRev,
	)
	if err != nil {
		return project.Config{}, fmt.Errorf("sqlite projects update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_, stillThere, _ := s.Get(ctx, cfg.Name)
		if !stillThere {
			return project.Config{}, project.ErrNotFound
		}
		return project.Config{}, project.ErrRevisionMismatch
	}
	return cfg, nil
}

func (s *sqliteProjectStore) SetSecret(ctx context.Context, name, secretURI string) error {
	existing, ok, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return project.ErrNotFound
	}
	existing.SecretURI = secretURI
	existing.UpdatedAt = time.Now().UTC()
	raw, err := marshalConfig(existing)
	if err != nil {
		return fmt.Errorf("sqlite projects set-secret marshal: %w", err)
	}
	_, err = s.db.ExecContext(
		ctx,
		`UPDATE projects SET data = ?, updated_at = ? WHERE name = ? AND soft_deleted = 0`,
		string(raw), existing.UpdatedAt.Format(time.RFC3339Nano), name,
	)
	if err != nil {
		return fmt.Errorf("sqlite projects set-secret: %w", err)
	}
	return nil
}

func (s *sqliteProjectStore) SoftDelete(ctx context.Context, name, revision string) error {
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE projects SET soft_deleted = 1, updated_at = ? WHERE name = ? AND revision = ? AND soft_deleted = 0`,
		time.Now().UTC().Format(time.RFC3339Nano),
		name, revision,
	)
	if err != nil {
		return fmt.Errorf("sqlite projects soft-delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_, stillThere, _ := s.Get(ctx, name)
		if !stillThere {
			return project.ErrNotFound
		}
		return project.ErrRevisionMismatch
	}
	return nil
}

func (s *sqliteProjectStore) List(ctx context.Context) ([]project.Config, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT data FROM projects WHERE soft_deleted = 0`,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite projects list: %w", err)
	}
	defer rows.Close()
	var out []project.Config
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("sqlite projects list scan: %w", err)
		}
		cfg, err := unmarshalConfig([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("sqlite projects list unmarshal: %w", err)
		}
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite projects list rows: %w", err)
	}
	return out, nil
}

// Close is a no-op — the parent sqliteStore owns *sql.DB.
func (s *sqliteProjectStore) Close(_ context.Context) error { return nil }
