// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// sqliteTenantStore satisfies tenant.Store with the shared *sql.DB
// from sqliteStore. Rows persist as JSON blobs in `tenants.data`;
// `soft_deleted` gates List + Get visibility.
type sqliteTenantStore struct {
	db *sql.DB
}

func (s *sqliteTenantStore) Upsert(ctx context.Context, record tenant.Record) error {
	if record.LastSeenAt.IsZero() {
		record.LastSeenAt = time.Now().UTC()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = record.LastSeenAt
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("sqlite tenants upsert: marshal: %w", err)
	}
	// soft_deleted resets to 0 on upsert — plugin push-register for a
	// previously-deleted tenant brings it back, matching the Mongo
	// store's SoftDelete-then-Upsert behaviour.
	const q = `
INSERT INTO tenants(stack_name, data, soft_deleted, updated_at)
VALUES (?, ?, 0, ?)
ON CONFLICT(stack_name) DO UPDATE SET
    data=excluded.data,
    soft_deleted=0,
    updated_at=excluded.updated_at
`
	_, err = s.db.ExecContext(ctx, q, record.StackName, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("sqlite tenants upsert: %w", err)
	}
	return nil
}

func (s *sqliteTenantStore) List(ctx context.Context) ([]tenant.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM tenants WHERE soft_deleted = 0`)
	if err != nil {
		return nil, fmt.Errorf("sqlite tenants list: %w", err)
	}
	defer rows.Close()
	var out []tenant.Record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("sqlite tenants list scan: %w", err)
		}
		var r tenant.Record
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("sqlite tenants list unmarshal: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite tenants list rows: %w", err)
	}
	return out, nil
}

func (s *sqliteTenantStore) Get(ctx context.Context, stackName string) (tenant.Record, bool, error) {
	var raw string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data FROM tenants WHERE stack_name = ? AND soft_deleted = 0`,
		stackName,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return tenant.Record{}, false, nil
	}
	if err != nil {
		return tenant.Record{}, false, fmt.Errorf("sqlite tenants get: %w", err)
	}
	var r tenant.Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return tenant.Record{}, false, fmt.Errorf("sqlite tenants get unmarshal: %w", err)
	}
	return r, true, nil
}

func (s *sqliteTenantStore) SoftDelete(ctx context.Context, stackName string) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE tenants SET soft_deleted = 1, updated_at = ? WHERE stack_name = ?`,
		time.Now().UTC().Format(time.RFC3339Nano),
		stackName,
	)
	if err != nil {
		return fmt.Errorf("sqlite tenants soft-delete: %w", err)
	}
	return nil
}

// Close is a no-op — the parent sqliteStore owns *sql.DB.
func (s *sqliteTenantStore) Close(_ context.Context) error { return nil }
