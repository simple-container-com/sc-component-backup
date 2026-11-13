// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver — no cgo, Lambda-safe

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// Default file path under the archive root.
const (
	sqliteMetaDir  = "_meta"
	sqliteFileName = "sc-component-backup.sqlite"
)

// sqliteStore persists all four row families in one SQLite file next to
// the archives. JSON columns hold the full row — same json-tagged
// structs the Mongo backend serialises — plus typed columns for the
// indexes (primary keys, revisions, soft-delete markers, timestamps).
//
// WAL mode at open (`_pragma=journal_mode(WAL)`); single-process writer
// with concurrent readers — matches the one-service-per-deploy posture.
type sqliteStore struct {
	db       *sql.DB
	tenants  *sqliteTenantStore
	runs     *sqliteRunStore
	projects *sqliteProjectStore
	snaps    *sqliteSnapshotStore
}

// newSQLiteStore refuses a non-filesystem archive, resolves the file
// path, opens the DB, pings, migrates.
func newSQLiteStore(_ context.Context, cfg Config, archive storage.Store) (Store, error) {
	fsPath, ok := archiveFSRoot(archive)
	if !ok {
		return nil, fmt.Errorf("meta sqlite: requires an FSStore archive, got %T", archive)
	}
	dbPath := cfg.SQLitePath
	if dbPath == "" {
		dbPath = filepath.Join(fsPath, sqliteMetaDir, sqliteFileName)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return nil, fmt.Errorf("meta sqlite: mkdir %q: %w", filepath.Dir(dbPath), err)
	}
	dsn := "file:" + dbPath + "?" + url.Values{
		"_pragma": {"journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)", "foreign_keys(ON)"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("meta sqlite: open %q: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta sqlite: ping: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta sqlite: migrate: %w", err)
	}
	s := &sqliteStore{db: db}
	s.tenants = &sqliteTenantStore{db: db}
	s.runs = &sqliteRunStore{db: db}
	s.projects = &sqliteProjectStore{db: db}
	s.snaps = &sqliteSnapshotStore{db: db, runs: s.runs}
	return s, nil
}

// archiveFSRoot extracts the FSStore root; false for any other type.
func archiveFSRoot(archive storage.Store) (string, bool) {
	if fs, ok := archive.(*storage.FSStore); ok {
		return fs.Root(), true
	}
	return "", false
}

func (s *sqliteStore) Tenants() tenant.Store              { return s.tenants }
func (s *sqliteStore) Runs() scheduler.RunStore           { return s.runs }
func (s *sqliteStore) Projects() project.Store            { return s.projects }
func (s *sqliteStore) Snapshots() scheduler.SnapshotStore { return s.snaps }
func (s *sqliteStore) Close(_ context.Context) error      { return s.db.Close() }

// migrationSQL is a single CREATE IF NOT EXISTS batch — re-opening is
// idempotent. schema_version is advisory today; a future migration
// bump writes a new row + migrate switches on max version.
const migrationSQL = `
CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL PRIMARY KEY);
INSERT OR IGNORE INTO schema_version(version) VALUES (1);

CREATE TABLE IF NOT EXISTS tenants (
  stack_name   TEXT PRIMARY KEY,
  data         TEXT NOT NULL,
  soft_deleted INTEGER NOT NULL DEFAULT 0,
  updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tenants_soft_deleted ON tenants(soft_deleted);

CREATE TABLE IF NOT EXISTS projects (
  name         TEXT PRIMARY KEY,
  revision     TEXT NOT NULL,
  data         TEXT NOT NULL,
  soft_deleted INTEGER NOT NULL DEFAULT 0,
  updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_projects_soft_deleted ON projects(soft_deleted);

CREATE TABLE IF NOT EXISTS backup_runs (
  id            TEXT PRIMARY KEY,
  tenant_stack  TEXT NOT NULL,
  status        TEXT NOT NULL,
  started_at    TEXT NOT NULL,
  data          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_backup_runs_tenant ON backup_runs(tenant_stack, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_backup_runs_status ON backup_runs(status);

CREATE TABLE IF NOT EXISTS restore_runs (
  id            TEXT PRIMARY KEY,
  tenant_stack  TEXT NOT NULL,
  status        TEXT NOT NULL,
  started_at    TEXT NOT NULL,
  data          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_restore_runs_tenant ON restore_runs(tenant_stack, started_at DESC);

CREATE TABLE IF NOT EXISTS db_snapshots (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project    TEXT NOT NULL,
  database   TEXT NOT NULL,
  ts         TEXT NOT NULL,
  data       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_db_snapshots_project_ts ON db_snapshots(project, ts DESC);
CREATE INDEX IF NOT EXISTS idx_db_snapshots_project_database_ts ON db_snapshots(project, database, ts DESC);

CREATE TABLE IF NOT EXISTS object_samples (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  project     TEXT NOT NULL,
  database    TEXT NOT NULL,
  snapshot_id INTEGER NOT NULL,
  ts          TEXT NOT NULL,
  data        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_object_samples_project_database_ts ON object_samples(project, database, ts DESC);
CREATE INDEX IF NOT EXISTS idx_object_samples_snapshot ON object_samples(snapshot_id);
`

func migrate(db *sql.DB) error {
	_, err := db.Exec(migrationSQL)
	return err
}
