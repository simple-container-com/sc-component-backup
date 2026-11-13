// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
)

// postgresProjectStore mirrors sqliteProjectStore. Shares the storedConfig
// wrapper (defined in sqlite_projects.go) so Revision + SecretURI (both
// json:"-" on project.Config) survive the JSONB roundtrip.
type postgresProjectStore struct {
	db *sql.DB
}

func (s *postgresProjectStore) Create(ctx context.Context, cfg project.Config) (project.Config, error) {
	now := time.Now().UTC()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now
	cfg.SoftDeleted = false
	if cfg.Revision == "" {
		cfg.Revision = project.NewRevision()
	}
	raw, err := marshalConfig(cfg)
	if err != nil {
		return project.Config{}, fmt.Errorf("postgres projects create: marshal: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return project.Config{}, fmt.Errorf("postgres projects create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingSoftDeleted sql.NullBool
	err = tx.QueryRowContext(
		ctx,
		`SELECT soft_deleted FROM projects WHERE name = $1`,
		cfg.Name,
	).Scan(&existingSoftDeleted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO projects(name, revision, data, soft_deleted, updated_at) VALUES ($1, $2, $3::jsonb, FALSE, $4)`,
			cfg.Name, cfg.Revision, string(raw), now.Format(time.RFC3339Nano)); err != nil {
			return project.Config{}, fmt.Errorf("postgres projects create insert: %w", err)
		}
	case err != nil:
		return project.Config{}, fmt.Errorf("postgres projects create check: %w", err)
	default:
		if !existingSoftDeleted.Bool {
			return project.Config{}, project.ErrConflict
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET revision = $1, data = $2::jsonb, soft_deleted = FALSE, updated_at = $3 WHERE name = $4`,
			cfg.Revision, string(raw), now.Format(time.RFC3339Nano), cfg.Name); err != nil {
			return project.Config{}, fmt.Errorf("postgres projects create resurrect: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return project.Config{}, fmt.Errorf("postgres projects create commit: %w", err)
	}
	return cfg, nil
}

func (s *postgresProjectStore) Get(ctx context.Context, name string) (project.Config, bool, error) {
	var (
		raw      string
		revision string
	)
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data::text, revision FROM projects WHERE name = $1 AND soft_deleted = FALSE`,
		name,
	).Scan(&raw, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return project.Config{}, false, nil
	}
	if err != nil {
		return project.Config{}, false, fmt.Errorf("postgres projects get: %w", err)
	}
	cfg, err := unmarshalConfig([]byte(raw))
	if err != nil {
		return project.Config{}, false, fmt.Errorf("postgres projects get unmarshal: %w", err)
	}
	cfg.Revision = revision
	return cfg, true, nil
}

func (s *postgresProjectStore) Update(ctx context.Context, cfg project.Config) (project.Config, error) {
	callerRev := cfg.Revision
	newRev := project.NewRevision()
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
		return project.Config{}, fmt.Errorf("postgres projects update marshal: %w", err)
	}
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE projects SET revision = $1, data = $2::jsonb, updated_at = $3 WHERE name = $4 AND revision = $5 AND soft_deleted = FALSE`,
		newRev, string(raw), cfg.UpdatedAt.Format(time.RFC3339Nano),
		cfg.Name, callerRev,
	)
	if err != nil {
		return project.Config{}, fmt.Errorf("postgres projects update: %w", err)
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

func (s *postgresProjectStore) SetSecret(ctx context.Context, name, secretURI string) error {
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
		return fmt.Errorf("postgres projects set-secret marshal: %w", err)
	}
	_, err = s.db.ExecContext(
		ctx,
		`UPDATE projects SET data = $1::jsonb, updated_at = $2 WHERE name = $3 AND soft_deleted = FALSE`,
		string(raw), existing.UpdatedAt.Format(time.RFC3339Nano), name,
	)
	if err != nil {
		return fmt.Errorf("postgres projects set-secret: %w", err)
	}
	return nil
}

func (s *postgresProjectStore) SoftDelete(ctx context.Context, name, revision string) error {
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE projects SET soft_deleted = TRUE, updated_at = $1 WHERE name = $2 AND revision = $3 AND soft_deleted = FALSE`,
		time.Now().UTC().Format(time.RFC3339Nano),
		name, revision,
	)
	if err != nil {
		return fmt.Errorf("postgres projects soft-delete: %w", err)
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

func (s *postgresProjectStore) List(ctx context.Context) ([]project.Config, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT data::text FROM projects WHERE soft_deleted = FALSE`,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres projects list: %w", err)
	}
	defer rows.Close()
	var out []project.Config
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("postgres projects list scan: %w", err)
		}
		cfg, err := unmarshalConfig([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("postgres projects list unmarshal: %w", err)
		}
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres projects list rows: %w", err)
	}
	return out, nil
}

func (s *postgresProjectStore) Close(_ context.Context) error { return nil }
