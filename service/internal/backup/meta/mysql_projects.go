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

// mysqlProjectStore mirrors sqliteProjectStore with ? placeholders +
// ON DUPLICATE KEY UPDATE and the shared storedConfig wrapper.
type mysqlProjectStore struct {
	db *sql.DB
}

func (s *mysqlProjectStore) Create(ctx context.Context, cfg project.Config) (project.Config, error) {
	now := time.Now().UTC()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now
	cfg.SoftDeleted = false
	if cfg.Revision == "" {
		cfg.Revision = project.NewRevision()
	}
	raw, err := marshalConfig(cfg)
	if err != nil {
		return project.Config{}, fmt.Errorf("mysql projects create: marshal: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return project.Config{}, fmt.Errorf("mysql projects create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingSoftDeleted sql.NullInt64
	err = tx.QueryRowContext(
		ctx,
		`SELECT soft_deleted FROM projects WHERE name = ?`,
		cfg.Name,
	).Scan(&existingSoftDeleted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO projects(name, revision, data, soft_deleted, updated_at) VALUES (?, ?, ?, 0, ?)`,
			cfg.Name, cfg.Revision, string(raw), now.Format(time.RFC3339Nano)); err != nil {
			return project.Config{}, fmt.Errorf("mysql projects create insert: %w", err)
		}
	case err != nil:
		return project.Config{}, fmt.Errorf("mysql projects create check: %w", err)
	default:
		if existingSoftDeleted.Int64 == 0 {
			return project.Config{}, project.ErrConflict
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET revision = ?, data = ?, soft_deleted = 0, updated_at = ? WHERE name = ?`,
			cfg.Revision, string(raw), now.Format(time.RFC3339Nano), cfg.Name); err != nil {
			return project.Config{}, fmt.Errorf("mysql projects create resurrect: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return project.Config{}, fmt.Errorf("mysql projects create commit: %w", err)
	}
	return cfg, nil
}

func (s *mysqlProjectStore) Get(ctx context.Context, name string) (project.Config, bool, error) {
	var (
		raw      sql.RawBytes
		revision string
	)
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data, revision FROM projects WHERE name = ? AND soft_deleted = 0`,
		name,
	).Scan(&raw, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return project.Config{}, false, nil
	}
	if err != nil {
		return project.Config{}, false, fmt.Errorf("mysql projects get: %w", err)
	}
	cfg, err := unmarshalConfig(raw)
	if err != nil {
		return project.Config{}, false, fmt.Errorf("mysql projects get unmarshal: %w", err)
	}
	cfg.Revision = revision
	return cfg, true, nil
}

func (s *mysqlProjectStore) Update(ctx context.Context, cfg project.Config) (project.Config, error) {
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
		return project.Config{}, fmt.Errorf("mysql projects update marshal: %w", err)
	}
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE projects SET revision = ?, data = ?, updated_at = ? WHERE name = ? AND revision = ? AND soft_deleted = 0`,
		newRev, string(raw), cfg.UpdatedAt.Format(time.RFC3339Nano),
		cfg.Name, callerRev,
	)
	if err != nil {
		return project.Config{}, fmt.Errorf("mysql projects update: %w", err)
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

func (s *mysqlProjectStore) SetSecret(ctx context.Context, name, secretURI string) error {
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
		return fmt.Errorf("mysql projects set-secret marshal: %w", err)
	}
	_, err = s.db.ExecContext(
		ctx,
		`UPDATE projects SET data = ?, updated_at = ? WHERE name = ? AND soft_deleted = 0`,
		string(raw), existing.UpdatedAt.Format(time.RFC3339Nano), name,
	)
	if err != nil {
		return fmt.Errorf("mysql projects set-secret: %w", err)
	}
	return nil
}

func (s *mysqlProjectStore) SoftDelete(ctx context.Context, name, revision string) error {
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE projects SET soft_deleted = 1, updated_at = ? WHERE name = ? AND revision = ? AND soft_deleted = 0`,
		time.Now().UTC().Format(time.RFC3339Nano),
		name, revision,
	)
	if err != nil {
		return fmt.Errorf("mysql projects soft-delete: %w", err)
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

func (s *mysqlProjectStore) List(ctx context.Context) ([]project.Config, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT data FROM projects WHERE soft_deleted = 0`,
	)
	if err != nil {
		return nil, fmt.Errorf("mysql projects list: %w", err)
	}
	defer rows.Close()
	var out []project.Config
	for rows.Next() {
		var raw sql.RawBytes
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("mysql projects list scan: %w", err)
		}
		cfg, err := unmarshalConfig(raw)
		if err != nil {
			return nil, fmt.Errorf("mysql projects list unmarshal: %w", err)
		}
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql projects list rows: %w", err)
	}
	return out, nil
}

func (s *mysqlProjectStore) Close(_ context.Context) error { return nil }
