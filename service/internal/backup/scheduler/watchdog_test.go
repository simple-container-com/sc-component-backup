// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// silentLog is a logger that discards every record — keeps test output
// clean when a case intentionally triggers a Warn.
func silentLog() *slog.Logger { return slog.New(slog.NewTextHandler(discardWriter{}, nil)) }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestReconcileStuckRuns_MarksOldRunningAsCancelled — a run stuck in
// `running` past the cutoff is reclassified; FinishedAt + Error are
// populated.
func TestReconcileStuckRuns_MarksOldRunningAsCancelled(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryRunStore()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	stuck := backup.BackupRun{
		ID:          "r-stuck",
		TenantStack: "alpha",
		Status:      backup.StatusRunning,
		StartedAt:   now.Add(-3 * time.Hour), // older than the 2h default cutoff
	}
	_ = store.PutBackup(ctx, stuck)

	n := ReconcileStuckRuns(ctx, store, silentLog(), now, 2*time.Hour)
	if n != 1 {
		t.Fatalf("want 1 reconciled, got %d", n)
	}
	got, err := store.GetBackup(ctx, "r-stuck")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != backup.StatusCancelled {
		t.Errorf("Status: got %q, want %q", got.Status, backup.StatusCancelled)
	}
	if !got.FinishedAt.Equal(now) {
		t.Errorf("FinishedAt: got %s, want %s", got.FinishedAt, now)
	}
	if got.Error == "" {
		t.Error("Error should carry a reason")
	}
}

// TestReconcileStuckRuns_PreservesFreshRunning — a run still inside
// the cutoff window stays as `running`.
func TestReconcileStuckRuns_PreservesFreshRunning(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryRunStore()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	_ = store.PutBackup(ctx, backup.BackupRun{
		ID:        "r-fresh",
		Status:    backup.StatusRunning,
		StartedAt: now.Add(-10 * time.Minute),
	})

	n := ReconcileStuckRuns(ctx, store, silentLog(), now, 2*time.Hour)
	if n != 0 {
		t.Fatalf("want 0 reconciled, got %d", n)
	}
	got, _ := store.GetBackup(ctx, "r-fresh")
	if got.Status != backup.StatusRunning {
		t.Errorf("fresh run reclassified: got %q, want running", got.Status)
	}
}

// TestReconcileStuckRuns_SkipsTerminalStates — succeeded/failed/
// cancelled rows are untouched.
func TestReconcileStuckRuns_SkipsTerminalStates(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryRunStore()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, s := range []backup.Status{backup.StatusSucceeded, backup.StatusFailed, backup.StatusCancelled} {
		_ = store.PutBackup(ctx, backup.BackupRun{
			ID:        "r-" + string(s),
			Status:    s,
			StartedAt: now.Add(-3 * time.Hour),
		})
	}
	n := ReconcileStuckRuns(ctx, store, silentLog(), now, 2*time.Hour)
	if n != 0 {
		t.Fatalf("want 0 reconciled (terminal states skipped), got %d", n)
	}
}

// TestReconcileStuckRuns_DefaultCutoff — cutoff = 0 falls back to
// a 2h default; a 3h-old run is still reclassified.
func TestReconcileStuckRuns_DefaultCutoff(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryRunStore()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	_ = store.PutBackup(ctx, backup.BackupRun{
		ID:        "r-stuck",
		Status:    backup.StatusRunning,
		StartedAt: now.Add(-3 * time.Hour),
	})
	if n := ReconcileStuckRuns(ctx, store, silentLog(), now, 0); n != 1 {
		t.Errorf("default cutoff: want 1 reconciled, got %d", n)
	}
}

// TestReconcileStuckRuns_MixedTenantsAndStates — reconciles across
// tenants + counts correctly when some pass + some fail cutoff.
func TestReconcileStuckRuns_MixedTenantsAndStates(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryRunStore()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	// 2 stuck across 2 tenants, 1 fresh, 1 succeeded
	_ = store.PutBackup(ctx, backup.BackupRun{ID: "r1", TenantStack: "alpha", Status: backup.StatusRunning, StartedAt: now.Add(-5 * time.Hour)})
	_ = store.PutBackup(ctx, backup.BackupRun{ID: "r2", TenantStack: "beta", Status: backup.StatusPending, StartedAt: now.Add(-5 * time.Hour)})
	_ = store.PutBackup(ctx, backup.BackupRun{ID: "r3", TenantStack: "alpha", Status: backup.StatusRunning, StartedAt: now.Add(-5 * time.Minute)})
	_ = store.PutBackup(ctx, backup.BackupRun{ID: "r4", TenantStack: "alpha", Status: backup.StatusSucceeded, StartedAt: now.Add(-5 * time.Hour), FinishedAt: now.Add(-4 * time.Hour)})
	if n := ReconcileStuckRuns(ctx, store, silentLog(), now, 2*time.Hour); n != 2 {
		t.Fatalf("mixed: want 2 reconciled (r1 + r2), got %d", n)
	}
	r1, _ := store.GetBackup(ctx, "r1")
	r2, _ := store.GetBackup(ctx, "r2")
	r3, _ := store.GetBackup(ctx, "r3")
	r4, _ := store.GetBackup(ctx, "r4")
	if r1.Status != backup.StatusCancelled || r2.Status != backup.StatusCancelled {
		t.Errorf("r1/r2 should be cancelled: %q / %q", r1.Status, r2.Status)
	}
	if r3.Status != backup.StatusRunning {
		t.Errorf("r3 should stay running: %q", r3.Status)
	}
	if r4.Status != backup.StatusSucceeded {
		t.Errorf("r4 should stay succeeded: %q", r4.Status)
	}
}

// TestReconcileStuckRuns_NilStore — guards against a nil call.
func TestReconcileStuckRuns_NilStore(t *testing.T) {
	n := ReconcileStuckRuns(context.Background(), nil, silentLog(), time.Now(), time.Hour)
	if n != 0 {
		t.Errorf("nil store: want 0, got %d", n)
	}
}
