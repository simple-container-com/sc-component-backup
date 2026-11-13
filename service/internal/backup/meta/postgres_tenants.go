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

// postgresTenantStore mirrors sqliteTenantStore with PG numbered
// placeholders + JSONB.
type postgresTenantStore struct {
	db *sql.DB
}

func (s *postgresTenantStore) Upsert(ctx context.Context, record tenant.Record) error {
	if record.LastSeenAt.IsZero() {
		record.LastSeenAt = time.Now().UTC()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = record.LastSeenAt
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("postgres tenants upsert: marshal: %w", err)
	}
	const q = `
INSERT INTO tenants(stack_name, data, soft_deleted, updated_at)
VALUES ($1, $2::jsonb, FALSE, $3)
ON CONFLICT(stack_name) DO UPDATE SET
    data=EXCLUDED.data,
    soft_deleted=FALSE,
    updated_at=EXCLUDED.updated_at
`
	_, err = s.db.ExecContext(ctx, q, record.StackName, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("postgres tenants upsert: %w", err)
	}
	return nil
}

func (s *postgresTenantStore) List(ctx context.Context) ([]tenant.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data::text FROM tenants WHERE soft_deleted = FALSE`)
	if err != nil {
		return nil, fmt.Errorf("postgres tenants list: %w", err)
	}
	defer rows.Close()
	var out []tenant.Record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("postgres tenants list scan: %w", err)
		}
		var r tenant.Record
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("postgres tenants list unmarshal: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres tenants list rows: %w", err)
	}
	return out, nil
}

func (s *postgresTenantStore) Get(ctx context.Context, stackName string) (tenant.Record, bool, error) {
	var raw string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT data::text FROM tenants WHERE stack_name = $1 AND soft_deleted = FALSE`,
		stackName,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return tenant.Record{}, false, nil
	}
	if err != nil {
		return tenant.Record{}, false, fmt.Errorf("postgres tenants get: %w", err)
	}
	var r tenant.Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return tenant.Record{}, false, fmt.Errorf("postgres tenants get unmarshal: %w", err)
	}
	return r, true, nil
}

func (s *postgresTenantStore) SoftDelete(ctx context.Context, stackName string) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE tenants SET soft_deleted = TRUE, updated_at = $1 WHERE stack_name = $2`,
		time.Now().UTC().Format(time.RFC3339Nano),
		stackName,
	)
	if err != nil {
		return fmt.Errorf("postgres tenants soft-delete: %w", err)
	}
	return nil
}

func (s *postgresTenantStore) Close(_ context.Context) error { return nil }
