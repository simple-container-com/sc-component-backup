// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"bytes"
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

func newColocatedTest(t *testing.T) Store {
	t.Helper()
	arch := storage.NewInMemoryStore()
	store, err := NewFromConfig(context.Background(), Config{Type: TypeColocated}, arch)
	if err != nil {
		t.Fatalf("NewFromConfig(colocated): %v", err)
	}
	return store
}

func TestColocated_TenantRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newColocatedTest(t)
	rec := tenant.Record{
		StackName: "demo",
		Version:   "v1",
		Resources: []tenant.Resource{{Type: "mongo", Name: "db1", Connection: map[string]string{"driver": "mongodb", "uri": "mongodb://h/"}}},
	}
	if err := s.Tenants().Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, ok, err := s.Tenants().Get(ctx, "demo")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.StackName != "demo" || len(got.Resources) != 1 {
		t.Errorf("Get returned unexpected record: %+v", got)
	}
	rows, _ := s.Tenants().List(ctx)
	if len(rows) != 1 {
		t.Fatalf("List: got %d", len(rows))
	}
	if err := s.Tenants().SoftDelete(ctx, "demo"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, ok, _ := s.Tenants().Get(ctx, "demo"); ok {
		t.Error("Get should return ok=false after SoftDelete")
	}
}

func TestColocated_RunsPerTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := newColocatedTest(t)
	now := time.Now().UTC()
	for _, run := range []backup.BackupRun{
		{ID: "a1", TenantStack: "A", StartedAt: now.Add(-2 * time.Hour), Status: backup.StatusSucceeded},
		{ID: "a2", TenantStack: "A", StartedAt: now.Add(-1 * time.Hour), Status: backup.StatusSucceeded},
		{ID: "b1", TenantStack: "B", StartedAt: now.Add(-30 * time.Minute), Status: backup.StatusRunning},
	} {
		if err := s.Runs().PutBackup(ctx, run); err != nil {
			t.Fatalf("PutBackup %s: %v", run.ID, err)
		}
	}
	aRuns, _ := s.Runs().ListBackupsByTenant(ctx, "A")
	if len(aRuns) != 2 {
		t.Errorf("A runs: got %d, want 2", len(aRuns))
	}
	bRuns, _ := s.Runs().ListBackupsByTenant(ctx, "B")
	if len(bRuns) != 1 || bRuns[0].ID != "b1" {
		t.Errorf("B runs bleed: %+v", bRuns)
	}
	if aRuns[0].ID != "a2" {
		t.Errorf("A runs newest-first: got %q", aRuns[0].ID)
	}
}

func TestColocated_GetBackupByID(t *testing.T) {
	ctx := context.Background()
	s := newColocatedTest(t)
	now := time.Now().UTC()
	_ = s.Runs().PutBackup(ctx, backup.BackupRun{ID: "needle", TenantStack: "hay", StartedAt: now, Status: backup.StatusSucceeded})
	_ = s.Runs().PutBackup(ctx, backup.BackupRun{ID: "noise", TenantStack: "other", StartedAt: now.Add(-time.Minute), Status: backup.StatusSucceeded})
	got, err := s.Runs().GetBackup(ctx, "needle")
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if got.ID != "needle" || got.TenantStack != "hay" {
		t.Errorf("GetBackup wrong row: %+v", got)
	}
	if _, err := s.Runs().GetBackup(ctx, "ghost"); err == nil {
		t.Error("GetBackup should error on unknown id")
	}
}

func TestColocated_ProjectRevisionCheck(t *testing.T) {
	ctx := context.Background()
	s := newColocatedTest(t)
	created, err := s.Projects().Create(ctx, project.Config{Name: "demo", Engine: "mongodb"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Projects().Create(ctx, project.Config{Name: "demo", Engine: "mongodb"}); !errors.Is(err, project.ErrConflict) {
		t.Errorf("Create dup: want ErrConflict, got %v", err)
	}
	stale := created
	stale.Revision = "wrong"
	stale.Interval = "6h"
	if _, err := s.Projects().Update(ctx, stale); !errors.Is(err, project.ErrRevisionMismatch) {
		t.Errorf("Update stale: want ErrRevisionMismatch, got %v", err)
	}
	_ = s.Projects().SetSecret(ctx, "demo", "mongodb://u:p@h/")
	fresh := created
	fresh.Interval = "6h"
	updated, err := s.Projects().Update(ctx, fresh)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Revision == created.Revision {
		t.Error("Update should mint new revision")
	}
	if updated.SecretURI != "mongodb://u:p@h/" {
		t.Error("Update should preserve SecretURI")
	}
	if err := s.Projects().SoftDelete(ctx, "demo", updated.Revision); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, ok, _ := s.Projects().Get(ctx, "demo"); ok {
		t.Error("Get after SoftDelete should return ok=false")
	}
}

func TestColocated_ProjectList_SkipsDeleted(t *testing.T) {
	ctx := context.Background()
	s := newColocatedTest(t)
	for _, n := range []string{"a", "b", "c"} {
		_, _ = s.Projects().Create(ctx, project.Config{Name: n, Engine: "mongodb"})
	}
	b, _, _ := s.Projects().Get(ctx, "b")
	_ = s.Projects().SoftDelete(ctx, "b", b.Revision)
	rows, _ := s.Projects().List(ctx)
	if len(rows) != 2 {
		t.Errorf("List: got %d, want 2", len(rows))
	}
}

func TestColocated_SnapshotsFilterByProjectDatabase(t *testing.T) {
	ctx := context.Background()
	s := newColocatedTest(t)
	now := time.Now().UTC()
	for _, snap := range []stats.DbSnapshot{
		{ID: 1, Project: "p1", Database: "db1", Ts: now.Add(-3 * time.Hour), ArchiveBytes: 100},
		{ID: 2, Project: "p1", Database: "db1", Ts: now.Add(-2 * time.Hour), ArchiveBytes: 110},
		{ID: 3, Project: "p1", Database: "db2", Ts: now.Add(-1 * time.Hour), ArchiveBytes: 120},
		{ID: 4, Project: "p2", Database: "db1", Ts: now, ArchiveBytes: 130},
	} {
		if err := s.Snapshots().PutDbSnapshot(ctx, snap); err != nil {
			t.Fatalf("PutDbSnapshot id=%d: %v", snap.ID, err)
		}
	}
	rows, _ := s.Snapshots().SnapshotsForProject(ctx, "p1", time.Time{})
	if len(rows) != 3 {
		t.Errorf("SnapshotsForProject p1: got %d, want 3", len(rows))
	}
	rows, _ = s.Snapshots().SnapshotsForProject(ctx, "p1", now.Add(-90*time.Minute))
	if len(rows) != 1 {
		t.Errorf("Cutoff: got %d", len(rows))
	}
}

func TestColocated_ObjectSamples(t *testing.T) {
	ctx := context.Background()
	s := newColocatedTest(t)
	now := time.Now().UTC()
	tsKey := now.Format("20060102T150405Z")
	_ = s.Snapshots().PutObjectSamples(ctx, "demo", "db1", "key-that-isnt-a-ts", 42,
		[]stats.ObjectSample{{SnapshotID: 42, Name: "col1", Kind: "collection"}})
	_ = s.Snapshots().PutObjectSamples(ctx, "demo", "db1", tsKey, 99,
		[]stats.ObjectSample{{SnapshotID: 99, Name: "col2", Kind: "collection"}})
	samples, _ := s.Snapshots().ObjectSamplesForProject(ctx, "demo", "db1", time.Time{})
	if len(samples) != 2 {
		t.Errorf("Samples: got %d, want 2", len(samples))
	}
}

func TestColocated_RunResultsDelegates(t *testing.T) {
	ctx := context.Background()
	s := newColocatedTest(t)
	now := time.Now().UTC()
	_ = s.Runs().PutBackup(ctx, backup.BackupRun{ID: "r1", TenantStack: "demo", StartedAt: now.Add(-time.Hour), Status: backup.StatusSucceeded})
	_ = s.Runs().PutBackup(ctx, backup.BackupRun{ID: "r2", TenantStack: "demo", StartedAt: now, Status: backup.StatusFailed})
	results, _ := s.Snapshots().RunResultsForProject(ctx, "demo", time.Time{})
	if len(results) != 2 {
		t.Fatalf("RunResults: got %d, want 2", len(results))
	}
}

func TestColocated_ForeignKeysUnderMetaDontCrashList(t *testing.T) {
	ctx := context.Background()
	arch := storage.NewInMemoryStore()
	_, _ = arch.Put(ctx, "_meta/tenants/evil.json", strings.NewReader("{not json"))
	_, _ = arch.Put(ctx, "_meta/garbage/weird.json", bytes.NewReader([]byte(`{"random":true}`)))
	store, _ := NewFromConfig(ctx, Config{Type: TypeColocated}, arch)
	_ = store.Tenants().Upsert(ctx, tenant.Record{StackName: "good"})
	rows, err := store.Tenants().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	foundGood := false
	for _, r := range rows {
		if r.StackName == "good" {
			foundGood = true
		}
	}
	if !foundGood {
		t.Error("Legitimate row should survive")
	}
}
