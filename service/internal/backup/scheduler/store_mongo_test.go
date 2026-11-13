// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// runStoreTestEnv returns the Mongo URI to use for integration tests
// and a per-test unique DB name so parallel runs don't collide. Tests
// skip when BACKUP_RUNS_TEST_URI is unset — a CI runner without Mongo
// should see them green.
func runStoreTestEnv(t *testing.T) (string, string) {
	t.Helper()
	uri := strings.TrimSpace(os.Getenv("BACKUP_RUNS_TEST_URI"))
	if uri == "" {
		t.Skip("BACKUP_RUNS_TEST_URI not set; skipping Mongo integration test")
	}
	// Per-test DB so a developer running the suite doesn't see
	// cross-test state leak. Dropped in t.Cleanup.
	db := fmt.Sprintf("sc_component_backup_test_%s_%d",
		strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
	return uri, db
}

func newTestMongoRunStore(t *testing.T) *MongoRunStore {
	t.Helper()
	uri, dbName := runStoreTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := NewMongoRunStore(ctx, uri, dbName)
	if err != nil {
		t.Fatalf("NewMongoRunStore: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = s.db.Drop(cctx)
		_ = s.Close(cctx)
	})
	return s
}

func fixtureBackup(id, tenant string, t time.Time) backup.BackupRun {
	return backup.BackupRun{
		ID:                 id,
		TenantStack:        tenant,
		ResourceName:       "mongo",
		Driver:             "mongodb",
		StartedAt:          t,
		FinishedAt:         t.Add(90 * time.Second),
		Status:             backup.StatusSucceeded,
		S3Path:             fmt.Sprintf("forge-%s/db/%s.archive.gz", tenant, t.UTC().Format("20060102T150405Z")),
		SizeBytes:          1234567,
		RetentionExpiresAt: t.Add(14 * 24 * time.Hour),
		TriggerSource:      "schedule",
	}
}

func TestMongoRunStore_PutGetRoundtrip(t *testing.T) {
	s := newTestMongoRunStore(t)
	ctx := context.Background()
	want := fixtureBackup("run-1", "smoke-tenant", time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC))
	if err := s.PutBackup(ctx, want); err != nil {
		t.Fatalf("PutBackup: %v", err)
	}
	got, err := s.GetBackup(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	// Mongo stores times with ms precision; compare at that resolution.
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt: got %v, want %v", got.StartedAt, want.StartedAt)
	}
	if got.TenantStack != want.TenantStack || got.ResourceName != want.ResourceName ||
		got.Driver != want.Driver || got.Status != want.Status ||
		got.S3Path != want.S3Path || got.SizeBytes != want.SizeBytes ||
		got.TriggerSource != want.TriggerSource {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got, want)
	}
}

func TestMongoRunStore_UpdateReplaces(t *testing.T) {
	s := newTestMongoRunStore(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	run := fixtureBackup("run-1", "smoke-tenant", start)
	run.Status = backup.StatusRunning
	run.SizeBytes = 0
	if err := s.PutBackup(ctx, run); err != nil {
		t.Fatalf("Put: %v", err)
	}
	run.Status = backup.StatusSucceeded
	run.SizeBytes = 42
	if err := s.UpdateBackup(ctx, run); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.GetBackup(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != backup.StatusSucceeded || got.SizeBytes != 42 {
		t.Errorf("Update did not take: %+v", got)
	}
}

func TestMongoRunStore_ListNewestFirstPerTenant(t *testing.T) {
	s := newTestMongoRunStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	// Tenant A: 3 runs, out-of-order insertion.
	runs := []backup.BackupRun{
		fixtureBackup("a-mid", "tenant-a", base.Add(5*time.Minute)),
		fixtureBackup("a-old", "tenant-a", base),
		fixtureBackup("a-new", "tenant-a", base.Add(10*time.Minute)),
		// Tenant B: 2 runs — these must NOT bleed into tenant-a's list.
		fixtureBackup("b-1", "tenant-b", base.Add(2*time.Minute)),
		fixtureBackup("b-2", "tenant-b", base.Add(7*time.Minute)),
	}
	for _, r := range runs {
		if err := s.PutBackup(ctx, r); err != nil {
			t.Fatalf("Put %s: %v", r.ID, err)
		}
	}

	got, err := s.ListBackupsByTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows for tenant-a, got %d", len(got))
	}
	wantIDs := []string{"a-new", "a-mid", "a-old"}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("row %d: id = %q, want %q (full ids: %v)", i, got[i].ID, id, idsOf(got))
		}
	}
	// Cross-tenant isolation check.
	gotB, err := s.ListBackupsByTenant(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("List tenant-b: %v", err)
	}
	if len(gotB) != 2 {
		t.Errorf("expected 2 rows for tenant-b, got %d", len(gotB))
	}
	for _, r := range gotB {
		if r.TenantStack != "tenant-b" {
			t.Errorf("tenant-b list bled: got %+v", r)
		}
	}
}

func TestMongoRunStore_EmptyTenantReturnsEmpty(t *testing.T) {
	s := newTestMongoRunStore(t)
	ctx := context.Background()
	if err := s.PutBackup(ctx, fixtureBackup("x", "tenant-a", time.Now().UTC())); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.ListBackupsByTenant(ctx, "")
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty tenantStack should return 0 rows, got %d", len(got))
	}
}

func TestMongoRunStore_GetNotFound(t *testing.T) {
	s := newTestMongoRunStore(t)
	ctx := context.Background()
	_, err := s.GetBackup(ctx, "no-such-run")
	if err == nil {
		t.Fatalf("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should contain 'not found', got %v", err)
	}
	_, err = s.GetRestore(ctx, "no-such-restore")
	if err == nil {
		t.Fatalf("GetRestore should error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("GetRestore error: %v", err)
	}
}

func TestMongoRunStore_RestoreRoundtrip(t *testing.T) {
	s := newTestMongoRunStore(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	want := backup.RestoreRun{
		ID:           "restore-1",
		BackupRunID:  "backup-src-1",
		TenantStack:  "smoke-tenant",
		ResourceName: "mongo",
		Driver:       "mongodb",
		StartedAt:    start,
		FinishedAt:   start.Add(45 * time.Second),
		Status:       backup.StatusSucceeded,
		TriggeredBy:  "operator-smecsia",
	}
	if err := s.PutRestore(ctx, want); err != nil {
		t.Fatalf("PutRestore: %v", err)
	}
	// Update path
	want.Status = backup.StatusFailed
	want.Error = "mongorestore exit 1"
	if err := s.UpdateRestore(ctx, want); err != nil {
		t.Fatalf("UpdateRestore: %v", err)
	}
	got, err := s.GetRestore(ctx, "restore-1")
	if err != nil {
		t.Fatalf("GetRestore: %v", err)
	}
	if got.Status != backup.StatusFailed || got.Error != "mongorestore exit 1" ||
		got.BackupRunID != want.BackupRunID || got.TriggeredBy != want.TriggeredBy {
		t.Errorf("restore roundtrip mismatch: got %+v, want %+v", got, want)
	}
}

func TestMongoRunStore_InterfaceSatisfied(t *testing.T) {
	// Compile-time check: *MongoRunStore satisfies RunStore. Runs
	// regardless of BACKUP_RUNS_TEST_URI — belongs here because a
	// future interface change should fail the package's own test
	// package.
	var _ RunStore = (*MongoRunStore)(nil)
}

func idsOf(runs []backup.BackupRun) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}
