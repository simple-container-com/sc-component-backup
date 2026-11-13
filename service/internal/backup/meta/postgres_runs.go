// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// postgresRunStore mirrors sqliteRunStore. Upsert by id via
// ON CONFLICT DO UPDATE.
type postgresRunStore struct {
	db *sql.DB
}

func (s *postgresRunStore) PutBackup(ctx context.Context, run backup.BackupRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("postgres runs put: marshal: %w", err)
	}
	const q = `
INSERT INTO backup_runs(id, tenant_stack, status, started_at, data)
VALUES ($1, $2, $3, $4, $5::jsonb)
ON CONFLICT(id) DO UPDATE SET
    tenant_stack=EXCLUDED.tenant_stack,
    status=EXCLUDED.status,
    started_at=EXCLUDED.started_at,
    data=EXCLUDED.data
`
	_, err = s.db.ExecContext(ctx, q,
		run.ID, run.TenantStack, string(run.Status),
		run.StartedAt.UTC().Format(time.RFC3339Nano),
		string(raw))
	if err != nil {
		return fmt.Errorf("postgres runs put: %w", err)
	}
	return nil
}

func (s *postgresRunStore) UpdateBackup(ctx context.Context, run backup.BackupRun) error {
	return s.PutBackup(ctx, run)
}

func (s *postgresRunStore) GetBackup(ctx context.Context, id string) (backup.BackupRun, error) {
	var raw string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data::text FROM backup_runs WHERE id = $1`, id,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return backup.BackupRun{}, fmt.Errorf("postgres runs: backup %q not found", id)
	}
	if err != nil {
		return backup.BackupRun{}, fmt.Errorf("postgres runs get: %w", err)
	}
	var r backup.BackupRun
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return backup.BackupRun{}, fmt.Errorf("postgres runs get unmarshal: %w", err)
	}
	return r, nil
}

func (s *postgresRunStore) ListBackupsByTenant(ctx context.Context, tenantStack string) ([]backup.BackupRun, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT data::text FROM backup_runs WHERE tenant_stack = $1 ORDER BY started_at DESC`,
		tenantStack,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres runs list: %w", err)
	}
	defer rows.Close()
	var out []backup.BackupRun
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("postgres runs list scan: %w", err)
		}
		var r backup.BackupRun
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("postgres runs list unmarshal: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres runs list rows: %w", err)
	}
	return out, nil
}

func (s *postgresRunStore) PutRestore(ctx context.Context, run backup.RestoreRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("postgres restores put: marshal: %w", err)
	}
	const q = `
INSERT INTO restore_runs(id, tenant_stack, status, started_at, data)
VALUES ($1, $2, $3, $4, $5::jsonb)
ON CONFLICT(id) DO UPDATE SET
    tenant_stack=EXCLUDED.tenant_stack,
    status=EXCLUDED.status,
    started_at=EXCLUDED.started_at,
    data=EXCLUDED.data
`
	_, err = s.db.ExecContext(ctx, q,
		run.ID, run.TenantStack, string(run.Status),
		run.StartedAt.UTC().Format(time.RFC3339Nano),
		string(raw))
	if err != nil {
		return fmt.Errorf("postgres restores put: %w", err)
	}
	return nil
}

func (s *postgresRunStore) UpdateRestore(ctx context.Context, run backup.RestoreRun) error {
	return s.PutRestore(ctx, run)
}

func (s *postgresRunStore) GetRestore(ctx context.Context, id string) (backup.RestoreRun, error) {
	var raw string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data::text FROM restore_runs WHERE id = $1`, id,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return backup.RestoreRun{}, fmt.Errorf("postgres restores: restore %q not found", id)
	}
	if err != nil {
		return backup.RestoreRun{}, fmt.Errorf("postgres restores get: %w", err)
	}
	var r backup.RestoreRun
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return backup.RestoreRun{}, fmt.Errorf("postgres restores get unmarshal: %w", err)
	}
	return r, nil
}
