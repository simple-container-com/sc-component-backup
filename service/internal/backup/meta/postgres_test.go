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

// TestMeta_PostgresNewRefusesBadURI runs ALWAYS (no DB dependency). A
// malformed URI hits the driver's Open/Ping path and returns an error —
// confirms the dialect-selection wiring + fail-fast posture.
func TestMeta_PostgresNewRefusesBadURI(t *testing.T) {
	if _, err := newPostgresStore(context.Background(), "not-a-url"); err == nil {
		t.Fatal("expected error from newPostgresStore with bad URI")
	}
}

// pgStoreOrSkip is the integration harness — builds a store against
// BACKUP_PG_TEST_URI. Each test drops the schema's tables first to stay
// isolated.
func pgStoreOrSkip(t *testing.T) Store {
	t.Helper()
	uri := os.Getenv("BACKUP_PG_TEST_URI")
	if uri == "" {
		t.Skip("integration — needs BACKUP_PG_TEST_URI")
	}
	s, err := newPostgresStore(context.Background(), uri)
	if err != nil {
		t.Fatalf("newPostgresStore: %v", err)
	}
	// Drop + re-create every table so tests don't bleed. The migration
	// is idempotent; dropping is the quickest isolation.
	ps := s.(*postgresStore)
	for _, tbl := range []string{"object_samples", "db_snapshots", "restore_runs", "backup_runs", "projects", "tenants", "schema_version"} {
		if _, err := ps.db.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	if err := migratePostgres(context.Background(), ps.db); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestPostgres_MigrationsIdempotent(t *testing.T) {
	s := pgStoreOrSkip(t).(*postgresStore)
	// Re-migrate — must not duplicate the schema_version seed row.
	if err := migratePostgres(context.Background(), s.db); err != nil {
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

func TestPostgres_Tenants(t *testing.T) {
	s := pgStoreOrSkip(t)
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
		t.Errorf("Get: version drift %q", got.Version)
	}
	// SoftDelete then Get → ok=false.
	if err := s.Tenants().SoftDelete(ctx, "demo"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, ok, _ := s.Tenants().Get(ctx, "demo"); ok {
		t.Error("Get after SoftDelete should be invisible")
	}
	// Upsert resurrects the row.
	rec.Version = "2"
	if err := s.Tenants().Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert resurrect: %v", err)
	}
	got, ok, _ = s.Tenants().Get(ctx, "demo")
	if !ok || got.Version != "2" {
		t.Errorf("resurrect: ok=%v version=%q", ok, got.Version)
	}
}

func TestPostgres_ProjectsCRUD(t *testing.T) {
	s := pgStoreOrSkip(t)
	ctx := context.Background()
	created, err := s.Projects().Create(ctx, project.Config{Name: "p", Engine: "mongodb"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Create duplicate → ErrConflict.
	if _, err := s.Projects().Create(ctx, project.Config{Name: "p", Engine: "mongodb"}); err != project.ErrConflict {
		t.Errorf("duplicate Create: want ErrConflict, got %v", err)
	}
	// Update with stale revision → ErrRevisionMismatch.
	stale := created
	stale.Revision = "wrong"
	stale.Interval = "6h"
	if _, err := s.Projects().Update(ctx, stale); err != project.ErrRevisionMismatch {
		t.Errorf("stale Update: got %v", err)
	}
	// Good update bumps revision.
	created.Interval = "6h"
	updated, err := s.Projects().Update(ctx, created)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Revision == created.Revision {
		t.Error("Update should mint new revision")
	}
	// SetSecret preserves revision.
	if err := s.Projects().SetSecret(ctx, "p", "mongodb://h/"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	after, _, _ := s.Projects().Get(ctx, "p")
	if after.Revision != updated.Revision || after.SecretURI != "mongodb://h/" {
		t.Errorf("SetSecret: rev=%q uri=%q", after.Revision, after.SecretURI)
	}
	// SoftDelete.
	if err := s.Projects().SoftDelete(ctx, "p", updated.Revision); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, ok, _ := s.Projects().Get(ctx, "p"); ok {
		t.Error("Get after SoftDelete should be invisible")
	}
	// Create after SoftDelete resurrects.
	if _, err := s.Projects().Create(ctx, project.Config{Name: "p", Engine: "postgres"}); err != nil {
		t.Errorf("Create-after-SoftDelete resurrect: %v", err)
	}
}

func TestPostgres_RunsByTenant(t *testing.T) {
	s := pgStoreOrSkip(t)
	ctx := context.Background()
	now := time.Now().UTC()
	runs := s.Runs()
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "a1", TenantStack: "a", Status: backup.StatusSucceeded, StartedAt: now.Add(-2 * time.Hour), SizeBytes: 100})
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "a2", TenantStack: "a", Status: backup.StatusSucceeded, StartedAt: now, SizeBytes: 200})
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "b1", TenantStack: "b", Status: backup.StatusSucceeded, StartedAt: now, SizeBytes: 300})

	aRuns, err := runs.ListBackupsByTenant(ctx, "a")
	if err != nil {
		t.Fatalf("ListBackupsByTenant: %v", err)
	}
	if len(aRuns) != 2 {
		t.Errorf("tenant 'a' runs: got %d, want 2", len(aRuns))
	}
	// Newest first.
	if aRuns[0].ID != "a2" {
		t.Errorf("newest-first: got %q, want a2", aRuns[0].ID)
	}
	// No bleed.
	bRuns, _ := runs.ListBackupsByTenant(ctx, "b")
	if len(bRuns) != 1 || bRuns[0].ID != "b1" {
		t.Errorf("tenant 'b' runs: %v", bRuns)
	}
}

func TestPostgres_SnapshotsAndSamples(t *testing.T) {
	s := pgStoreOrSkip(t)
	ctx := context.Background()
	snaps := s.Snapshots()
	now := time.Now().UTC()

	snap := stats.DbSnapshot{Project: "p", Database: "db1", Ts: now, ArchiveBytes: 1024}
	if err := snaps.PutDbSnapshot(ctx, snap); err != nil {
		t.Fatalf("PutDbSnapshot: %v", err)
	}
	rows, err := snaps.SnapshotsForProject(ctx, "p", time.Time{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("SnapshotsForProject: rows=%d err=%v", len(rows), err)
	}
	// Autoassigned ID populated.
	if rows[0].ID == 0 {
		t.Error("autoassigned snapshot ID should be non-zero")
	}
	// Since cutoff filters out older rows.
	oldSnap := stats.DbSnapshot{Project: "p", Database: "db1", Ts: now.Add(-48 * time.Hour), ArchiveBytes: 500}
	_ = snaps.PutDbSnapshot(ctx, oldSnap)
	recent, _ := snaps.SnapshotsForProject(ctx, "p", now.Add(-24*time.Hour))
	if len(recent) != 1 {
		t.Errorf("since-cutoff filter: got %d rows, want 1", len(recent))
	}

	// Samples round-trip bound to the snapshot ID.
	rows, _ = snaps.SnapshotsForProject(ctx, "p", time.Time{})
	snapID := rows[0].ID
	samples := []stats.ObjectSample{{Name: "col1"}, {Name: "col2"}}
	if err := snaps.PutObjectSamples(ctx, "p", "db1", "key", snapID, samples); err != nil {
		t.Fatalf("PutObjectSamples: %v", err)
	}
	gotSamples, err := snaps.ObjectSamplesForProject(ctx, "p", "db1", time.Time{})
	if err != nil {
		t.Fatalf("ObjectSamplesForProject: %v", err)
	}
	if len(gotSamples) != 2 {
		t.Errorf("samples: got %d, want 2", len(gotSamples))
	}
	// PutObjectSamples replays replace.
	if err := snaps.PutObjectSamples(ctx, "p", "db1", "key", snapID, []stats.ObjectSample{{Name: "only"}}); err != nil {
		t.Fatalf("PutObjectSamples replay: %v", err)
	}
	gotSamples, _ = snaps.ObjectSamplesForProject(ctx, "p", "db1", time.Time{})
	if len(gotSamples) != 1 || gotSamples[0].Name != "only" {
		t.Errorf("replay: got %v", gotSamples)
	}
}

func TestPostgres_RunResultsForProject(t *testing.T) {
	s := pgStoreOrSkip(t)
	ctx := context.Background()
	now := time.Now().UTC()
	runs := s.Runs()
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "r1", TenantStack: "p", Status: backup.StatusSucceeded, StartedAt: now.Add(-1 * time.Hour), SizeBytes: 100})
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "r2", TenantStack: "p", Status: backup.StatusFailed, StartedAt: now, SizeBytes: 0})
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "r3", TenantStack: "p", Status: backup.StatusSucceeded, StartedAt: now.Add(-48 * time.Hour), SizeBytes: 500})

	// All-time: 3 results.
	all, err := s.Snapshots().RunResultsForProject(ctx, "p", time.Time{})
	if err != nil {
		t.Fatalf("RunResultsForProject: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all results: got %d, want 3", len(all))
	}
	// Since 24h → 2 results (r3 is 48h old).
	recent, _ := s.Snapshots().RunResultsForProject(ctx, "p", now.Add(-24*time.Hour))
	if len(recent) != 2 {
		t.Errorf("since-cutoff results: got %d, want 2", len(recent))
	}
}
