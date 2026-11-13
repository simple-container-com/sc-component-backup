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

type mysqlRunStore struct {
	db *sql.DB
}

func (s *mysqlRunStore) PutBackup(ctx context.Context, run backup.BackupRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("mysql runs put: marshal: %w", err)
	}
	const q = `
INSERT INTO backup_runs(id, tenant_stack, status, started_at, data)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
    tenant_stack=VALUES(tenant_stack),
    status=VALUES(status),
    started_at=VALUES(started_at),
    data=VALUES(data)
`
	_, err = s.db.ExecContext(ctx, q,
		run.ID, run.TenantStack, string(run.Status),
		run.StartedAt.UTC().Format(time.RFC3339Nano),
		string(raw))
	if err != nil {
		return fmt.Errorf("mysql runs put: %w", err)
	}
	return nil
}

func (s *mysqlRunStore) UpdateBackup(ctx context.Context, run backup.BackupRun) error {
	return s.PutBackup(ctx, run)
}

func (s *mysqlRunStore) GetBackup(ctx context.Context, id string) (backup.BackupRun, error) {
	var raw sql.RawBytes
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data FROM backup_runs WHERE id = ?`, id,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return backup.BackupRun{}, fmt.Errorf("mysql runs: backup %q not found", id)
	}
	if err != nil {
		return backup.BackupRun{}, fmt.Errorf("mysql runs get: %w", err)
	}
	var r backup.BackupRun
	if err := json.Unmarshal(raw, &r); err != nil {
		return backup.BackupRun{}, fmt.Errorf("mysql runs get unmarshal: %w", err)
	}
	return r, nil
}

func (s *mysqlRunStore) ListBackupsByTenant(ctx context.Context, tenantStack string) ([]backup.BackupRun, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT data FROM backup_runs WHERE tenant_stack = ? ORDER BY started_at DESC`,
		tenantStack,
	)
	if err != nil {
		return nil, fmt.Errorf("mysql runs list: %w", err)
	}
	defer rows.Close()
	var out []backup.BackupRun
	for rows.Next() {
		var raw sql.RawBytes
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("mysql runs list scan: %w", err)
		}
		var r backup.BackupRun
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("mysql runs list unmarshal: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql runs list rows: %w", err)
	}
	return out, nil
}

func (s *mysqlRunStore) PutRestore(ctx context.Context, run backup.RestoreRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("mysql restores put: marshal: %w", err)
	}
	const q = `
INSERT INTO restore_runs(id, tenant_stack, status, started_at, data)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
    tenant_stack=VALUES(tenant_stack),
    status=VALUES(status),
    started_at=VALUES(started_at),
    data=VALUES(data)
`
	_, err = s.db.ExecContext(ctx, q,
		run.ID, run.TenantStack, string(run.Status),
		run.StartedAt.UTC().Format(time.RFC3339Nano),
		string(raw))
	if err != nil {
		return fmt.Errorf("mysql restores put: %w", err)
	}
	return nil
}

func (s *mysqlRunStore) UpdateRestore(ctx context.Context, run backup.RestoreRun) error {
	return s.PutRestore(ctx, run)
}

func (s *mysqlRunStore) GetRestore(ctx context.Context, id string) (backup.RestoreRun, error) {
	var raw sql.RawBytes
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data FROM restore_runs WHERE id = ?`, id,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return backup.RestoreRun{}, fmt.Errorf("mysql restores: restore %q not found", id)
	}
	if err != nil {
		return backup.RestoreRun{}, fmt.Errorf("mysql restores get: %w", err)
	}
	var r backup.RestoreRun
	if err := json.Unmarshal(raw, &r); err != nil {
		return backup.RestoreRun{}, fmt.Errorf("mysql restores get unmarshal: %w", err)
	}
	return r, nil
}
