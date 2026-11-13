// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// sqliteRunStore satisfies scheduler.RunStore. Backup + restore rows
// carry their full data as JSON; the typed columns (id, tenant_stack,
// status, started_at) feed the ListBackupsByTenant + status-filter
// indexes.
type sqliteRunStore struct {
	db *sql.DB
}

func (s *sqliteRunStore) PutBackup(ctx context.Context, run backup.BackupRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("sqlite runs put: marshal: %w", err)
	}
	const q = `
INSERT INTO backup_runs(id, tenant_stack, status, started_at, data)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    tenant_stack=excluded.tenant_stack,
    status=excluded.status,
    started_at=excluded.started_at,
    data=excluded.data
`
	_, err = s.db.ExecContext(ctx, q,
		run.ID, run.TenantStack, string(run.Status),
		run.StartedAt.UTC().Format(time.RFC3339Nano),
		string(raw))
	if err != nil {
		return fmt.Errorf("sqlite runs put: %w", err)
	}
	return nil
}

// UpdateBackup is identical to PutBackup today (both upsert by id) —
// matches the Mongo-backed store's behaviour.
func (s *sqliteRunStore) UpdateBackup(ctx context.Context, run backup.BackupRun) error {
	return s.PutBackup(ctx, run)
}

func (s *sqliteRunStore) GetBackup(ctx context.Context, id string) (backup.BackupRun, error) {
	var raw string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data FROM backup_runs WHERE id = ?`, id,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return backup.BackupRun{}, fmt.Errorf("sqlite runs: backup %q not found", id)
	}
	if err != nil {
		return backup.BackupRun{}, fmt.Errorf("sqlite runs get: %w", err)
	}
	var r backup.BackupRun
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return backup.BackupRun{}, fmt.Errorf("sqlite runs get unmarshal: %w", err)
	}
	return r, nil
}

func (s *sqliteRunStore) ListBackupsByTenant(ctx context.Context, tenantStack string) ([]backup.BackupRun, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT data FROM backup_runs WHERE tenant_stack = ? ORDER BY started_at DESC`,
		tenantStack,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite runs list: %w", err)
	}
	defer rows.Close()
	var out []backup.BackupRun
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("sqlite runs list scan: %w", err)
		}
		var r backup.BackupRun
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("sqlite runs list unmarshal: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite runs list rows: %w", err)
	}
	return out, nil
}

func (s *sqliteRunStore) PutRestore(ctx context.Context, run backup.RestoreRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("sqlite restores put: marshal: %w", err)
	}
	const q = `
INSERT INTO restore_runs(id, tenant_stack, status, started_at, data)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    tenant_stack=excluded.tenant_stack,
    status=excluded.status,
    started_at=excluded.started_at,
    data=excluded.data
`
	_, err = s.db.ExecContext(ctx, q,
		run.ID, run.TenantStack, string(run.Status),
		run.StartedAt.UTC().Format(time.RFC3339Nano),
		string(raw))
	if err != nil {
		return fmt.Errorf("sqlite restores put: %w", err)
	}
	return nil
}

func (s *sqliteRunStore) UpdateRestore(ctx context.Context, run backup.RestoreRun) error {
	return s.PutRestore(ctx, run)
}

func (s *sqliteRunStore) GetRestore(ctx context.Context, id string) (backup.RestoreRun, error) {
	var raw string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data FROM restore_runs WHERE id = ?`, id,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return backup.RestoreRun{}, fmt.Errorf("sqlite restores: restore %q not found", id)
	}
	if err != nil {
		return backup.RestoreRun{}, fmt.Errorf("sqlite restores get: %w", err)
	}
	var r backup.RestoreRun
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return backup.RestoreRun{}, fmt.Errorf("sqlite restores get unmarshal: %w", err)
	}
	return r, nil
}
