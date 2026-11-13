// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/go-sql-driver/mysql" // registers "mysql" driver for database/sql

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// mysqlStore persists all four row families in MySQL / MariaDB. JSON
// columns hold the full row; typed columns feed the indexes.
//
// Shape mirrors sqliteStore (slice 1e.4d) with dialect swaps:
//   - `?` placeholders match SQLite + PG-unlike
//   - `JSON` columns (MariaDB 10.2+ / MySQL 5.7+)
//   - `INSERT ... ON CONFLICT` → `INSERT ... ON DUPLICATE KEY UPDATE`
//   - `AUTOINCREMENT` → `AUTO_INCREMENT`
//   - `BOOLEAN` → `TINYINT(1)` (MySQL alias)
//   - `TEXT` is accepted for PK columns on MariaDB but MySQL < 5.7
//     refuses TEXT PK; use `VARCHAR(191)` to stay portable + fit in
//     utf8mb4 index-byte budget.
//   - `ENGINE=InnoDB` on every CREATE for crash-safe + transactional
//     semantics.
type mysqlStore struct {
	db       *sql.DB
	tenants  *mysqlTenantStore
	runs     *mysqlRunStore
	projects *mysqlProjectStore
	snaps    *mysqlSnapshotStore
}

func newMySQLStore(ctx context.Context, uri string) (Store, error) {
	if uri == "" {
		return nil, fmt.Errorf("meta mysql: requires a URI")
	}
	// Normalise to the go-sql-driver/mysql DSN shape. mysql:// URIs need
	// the scheme stripped; plain user:pass@tcp(...)/ passes through.
	dsn := strings.TrimPrefix(strings.TrimPrefix(uri, "mysql://"), "mariadb://")
	// Require parseTime=true so TIMESTAMP columns round-trip as Go
	// time.Time; `multiStatements=true` lets us run the batch migration
	// in one Exec. Append only when the caller didn't already set them.
	if !strings.Contains(dsn, "parseTime=") {
		dsn = appendDSNParam(dsn, "parseTime=true")
	}
	if !strings.Contains(dsn, "multiStatements=") {
		dsn = appendDSNParam(dsn, "multiStatements=true")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("meta mysql: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta mysql: ping: %w", err)
	}
	if err := migrateMySQL(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta mysql: migrate: %w", err)
	}
	s := &mysqlStore{db: db}
	s.tenants = &mysqlTenantStore{db: db}
	s.runs = &mysqlRunStore{db: db}
	s.projects = &mysqlProjectStore{db: db}
	s.snaps = &mysqlSnapshotStore{db: db, runs: s.runs}
	return s, nil
}

func (s *mysqlStore) Tenants() tenant.Store              { return s.tenants }
func (s *mysqlStore) Runs() scheduler.RunStore           { return s.runs }
func (s *mysqlStore) Projects() project.Store            { return s.projects }
func (s *mysqlStore) Snapshots() scheduler.SnapshotStore { return s.snaps }
func (s *mysqlStore) Close(_ context.Context) error      { return s.db.Close() }

// migrationSQLMySQL mirrors migrationSQLPostgres with MariaDB/MySQL
// dialect. VARCHAR(191) on PK columns fits utf8mb4 max key length
// (767 bytes / 4 bytes-per-char). ENGINE=InnoDB is explicit —
// older MariaDB builds default to Aria.
const migrationSQLMySQL = `
CREATE TABLE IF NOT EXISTS schema_version (
  version INT NOT NULL PRIMARY KEY
) ENGINE=InnoDB;
INSERT IGNORE INTO schema_version(version) VALUES (1);

CREATE TABLE IF NOT EXISTS tenants (
  stack_name   VARCHAR(191) PRIMARY KEY,
  data         JSON NOT NULL,
  soft_deleted TINYINT(1) NOT NULL DEFAULT 0,
  updated_at   VARCHAR(40) NOT NULL,
  INDEX idx_tenants_soft_deleted (soft_deleted)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS projects (
  name         VARCHAR(191) PRIMARY KEY,
  revision     VARCHAR(64) NOT NULL,
  data         JSON NOT NULL,
  soft_deleted TINYINT(1) NOT NULL DEFAULT 0,
  updated_at   VARCHAR(40) NOT NULL,
  INDEX idx_projects_soft_deleted (soft_deleted)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS backup_runs (
  id            VARCHAR(191) PRIMARY KEY,
  tenant_stack  VARCHAR(191) NOT NULL,
  status        VARCHAR(32)  NOT NULL,
  started_at    VARCHAR(40)  NOT NULL,
  data          JSON NOT NULL,
  INDEX idx_backup_runs_tenant (tenant_stack, started_at DESC),
  INDEX idx_backup_runs_status (status)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS restore_runs (
  id            VARCHAR(191) PRIMARY KEY,
  tenant_stack  VARCHAR(191) NOT NULL,
  status        VARCHAR(32)  NOT NULL,
  started_at    VARCHAR(40)  NOT NULL,
  data          JSON NOT NULL,
  INDEX idx_restore_runs_tenant (tenant_stack, started_at DESC)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS db_snapshots (
  id         BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  project    VARCHAR(191) NOT NULL,
  ` + "`database`" + `   VARCHAR(191) NOT NULL,
  ts         VARCHAR(40) NOT NULL,
  data       JSON NOT NULL,
  INDEX idx_db_snapshots_project_ts (project, ts DESC),
  INDEX idx_db_snapshots_project_database_ts (project, ` + "`database`" + `, ts DESC)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS object_samples (
  id          BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  project     VARCHAR(191) NOT NULL,
  ` + "`database`" + `    VARCHAR(191) NOT NULL,
  snapshot_id BIGINT NOT NULL,
  ts          VARCHAR(40) NOT NULL,
  data        JSON NOT NULL,
  INDEX idx_object_samples_project_database_ts (project, ` + "`database`" + `, ts DESC),
  INDEX idx_object_samples_snapshot (snapshot_id)
) ENGINE=InnoDB;
`

func migrateMySQL(ctx context.Context, db *sql.DB) error {
	// MySQL doesn't let us batch DDL in a single Exec without
	// multiStatements=true in the DSN (set above). Still safe to split
	// defensively — a mid-migration failure would otherwise leave half
	// the tables created.
	stmts := strings.Split(migrationSQLMySQL, ";")
	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("stmt %.60q...: %w", stmt, err)
		}
	}
	return nil
}

func appendDSNParam(dsn, param string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&" + param
	}
	return dsn + "?" + param
}
