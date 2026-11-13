// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
)

// TestProjectDbResults — pure helper, no store.
func TestProjectDbResults(t *testing.T) {
	ts := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	runs := []backup.BackupRun{
		{StartedAt: ts("2026-11-10T00:00:00Z"), Status: backup.StatusSucceeded},
		{StartedAt: ts("2026-11-11T00:00:00Z"), Status: backup.StatusFailed},
		{StartedAt: ts("2026-11-12T00:00:00Z"), Status: backup.StatusRunning},
	}
	// since zero → all runs flow through, status mapping applied.
	got := ProjectDbResults(runs, time.Time{})
	if len(got) != 3 {
		t.Fatalf("want 3 results, got %d", len(got))
	}
	if got[0].Status != "ok" || got[1].Status != "failed" || got[2].Status != "running" {
		t.Errorf("status mapping: %v / %v / %v", got[0].Status, got[1].Status, got[2].Status)
	}
	// since cutoff drops older rows.
	got = ProjectDbResults(runs, ts("2026-11-11T12:00:00Z"))
	if len(got) != 1 {
		t.Errorf("want 1 after cutoff, got %d", len(got))
	}
	if got[0].Status != "running" {
		t.Errorf("post-cutoff status: %q", got[0].Status)
	}
}

// TestInMemorySnapshotStore_PutAndList — basic round-trip + project
// filter + since cutoff.
func TestInMemorySnapshotStore_PutAndList(t *testing.T) {
	store := NewInMemorySnapshotStore(NewInMemoryRunStore())
	ctx := context.Background()
	ts := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	snaps := []stats.DbSnapshot{
		{ID: 1, Project: "p1", Database: "db1", Ts: ts("2026-11-10T00:00:00Z"), ArchiveBytes: 100},
		{ID: 2, Project: "p1", Database: "db1", Ts: ts("2026-11-11T00:00:00Z"), ArchiveBytes: 200},
		{ID: 3, Project: "p2", Database: "db1", Ts: ts("2026-11-11T00:00:00Z"), ArchiveBytes: 300},
	}
	for _, s := range snaps {
		if err := store.PutDbSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.SnapshotsForProject(ctx, "p1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("p1 want 2, got %d", len(got))
	}
	// Newest-first ordering.
	if got[0].ID != 2 || got[1].ID != 1 {
		t.Errorf("newest-first order broken: %d / %d", got[0].ID, got[1].ID)
	}

	// Cutoff at 2026-11-10T12:00Z drops ID=1.
	got, err = store.SnapshotsForProject(ctx, "p1", ts("2026-11-10T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 2 {
		t.Errorf("cutoff filter broken: %v", got)
	}
}

// TestInMemorySnapshotStore_ObjectSamples — samples bind to their
// parent snapshot and come back only for the matching (project,database).
func TestInMemorySnapshotStore_ObjectSamples(t *testing.T) {
	store := NewInMemorySnapshotStore(NewInMemoryRunStore())
	ctx := context.Background()
	ts := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)

	// Two parent snapshots in different (project, database) pairs.
	_ = store.PutDbSnapshot(ctx, stats.DbSnapshot{ID: 10, Project: "p1", Database: "db1", Ts: ts})
	_ = store.PutDbSnapshot(ctx, stats.DbSnapshot{ID: 20, Project: "p1", Database: "db2", Ts: ts})
	_ = store.PutDbSnapshot(ctx, stats.DbSnapshot{ID: 30, Project: "p2", Database: "db1", Ts: ts})

	_ = store.PutObjectSamples(ctx, "p1", "db1", "k1", 10, []stats.ObjectSample{
		{Name: "coll-a", Kind: "collection"},
		{Name: "coll-b", Kind: "collection"},
	})
	_ = store.PutObjectSamples(ctx, "p1", "db2", "k2", 20, []stats.ObjectSample{
		{Name: "coll-c", Kind: "collection"},
	})
	_ = store.PutObjectSamples(ctx, "p2", "db1", "k3", 30, []stats.ObjectSample{
		{Name: "coll-d", Kind: "collection"},
	})

	got, err := store.ObjectSamplesForProject(ctx, "p1", "db1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 samples for (p1,db1), got %d", len(got))
	}
	for _, s := range got {
		if s.SnapshotID != 10 {
			t.Errorf("sample snapshotId leak across (project, database): %+v", s)
		}
	}
}

// TestInMemorySnapshotStore_RunResults — delegates to the RunStore and
// applies the cutoff.
func TestInMemorySnapshotStore_RunResults(t *testing.T) {
	runs := NewInMemoryRunStore()
	store := NewInMemorySnapshotStore(runs)
	ctx := context.Background()
	ts := func(s string) time.Time {
		v, _ := time.Parse(time.RFC3339, s)
		return v
	}
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "r1", TenantStack: "p1", StartedAt: ts("2026-11-10T00:00:00Z"), Status: backup.StatusSucceeded})
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "r2", TenantStack: "p1", StartedAt: ts("2026-11-12T00:00:00Z"), Status: backup.StatusFailed})
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "r3", TenantStack: "p2", StartedAt: ts("2026-11-12T00:00:00Z"), Status: backup.StatusSucceeded})

	out, err := store.RunResultsForProject(ctx, "p1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Errorf("p1 want 2 results, got %d", len(out))
	}
}

// Integration test — gated on BACKUP_SNAPSHOTS_TEST_URI. Mirrors the
// RunStore integration test pattern (store_mongo_test.go).
func TestMongoSnapshotStore_Integration(t *testing.T) {
	uri := os.Getenv("BACKUP_SNAPSHOTS_TEST_URI")
	if uri == "" {
		t.Skip("BACKUP_SNAPSHOTS_TEST_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	runs := NewInMemoryRunStore()
	store, err := NewMongoSnapshotStore(ctx, uri, "sc_component_backup_test", runs)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer store.Close(ctx)
	// Clean up any prior test rows.
	_, _ = store.snapshots.DeleteMany(ctx, map[string]any{"project": "snap-it"})
	_, _ = store.samples.DeleteMany(ctx, map[string]any{"project": "snap-it"})

	snap := stats.DbSnapshot{
		ID: time.Now().UnixNano(), Project: "snap-it", Database: "db1",
		Engine: "mongodb", Ts: time.Now().UTC(), Status: "ok", ArchiveBytes: 42,
	}
	if err := store.PutDbSnapshot(ctx, snap); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	if err := store.PutObjectSamples(ctx, "snap-it", "db1", "k1", snap.ID, []stats.ObjectSample{
		{Name: "coll-a", Kind: "collection"},
		{Name: "coll-b", Kind: "collection"},
	}); err != nil {
		t.Fatalf("put samples: %v", err)
	}
	got, err := store.SnapshotsForProject(ctx, "snap-it", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != snap.ID {
		t.Errorf("snapshot round-trip broken: %v", got)
	}
	samples, err := store.ObjectSamplesForProject(ctx, "snap-it", "db1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Errorf("samples round-trip broken: %v", samples)
	}

	// Replay with the same snapshotId must replace, not duplicate.
	if err := store.PutObjectSamples(ctx, "snap-it", "db1", "k1", snap.ID, []stats.ObjectSample{
		{Name: "coll-c", Kind: "collection"},
	}); err != nil {
		t.Fatal(err)
	}
	samples, _ = store.ObjectSamplesForProject(ctx, "snap-it", "db1", time.Time{})
	if len(samples) != 1 || samples[0].Name != "coll-c" {
		t.Errorf("samples replay-replace broken: %v", samples)
	}
}

// Compile-time assertions that both SnapshotStore impls satisfy the
// interface.
var (
	_ SnapshotStore = (*InMemorySnapshotStore)(nil)
	_ SnapshotStore = (*MongoSnapshotStore)(nil)
)
