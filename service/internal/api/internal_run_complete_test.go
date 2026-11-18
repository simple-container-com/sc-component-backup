// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// seedRun lands one `running` row for the handler to transition.
func seedRun(t *testing.T, id string) *scheduler.InMemoryRunStore {
	t.Helper()
	store := scheduler.NewInMemoryRunStore()
	_ = store.PutBackup(context.Background(), backup.BackupRun{
		ID:           id,
		TenantStack:  "alpha",
		ResourceName: "db1",
		Driver:       "mongodb",
		Status:       backup.StatusRunning,
		StartedAt:    time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
	})
	return store
}

func doComplete(t *testing.T, store scheduler.RunStore, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Method(http.MethodPost, "/api/v1/internal/runs/{id}/complete",
		InternalRunComplete(InternalRunCompleteConfig{Runs: store}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/runs/"+id+"/complete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rr, req)
	return rr
}

// TestInternalRunComplete_HappyOk — status ok → row transitions to
// succeeded with bytes + s3_path populated, Error cleared, FinishedAt
// non-zero.
func TestInternalRunComplete_HappyOk(t *testing.T) {
	store := seedRun(t, "r1")
	rr := doComplete(t, store, "r1", `{"status":"ok","bytes":1234,"s3_path":"alpha/db1/foo.gz"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body=%q", rr.Code, rr.Body.String())
	}
	run, _ := store.GetBackup(context.Background(), "r1")
	if run.Status != backup.StatusSucceeded {
		t.Errorf("Status: got %q, want succeeded", run.Status)
	}
	if run.SizeBytes != 1234 {
		t.Errorf("SizeBytes: got %d, want 1234", run.SizeBytes)
	}
	if run.S3Path != "alpha/db1/foo.gz" {
		t.Errorf("S3Path: got %q", run.S3Path)
	}
	if run.FinishedAt.IsZero() {
		t.Error("FinishedAt should be populated")
	}
}

// TestInternalRunComplete_HappyFailed — status failed → row transitions
// to failed with error populated.
func TestInternalRunComplete_HappyFailed(t *testing.T) {
	store := seedRun(t, "r1")
	rr := doComplete(t, store, "r1", `{"status":"failed","error":"mongodump: EOF"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	run, _ := store.GetBackup(context.Background(), "r1")
	if run.Status != backup.StatusFailed {
		t.Errorf("Status: got %q, want failed", run.Status)
	}
	if !strings.Contains(run.Error, "mongodump: EOF") {
		t.Errorf("Error: got %q", run.Error)
	}
}

// TestInternalRunComplete_UnknownRun_404 — missing run id → 404.
func TestInternalRunComplete_UnknownRun_404(t *testing.T) {
	store := seedRun(t, "r1")
	rr := doComplete(t, store, "nope", `{"status":"ok"}`)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestInternalRunComplete_AlreadyTerminal_Idempotent — row already
// in a terminal state (worker retry after transient network hiccup);
// handler returns 200 WITHOUT flipping the row again.
func TestInternalRunComplete_AlreadyTerminal_Idempotent(t *testing.T) {
	store := seedRun(t, "r1")
	ctx := context.Background()
	already, _ := store.GetBackup(ctx, "r1")
	already.Status = backup.StatusSucceeded
	already.FinishedAt = time.Date(2026, 11, 18, 12, 5, 0, 0, time.UTC)
	already.SizeBytes = 999
	_ = store.UpdateBackup(ctx, already)

	rr := doComplete(t, store, "r1", `{"status":"ok","bytes":1234}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	// Row NOT overwritten
	run, _ := store.GetBackup(ctx, "r1")
	if run.SizeBytes != 999 {
		t.Errorf("SizeBytes: terminal row was modified, got %d", run.SizeBytes)
	}
}

// TestInternalRunComplete_BadStatus_400 — anything other than
// ok|failed is rejected.
func TestInternalRunComplete_BadStatus_400(t *testing.T) {
	store := seedRun(t, "r1")
	rr := doComplete(t, store, "r1", `{"status":"unknown"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rr.Code)
	}
}

// TestInternalRunComplete_NotWired_501 — nil store surfaces 501.
func TestInternalRunComplete_NotWired_501(t *testing.T) {
	r := chi.NewRouter()
	r.Method(http.MethodPost, "/api/v1/internal/runs/{id}/complete",
		InternalRunComplete(InternalRunCompleteConfig{}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/runs/r1/complete", strings.NewReader(`{"status":"ok"}`))
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("want 501, got %d", rr.Code)
	}
}

// TestInternalRunComplete_ErrorTruncated — a verbose error longer than
// 512 bytes gets clamped so the Mongo doc doesn't bloat.
func TestInternalRunComplete_ErrorTruncated(t *testing.T) {
	store := seedRun(t, "r1")
	longErr := strings.Repeat("x", 600)
	body, _ := json.Marshal(map[string]any{"status": "failed", "error": longErr})
	rr := doComplete(t, store, "r1", string(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	run, _ := store.GetBackup(context.Background(), "r1")
	if len(run.Error) > 512 {
		t.Errorf("Error not truncated: len=%d", len(run.Error))
	}
}
