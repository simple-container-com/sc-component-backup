// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// newSQLiteStoreForTest wires an FSStore + sqliteStore under t.TempDir().
func newSQLiteStoreForTest(t *testing.T) Store {
	t.Helper()
	root := t.TempDir()
	fs, err := storage.NewFSStore(context.Background(), storage.FSConfig{Root: root})
	if err != nil {
		t.Fatalf("FSStore: %v", err)
	}
	s, err := NewFromConfig(context.Background(), Config{Type: TypeSQLite}, fs)
	if err != nil {
		t.Fatalf("SQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestSQLite_MigrationsIdempotent(t *testing.T) {
	root := t.TempDir()
	fs, err := storage.NewFSStore(context.Background(), storage.FSConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// Open twice in sequence; the second open must not duplicate the
	// schema_version row or fail on existing tables.
	s1, err := NewFromConfig(context.Background(), Config{Type: TypeSQLite}, fs)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = s1.Close(context.Background())
	s2, err := NewFromConfig(context.Background(), Config{Type: TypeSQLite}, fs)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	store := s2.(*sqliteStore)
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&n); err != nil {
		t.Fatalf("schema_version count: %v", err)
	}
	if n != 1 {
		t.Errorf("schema_version row count: %d, want 1", n)
	}
	_ = s2.Close(context.Background())
}

func TestSQLite_RefusesNonFSStoreArchive(t *testing.T) {
	mem := storage.NewInMemoryStore()
	_, err := NewFromConfig(context.Background(), Config{Type: TypeSQLite}, mem)
	if err == nil {
		t.Fatal("expected refusal")
	}
	got := err.Error()
	if !strings.Contains(got, "sqlite") || !strings.Contains(got, "filesystem") {
		t.Errorf("error should mention sqlite + filesystem; got %q", got)
	}
}

func TestSQLite_TenantsRoundTrip(t *testing.T) {
	s := newSQLiteStoreForTest(t)
	ctx := context.Background()
	store := s.Tenants()

	r1 := tenant.Record{
		StackName: "alpha",
		Version:   "v1",
		Resources: []tenant.Resource{{Type: "mongodb", Name: "db", Connection: map[string]string{"driver": "mongodb", "database": "db1"}}},
	}
	r2 := tenant.Record{StackName: "beta", Version: "v1"}
	if err := store.Upsert(ctx, r1); err != nil {
		t.Fatalf("Upsert alpha: %v", err)
	}
	if err := store.Upsert(ctx, r2); err != nil {
		t.Fatalf("Upsert beta: %v", err)
	}
	got, ok, err := store.Get(ctx, "alpha")
	if err != nil || !ok {
		t.Fatalf("Get alpha: ok=%v err=%v", ok, err)
	}
	if got.Version != "v1" || len(got.Resources) != 1 || got.Resources[0].Connection["database"] != "db1" {
		t.Errorf("Get alpha: shape drift: %+v", got)
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("List: got %d, want 2", len(list))
	}
	// SoftDelete alpha.
	if err := store.SoftDelete(ctx, "alpha"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	_, ok, _ = store.Get(ctx, "alpha")
	if ok {
		t.Error("Get after SoftDelete: should be ok=false")
	}
	list, _ = store.List(ctx)
	if len(list) != 1 || list[0].StackName != "beta" {
		t.Errorf("List after SoftDelete: got %+v, want [beta]", list)
	}
	// Re-Upsert alpha resurrects.
	if err := store.Upsert(ctx, r1); err != nil {
		t.Fatalf("Re-Upsert: %v", err)
	}
	_, ok, _ = store.Get(ctx, "alpha")
	if !ok {
		t.Error("Upsert should bring a soft-deleted tenant back")
	}
}

func TestSQLite_ProjectCRUD(t *testing.T) {
	s := newSQLiteStoreForTest(t)
	ctx := context.Background()
	store := s.Projects()

	cfg := project.Config{
		Name:              "demo",
		Engine:            "mongodb",
		Interval:          "1h",
		Retention:         project.Retention{Last: 7},
		Retries:           3,
		ShrinkWarnPercent: 50,
	}
	created, err := store.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Revision == "" {
		t.Fatal("Create should mint a revision")
	}
	if _, err := store.Create(ctx, cfg); !errors.Is(err, project.ErrConflict) {
		t.Errorf("duplicate Create: want ErrConflict, got %v", err)
	}
	// Stale-revision Update.
	stale := created
	stale.Revision = "wrong"
	stale.Interval = "6h"
	if _, err := store.Update(ctx, stale); !errors.Is(err, project.ErrRevisionMismatch) {
		t.Errorf("stale Update: want ErrRevisionMismatch, got %v", err)
	}
	// Correct Update bumps revision.
	fresh := created
	fresh.Interval = "6h"
	updated, err := store.Update(ctx, fresh)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Revision == created.Revision {
		t.Error("Update should mint a new revision")
	}
	if updated.Interval != "6h" {
		t.Errorf("Update should apply new field: %q", updated.Interval)
	}
	// SetSecret keeps revision + lands the URI.
	if err := store.SetSecret(ctx, "demo", "mongodb://u:p@h/"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.Get(ctx, "demo")
	if got.SecretURI != "mongodb://u:p@h/" {
		t.Error("SetSecret didn't persist")
	}
	if got.Revision != updated.Revision {
		t.Error("SetSecret should NOT bump revision")
	}
	// Stale SoftDelete.
	if err := store.SoftDelete(ctx, "demo", "wrong"); !errors.Is(err, project.ErrRevisionMismatch) {
		t.Errorf("stale SoftDelete: want ErrRevisionMismatch, got %v", err)
	}
	if err := store.SoftDelete(ctx, "demo", updated.Revision); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	_, ok, _ := store.Get(ctx, "demo")
	if ok {
		t.Error("Get after SoftDelete: should be ok=false")
	}
	// Resurrection via Create.
	res, err := store.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("Create after SoftDelete: %v", err)
	}
	if res.Revision == updated.Revision {
		t.Error("Resurrected Create should mint a new revision")
	}
}

func TestSQLite_RunsNewestFirst(t *testing.T) {
	s := newSQLiteStoreForTest(t)
	ctx := context.Background()
	store := s.Runs()

	base := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b", "c"} {
		if err := store.PutBackup(ctx, backup.BackupRun{
			ID:          id,
			TenantStack: "tenant1",
			Status:      backup.StatusSucceeded,
			StartedAt:   base.Add(time.Duration(i) * time.Minute),
			SizeBytes:   int64((i + 1) * 100),
		}); err != nil {
			t.Fatalf("PutBackup %s: %v", id, err)
		}
	}
	// Another tenant's run — must not bleed into the tenant1 list.
	_ = store.PutBackup(ctx, backup.BackupRun{
		ID: "x", TenantStack: "tenant2", Status: backup.StatusSucceeded, StartedAt: base,
	})
	rows, err := store.ListBackupsByTenant(ctx, "tenant1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len: %d, want 3", len(rows))
	}
	if rows[0].ID != "c" || rows[1].ID != "b" || rows[2].ID != "a" {
		t.Errorf("order: %v", []string{rows[0].ID, rows[1].ID, rows[2].ID})
	}
	got, err := store.GetBackup(ctx, "b")
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if got.SizeBytes != 200 {
		t.Errorf("size: %d", got.SizeBytes)
	}
}

func TestSQLite_SnapshotsAndRunResults(t *testing.T) {
	s := newSQLiteStoreForTest(t)
	ctx := context.Background()
	snaps := s.Snapshots()
	runs := s.Runs()

	base := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

	for i, db := range []string{"db1", "db1", "db2"} {
		if err := snaps.PutDbSnapshot(ctx, stats.DbSnapshot{
			Project:      "proj",
			Database:     db,
			Ts:           base.Add(time.Duration(i) * time.Minute),
			ArchiveBytes: int64((i + 1) * 1000),
			Status:       "ok",
		}); err != nil {
			t.Fatalf("PutDbSnapshot %d: %v", i, err)
		}
	}
	got, err := snaps.SnapshotsForProject(ctx, "proj", time.Time{})
	if err != nil || len(got) != 3 {
		t.Fatalf("SnapshotsForProject: n=%d err=%v", len(got), err)
	}
	// Find a db1 snapshot to attach samples to.
	var db1Snap stats.DbSnapshot
	for _, sn := range got {
		if sn.Database == "db1" {
			db1Snap = sn
			break
		}
	}
	if err := snaps.PutObjectSamples(ctx, "proj", "db1", "k", db1Snap.ID, []stats.ObjectSample{
		{Name: "col1", Kind: "collection"},
		{Name: "col2", Kind: "collection"},
	}); err != nil {
		t.Fatalf("PutObjectSamples: %v", err)
	}
	samples, err := snaps.ObjectSamplesForProject(ctx, "proj", "db1", time.Time{})
	if err != nil || len(samples) != 2 {
		t.Fatalf("ObjectSamplesForProject db1: n=%d err=%v", len(samples), err)
	}
	// db2 filter returns empty.
	samples, _ = snaps.ObjectSamplesForProject(ctx, "proj", "db2", time.Time{})
	if len(samples) != 0 {
		t.Errorf("db2 samples should be empty; got %d", len(samples))
	}
	// Re-put with the same snapshot ID REPLACES prior samples.
	if err := snaps.PutObjectSamples(ctx, "proj", "db1", "k", db1Snap.ID, []stats.ObjectSample{
		{Name: "only", Kind: "collection"},
	}); err != nil {
		t.Fatalf("PutObjectSamples replay: %v", err)
	}
	samples, _ = snaps.ObjectSamplesForProject(ctx, "proj", "db1", time.Time{})
	if len(samples) != 1 || samples[0].Name != "only" {
		t.Errorf("replay should replace; got %+v", samples)
	}

	// RunResultsForProject projects from the run store.
	_ = runs.PutBackup(ctx, backup.BackupRun{
		ID: "r1", TenantStack: "proj", Status: backup.StatusSucceeded, StartedAt: base.Add(10 * time.Minute),
	})
	_ = runs.PutBackup(ctx, backup.BackupRun{
		ID: "r2", TenantStack: "proj", Status: backup.StatusFailed, StartedAt: base.Add(20 * time.Minute),
	})
	results, err := snaps.RunResultsForProject(ctx, "proj", time.Time{})
	if err != nil || len(results) != 2 {
		t.Fatalf("RunResultsForProject: n=%d err=%v", len(results), err)
	}
	results, _ = snaps.RunResultsForProject(ctx, "proj", base.Add(15*time.Minute))
	if len(results) != 1 {
		t.Errorf("since cutoff should trim; got %d", len(results))
	}
}
