// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" driver for database/sql

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// postgresStore persists all four row families in a Postgres schema.
// JSONB columns hold the full row — same json-tagged structs the Mongo
// + SQLite backends serialise — plus typed columns for the indexes
// (primary keys, revisions, soft-delete markers, timestamps).
//
// Shape mirrors sqliteStore (slice 1e.4d) with dialect swaps:
//   - `?` placeholders → `$1`/`$2`/… numbered
//   - `TEXT` for JSON columns → `JSONB`
//   - `INTEGER PRIMARY KEY AUTOINCREMENT` → `BIGSERIAL PRIMARY KEY`
//   - `INSERT ... ON CONFLICT(col) DO UPDATE SET ...` kept; same syntax
//   - `TIMESTAMPTZ` for timestamps (text-serialised at the Go layer so
//     the logic matches SQLite verbatim; a future slice can swap to
//     typed time.Time columns if we start needing server-side time
//     predicates)
type postgresStore struct {
	db       *sql.DB
	tenants  *postgresTenantStore
	runs     *postgresRunStore
	projects *postgresProjectStore
	snaps    *postgresSnapshotStore
}

func newPostgresStore(ctx context.Context, uri string) (Store, error) {
	if uri == "" {
		return nil, fmt.Errorf("meta postgres: requires a URI")
	}
	db, err := sql.Open("pgx", uri)
	if err != nil {
		return nil, fmt.Errorf("meta postgres: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta postgres: ping: %w", err)
	}
	if err := migratePostgres(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta postgres: migrate: %w", err)
	}
	s := &postgresStore{db: db}
	s.tenants = &postgresTenantStore{db: db}
	s.runs = &postgresRunStore{db: db}
	s.projects = &postgresProjectStore{db: db}
	s.snaps = &postgresSnapshotStore{db: db, runs: s.runs}
	return s, nil
}

func (s *postgresStore) Tenants() tenant.Store              { return s.tenants }
func (s *postgresStore) Runs() scheduler.RunStore           { return s.runs }
func (s *postgresStore) Projects() project.Store            { return s.projects }
func (s *postgresStore) Snapshots() scheduler.SnapshotStore { return s.snaps }
func (s *postgresStore) Close(_ context.Context) error      { return s.db.Close() }

// migrationSQLPostgres mirrors sqlite.go's migrationSQL. Idempotent via
// IF NOT EXISTS so service boot against a cold DB OR an existing one
// works safely. schema_version is advisory today; a future migration
// bump writes a new row + the migrator switches on max version.
const migrationSQLPostgres = `
CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL PRIMARY KEY);
INSERT INTO schema_version(version) VALUES (1) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS tenants (
  stack_name   TEXT PRIMARY KEY,
  data         JSONB NOT NULL,
  soft_deleted BOOLEAN NOT NULL DEFAULT FALSE,
  updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tenants_soft_deleted ON tenants(soft_deleted);

CREATE TABLE IF NOT EXISTS projects (
  name         TEXT PRIMARY KEY,
  revision     TEXT NOT NULL,
  data         JSONB NOT NULL,
  soft_deleted BOOLEAN NOT NULL DEFAULT FALSE,
  updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_projects_soft_deleted ON projects(soft_deleted);

CREATE TABLE IF NOT EXISTS backup_runs (
  id            TEXT PRIMARY KEY,
  tenant_stack  TEXT NOT NULL,
  status        TEXT NOT NULL,
  started_at    TEXT NOT NULL,
  data          JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_backup_runs_tenant ON backup_runs(tenant_stack, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_backup_runs_status ON backup_runs(status);

CREATE TABLE IF NOT EXISTS restore_runs (
  id            TEXT PRIMARY KEY,
  tenant_stack  TEXT NOT NULL,
  status        TEXT NOT NULL,
  started_at    TEXT NOT NULL,
  data          JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_restore_runs_tenant ON restore_runs(tenant_stack, started_at DESC);

CREATE TABLE IF NOT EXISTS db_snapshots (
  id         BIGSERIAL PRIMARY KEY,
  project    TEXT NOT NULL,
  database   TEXT NOT NULL,
  ts         TEXT NOT NULL,
  data       JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_db_snapshots_project_ts ON db_snapshots(project, ts DESC);
CREATE INDEX IF NOT EXISTS idx_db_snapshots_project_database_ts ON db_snapshots(project, database, ts DESC);

CREATE TABLE IF NOT EXISTS object_samples (
  id          BIGSERIAL PRIMARY KEY,
  project     TEXT NOT NULL,
  database    TEXT NOT NULL,
  snapshot_id BIGINT NOT NULL,
  ts          TEXT NOT NULL,
  data        JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_object_samples_project_database_ts ON object_samples(project, database, ts DESC);
CREATE INDEX IF NOT EXISTS idx_object_samples_snapshot ON object_samples(snapshot_id);
`

func migratePostgres(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, migrationSQLPostgres)
	return err
}
