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

	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

type mysqlTenantStore struct {
	db *sql.DB
}

func (s *mysqlTenantStore) Upsert(ctx context.Context, record tenant.Record) error {
	if record.LastSeenAt.IsZero() {
		record.LastSeenAt = time.Now().UTC()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = record.LastSeenAt
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("mysql tenants upsert: marshal: %w", err)
	}
	const q = `
INSERT INTO tenants(stack_name, data, soft_deleted, updated_at)
VALUES (?, ?, 0, ?)
ON DUPLICATE KEY UPDATE
    data=VALUES(data),
    soft_deleted=0,
    updated_at=VALUES(updated_at)
`
	_, err = s.db.ExecContext(ctx, q, record.StackName, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("mysql tenants upsert: %w", err)
	}
	return nil
}

func (s *mysqlTenantStore) List(ctx context.Context) ([]tenant.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM tenants WHERE soft_deleted = 0`)
	if err != nil {
		return nil, fmt.Errorf("mysql tenants list: %w", err)
	}
	defer rows.Close()
	var out []tenant.Record
	for rows.Next() {
		var raw sql.RawBytes
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("mysql tenants list scan: %w", err)
		}
		var r tenant.Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("mysql tenants list unmarshal: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql tenants list rows: %w", err)
	}
	return out, nil
}

func (s *mysqlTenantStore) Get(ctx context.Context, stackName string) (tenant.Record, bool, error) {
	var raw sql.RawBytes
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data FROM tenants WHERE stack_name = ? AND soft_deleted = 0`,
		stackName,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return tenant.Record{}, false, nil
	}
	if err != nil {
		return tenant.Record{}, false, fmt.Errorf("mysql tenants get: %w", err)
	}
	var r tenant.Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return tenant.Record{}, false, fmt.Errorf("mysql tenants get unmarshal: %w", err)
	}
	return r, true, nil
}

func (s *mysqlTenantStore) SoftDelete(ctx context.Context, stackName string) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE tenants SET soft_deleted = 1, updated_at = ? WHERE stack_name = ?`,
		time.Now().UTC().Format(time.RFC3339Nano),
		stackName,
	)
	if err != nil {
		return fmt.Errorf("mysql tenants soft-delete: %w", err)
	}
	return nil
}

func (s *mysqlTenantStore) Close(_ context.Context) error { return nil }
