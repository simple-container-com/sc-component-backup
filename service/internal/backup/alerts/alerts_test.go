// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package alerts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

type fakeRunLister struct {
	rowsByTenant map[string][]backup.BackupRun
	err          error
}

func (f *fakeRunLister) ListBackupsByTenant(_ context.Context, stack string) ([]backup.BackupRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rowsByTenant[stack], nil
}

func mkRun(id, stack string, finished time.Time, status backup.Status, errMsg string) backup.BackupRun {
	return backup.BackupRun{
		ID:          id,
		TenantStack: stack,
		StartedAt:   finished.Add(-time.Minute),
		FinishedAt:  finished,
		Status:      status,
		Error:       errMsg,
	}
}

// TestAlertIDFromRun_StableAndJSSafe — two runs with the same ID
// hash to the same int64; the range stays within JavaScript's
// Number.MAX_SAFE_INTEGER (2^53 - 1).
func TestAlertIDFromRun_StableAndJSSafe(t *testing.T) {
	a := AlertIDFromRun("run-abc-123")
	b := AlertIDFromRun("run-abc-123")
	if a != b {
		t.Errorf("hash not stable: %d vs %d", a, b)
	}
	if a < 0 || a > (1<<53)-1 {
		t.Errorf("id %d out of JS-safe range [0, 2^53-1]", a)
	}
	c := AlertIDFromRun("run-abc-124")
	if a == c {
		t.Errorf("different inputs collided to %d", a)
	}
}

// TestSynthesize_FailedAndCancelled — failed runs emit error-level
// alerts, cancelled runs emit warning, succeeded/running emit
// nothing.
func TestSynthesize_FailedAndCancelled(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	runs := &fakeRunLister{rowsByTenant: map[string][]backup.BackupRun{
		"tenant-a": {
			mkRun("ok", "tenant-a", now.Add(-time.Hour), backup.StatusSucceeded, ""),
			mkRun("fail", "tenant-a", now.Add(-2*time.Hour), backup.StatusFailed, "connection refused"),
			mkRun("cancel", "tenant-a", now.Add(-3*time.Hour), backup.StatusCancelled, ""),
			mkRun("running", "tenant-a", now.Add(-4*time.Hour), backup.StatusRunning, ""),
		},
	}}
	s := Synthesizer{Now: func() time.Time { return now }}
	out, err := s.Emit(context.Background(), []string{"tenant-a"}, runs, State{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 alerts (fail + cancel), got %d: %+v", len(out), out)
	}
	if out[0].Level != LevelError {
		t.Errorf("want error-level first, got %q", out[0].Level)
	}
	if out[1].Level != LevelWarning {
		t.Errorf("want warning-level second, got %q", out[1].Level)
	}
	// Error text includes the run's error message.
	if out[0].Text == "" || out[0].Project != "tenant-a" {
		t.Errorf("bad alert content: %+v", out[0])
	}
}

// TestSynthesize_LookbackSkip — runs older than Lookback are
// skipped so the feed doesn't grow unbounded.
func TestSynthesize_LookbackSkip(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	runs := &fakeRunLister{rowsByTenant: map[string][]backup.BackupRun{
		"t": {
			mkRun("recent-fail", "t", now.Add(-1*time.Hour), backup.StatusFailed, "oops"),
			mkRun("old-fail", "t", now.Add(-30*24*time.Hour), backup.StatusFailed, "ancient"),
		},
	}}
	s := Synthesizer{Lookback: 24 * time.Hour, Now: func() time.Time { return now }}
	out, _ := s.Emit(context.Background(), []string{"t"}, runs, State{})
	if len(out) != 1 {
		t.Fatalf("want 1 alert (lookback=24h), got %d", len(out))
	}
	if out[0].Project != "t" || out[0].Text == "" {
		t.Errorf("wrong alert surfaced: %+v", out[0])
	}
}

// TestSynthesize_PerIDDismissal — an alert id present in State.IDs
// is filtered out. Operator-dismissed noise doesn't come back.
func TestSynthesize_PerIDDismissal(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	runs := &fakeRunLister{rowsByTenant: map[string][]backup.BackupRun{
		"t": {mkRun("fail-a", "t", now.Add(-time.Hour), backup.StatusFailed, "x")},
	}}
	id := AlertIDFromRun("fail-a")
	state := State{IDs: map[int64]struct{}{id: {}}}
	s := Synthesizer{Now: func() time.Time { return now }}
	out, _ := s.Emit(context.Background(), []string{"t"}, runs, state)
	if len(out) != 0 {
		t.Errorf("dismissed alert came back: %+v", out)
	}
}

// TestSynthesize_BeforeCutoff — runs finished at or before the
// cutoff are filtered; newer runs pass.
func TestSynthesize_BeforeCutoff(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	cutoff := now.Add(-2 * time.Hour)
	runs := &fakeRunLister{rowsByTenant: map[string][]backup.BackupRun{
		"t": {
			mkRun("old", "t", now.Add(-3*time.Hour), backup.StatusFailed, "x"),
			mkRun("at-cutoff", "t", cutoff, backup.StatusFailed, "x"),
			mkRun("new", "t", now.Add(-time.Hour), backup.StatusFailed, "x"),
		},
	}}
	s := Synthesizer{Now: func() time.Time { return now }}
	out, _ := s.Emit(context.Background(), []string{"t"}, runs, State{Before: cutoff})
	if len(out) != 1 {
		t.Fatalf("want 1 alert (only `new`), got %d: %+v", len(out), out)
	}
}

// TestSynthesize_MaxPerTenant — a tenant in continuous failure
// doesn't flood the feed past MaxPerTenant.
func TestSynthesize_MaxPerTenant(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	rows := make([]backup.BackupRun, 0, 20)
	for i := 0; i < 20; i++ {
		rows = append(rows, mkRun(
			"fail-"+time.Duration(i).String(),
			"t", now.Add(-time.Duration(i)*time.Minute),
			backup.StatusFailed, "x",
		))
	}
	runs := &fakeRunLister{rowsByTenant: map[string][]backup.BackupRun{"t": rows}}
	s := Synthesizer{MaxPerTenant: 3, Now: func() time.Time { return now }}
	out, _ := s.Emit(context.Background(), []string{"t"}, runs, State{})
	if len(out) != 3 {
		t.Errorf("MaxPerTenant: want 3, got %d", len(out))
	}
}

// TestSynthesize_ListerError — a per-tenant list failure bubbles.
// Overview handler logs + renders an empty alerts list rather than
// a 500.
func TestSynthesize_ListerError(t *testing.T) {
	runs := &fakeRunLister{err: errors.New("mongo down")}
	s := Synthesizer{}
	_, err := s.Emit(context.Background(), []string{"t"}, runs, State{})
	if err == nil {
		t.Fatal("expected error from lister")
	}
}

// TestState_IsDismissed — the state's own filter function pins the
// precedence rules in one place the overview handler can rely on.
func TestState_IsDismissed(t *testing.T) {
	at := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	s := State{
		IDs:    map[int64]struct{}{42: {}},
		Before: at,
	}
	if !s.IsDismissed(42, at.Add(time.Hour)) {
		t.Error("id-match should dismiss regardless of ts")
	}
	if !s.IsDismissed(1, at.Add(-time.Hour)) {
		t.Error("ts before cutoff should dismiss regardless of id")
	}
	if !s.IsDismissed(1, at) {
		t.Error("ts at the cutoff should dismiss (inclusive)")
	}
	if s.IsDismissed(1, at.Add(time.Hour)) {
		t.Error("new, non-matching ts should NOT dismiss")
	}
	// Zero before + unknown id → not dismissed.
	empty := State{IDs: map[int64]struct{}{}}
	if empty.IsDismissed(1, at) {
		t.Error("empty state should never dismiss")
	}
}

// TestInMemoryDismissals_Lifecycle — Dismiss + DismissAll + State
// round-trip. DismissAll monotonic — moving the cutoff backward
// is a no-op.
func TestInMemoryDismissals_Lifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryDismissals()
	if err := s.Dismiss(ctx, 42); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	if err := s.Dismiss(ctx, 42); err != nil {
		t.Fatalf("Dismiss (idempotent): %v", err)
	}
	t1 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	if err := s.DismissAll(ctx, t1); err != nil {
		t.Fatalf("DismissAll t1: %v", err)
	}
	// Move cutoff backward → no-op (monotonic).
	if err := s.DismissAll(ctx, t1.Add(-time.Hour)); err != nil {
		t.Fatalf("DismissAll backward: %v", err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if _, ok := state.IDs[42]; !ok {
		t.Error("id 42 missing from state")
	}
	if !state.Before.Equal(t1) {
		t.Errorf("Before: got %s, want %s (no backward move)", state.Before, t1)
	}
	// Returned IDs map is a copy — mutating it must not touch the
	// store.
	state.IDs[99] = struct{}{}
	state2, _ := s.State(ctx)
	if _, ok := state2.IDs[99]; ok {
		t.Error("State.IDs is not defensively copied")
	}
}
