// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// TestMeta_MySQLNewRefusesBadURI runs ALWAYS (no DB dependency). A
// malformed URI hits the driver's Open/Ping path and returns an error —
// confirms the dialect-selection wiring + fail-fast posture.
func TestMeta_MySQLNewRefusesBadURI(t *testing.T) {
	// Note: the MySQL driver's DSN parse accepts a lot of forms (some
	// weirdly permissive). Pass a shape that's definitely wrong: a
	// pure scheme-less string with no host/db at all.
	if _, err := newMySQLStore(context.Background(), ""); err == nil {
		t.Fatal("expected error from newMySQLStore with empty URI")
	}
}

// mysqlStoreOrSkip is the integration harness. BACKUP_MYSQL_TEST_URI
// gates it; drops + re-migrates every table so tests don't bleed.
func mysqlStoreOrSkip(t *testing.T) Store {
	t.Helper()
	uri := os.Getenv("BACKUP_MYSQL_TEST_URI")
	if uri == "" {
		t.Skip("integration — needs BACKUP_MYSQL_TEST_URI")
	}
	s, err := newMySQLStore(context.Background(), uri)
	if err != nil {
		t.Fatalf("newMySQLStore: %v", err)
	}
	ms := s.(*mysqlStore)
	for _, tbl := range []string{"object_samples", "db_snapshots", "restore_runs", "backup_runs", "projects", "tenants", "schema_version"} {
		if _, err := ms.db.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	if err := migrateMySQL(context.Background(), ms.db); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestMySQL_MigrationsIdempotent(t *testing.T) {
	s := mysqlStoreOrSkip(t).(*mysqlStore)
	if err := migrateMySQL(context.Background(), s.db); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_version row count: got %d, want 1", count)
	}
}

func TestMySQL_Tenants(t *testing.T) {
	s := mysqlStoreOrSkip(t)
	ctx := context.Background()
	rec := tenant.Record{StackName: "demo", Version: "1"}
	if err := s.Tenants().Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, ok, err := s.Tenants().Get(ctx, "demo")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Version != "1" {
		t.Errorf("version drift %q", got.Version)
	}
	if err := s.Tenants().SoftDelete(ctx, "demo"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, ok, _ := s.Tenants().Get(ctx, "demo"); ok {
		t.Error("Get after SoftDelete should be invisible")
	}
	rec.Version = "2"
	if err := s.Tenants().Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert resurrect: %v", err)
	}
	got, ok, _ = s.Tenants().Get(ctx, "demo")
	if !ok || got.Version != "2" {
		t.Errorf("resurrect: ok=%v version=%q", ok, got.Version)
	}
}

func TestMySQL_ProjectsCRUD(t *testing.T) {
	s := mysqlStoreOrSkip(t)
	ctx := context.Background()
	created, err := s.Projects().Create(ctx, project.Config{Name: "p", Engine: "mysql"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Projects().Create(ctx, project.Config{Name: "p"}); err != project.ErrConflict {
		t.Errorf("duplicate Create: want ErrConflict, got %v", err)
	}
	stale := created
	stale.Revision = "wrong"
	if _, err := s.Projects().Update(ctx, stale); err != project.ErrRevisionMismatch {
		t.Errorf("stale Update: got %v", err)
	}
	created.Interval = "6h"
	updated, err := s.Projects().Update(ctx, created)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Revision == created.Revision {
		t.Error("Update should mint new revision")
	}
	if err := s.Projects().SoftDelete(ctx, "p", updated.Revision); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, ok, _ := s.Projects().Get(ctx, "p"); ok {
		t.Error("Get after SoftDelete should be invisible")
	}
	if _, err := s.Projects().Create(ctx, project.Config{Name: "p"}); err != nil {
		t.Errorf("Create-after-SoftDelete resurrect: %v", err)
	}
}

func TestMySQL_RunsByTenant(t *testing.T) {
	s := mysqlStoreOrSkip(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r := s.Runs()
	_ = r.PutBackup(ctx, backup.BackupRun{ID: "a1", TenantStack: "a", Status: backup.StatusSucceeded, StartedAt: now.Add(-2 * time.Hour)})
	_ = r.PutBackup(ctx, backup.BackupRun{ID: "a2", TenantStack: "a", Status: backup.StatusSucceeded, StartedAt: now})
	_ = r.PutBackup(ctx, backup.BackupRun{ID: "b1", TenantStack: "b", Status: backup.StatusSucceeded, StartedAt: now})

	aRuns, err := r.ListBackupsByTenant(ctx, "a")
	if err != nil {
		t.Fatalf("ListBackupsByTenant: %v", err)
	}
	if len(aRuns) != 2 || aRuns[0].ID != "a2" {
		t.Errorf("tenant 'a' runs: got %v", aRuns)
	}
	bRuns, _ := r.ListBackupsByTenant(ctx, "b")
	if len(bRuns) != 1 {
		t.Errorf("tenant 'b' runs: got %d", len(bRuns))
	}
}

func TestMySQL_SnapshotsAndSamples(t *testing.T) {
	s := mysqlStoreOrSkip(t)
	ctx := context.Background()
	snaps := s.Snapshots()
	now := time.Now().UTC()

	if err := snaps.PutDbSnapshot(ctx, stats.DbSnapshot{Project: "p", Database: "db1", Ts: now, ArchiveBytes: 1024}); err != nil {
		t.Fatalf("PutDbSnapshot: %v", err)
	}
	rows, _ := snaps.SnapshotsForProject(ctx, "p", time.Time{})
	if len(rows) != 1 || rows[0].ID == 0 {
		t.Fatalf("SnapshotsForProject rows=%d id=%d", len(rows), rows[0].ID)
	}
	snapID := rows[0].ID

	// Since cutoff.
	_ = snaps.PutDbSnapshot(ctx, stats.DbSnapshot{Project: "p", Database: "db1", Ts: now.Add(-48 * time.Hour), ArchiveBytes: 500})
	recent, _ := snaps.SnapshotsForProject(ctx, "p", now.Add(-24*time.Hour))
	if len(recent) != 1 {
		t.Errorf("since-cutoff filter: got %d rows, want 1", len(recent))
	}

	samples := []stats.ObjectSample{{Name: "col1"}, {Name: "col2"}}
	if err := snaps.PutObjectSamples(ctx, "p", "db1", "key", snapID, samples); err != nil {
		t.Fatalf("PutObjectSamples: %v", err)
	}
	got, _ := snaps.ObjectSamplesForProject(ctx, "p", "db1", time.Time{})
	if len(got) != 2 {
		t.Errorf("samples: got %d", len(got))
	}
	// Replay replace.
	_ = snaps.PutObjectSamples(ctx, "p", "db1", "key", snapID, []stats.ObjectSample{{Name: "only"}})
	got, _ = snaps.ObjectSamplesForProject(ctx, "p", "db1", time.Time{})
	if len(got) != 1 || got[0].Name != "only" {
		t.Errorf("replay: got %v", got)
	}
}

func TestMySQL_RunResultsForProject(t *testing.T) {
	s := mysqlStoreOrSkip(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r := s.Runs()
	_ = r.PutBackup(ctx, backup.BackupRun{ID: "r1", TenantStack: "p", Status: backup.StatusSucceeded, StartedAt: now.Add(-1 * time.Hour)})
	_ = r.PutBackup(ctx, backup.BackupRun{ID: "r2", TenantStack: "p", Status: backup.StatusFailed, StartedAt: now})
	_ = r.PutBackup(ctx, backup.BackupRun{ID: "r3", TenantStack: "p", Status: backup.StatusSucceeded, StartedAt: now.Add(-48 * time.Hour)})

	all, _ := s.Snapshots().RunResultsForProject(ctx, "p", time.Time{})
	if len(all) != 3 {
		t.Errorf("all results: got %d, want 3", len(all))
	}
	recent, _ := s.Snapshots().RunResultsForProject(ctx, "p", now.Add(-24*time.Hour))
	if len(recent) != 2 {
		t.Errorf("since-cutoff: got %d, want 2", len(recent))
	}
}
