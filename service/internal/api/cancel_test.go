// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

func cancelRouter(cfg *CancelConfig) chi.Router {
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/cancel", cfg.Cancel)
	return r
}

func TestCancel_FlipsRunningToCancelled(t *testing.T) {
	ctx := context.Background()
	runs := scheduler.NewInMemoryRunStore()
	_ = runs.PutBackup(ctx, backup.BackupRun{
		ID:          "run-1",
		TenantStack: "demo",
		Status:      backup.StatusRunning,
		StartedAt:   time.Now(),
	})
	cfg := &CancelConfig{Runs: runs}
	rtr := cancelRouter(cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/demo/cancel", nil)
	rtr.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["run_id"] != "run-1" {
		t.Errorf("run_id: got %q, want run-1", resp["run_id"])
	}

	got, _ := runs.GetBackup(ctx, "run-1")
	if got.Status != backup.StatusCancelled {
		t.Errorf("status after cancel: got %q, want %q", got.Status, backup.StatusCancelled)
	}
	if got.FinishedAt.IsZero() {
		t.Error("FinishedAt should be stamped on cancel")
	}
}

func TestCancel_PicksNewestRunning(t *testing.T) {
	ctx := context.Background()
	runs := scheduler.NewInMemoryRunStore()
	now := time.Now()
	// Older running run + a finished one afterwards + a newer running one.
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "old", TenantStack: "demo", Status: backup.StatusRunning, StartedAt: now.Add(-2 * time.Hour)})
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "done", TenantStack: "demo", Status: backup.StatusSucceeded, StartedAt: now.Add(-1 * time.Hour)})
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "new", TenantStack: "demo", Status: backup.StatusRunning, StartedAt: now})
	cfg := &CancelConfig{Runs: runs}
	rtr := cancelRouter(cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/demo/cancel", nil)
	rtr.ServeHTTP(rec, req)
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["run_id"] != "new" {
		t.Errorf("Cancel should pick the newest running run; got %q", resp["run_id"])
	}
	// The older running run stays running (the handler cancels one at a time).
	old, _ := runs.GetBackup(ctx, "old")
	if old.Status != backup.StatusRunning {
		t.Errorf("old running run should stay running; got %q", old.Status)
	}
}

func TestCancel_IdempotentWhenNoRunning(t *testing.T) {
	ctx := context.Background()
	runs := scheduler.NewInMemoryRunStore()
	_ = runs.PutBackup(ctx, backup.BackupRun{ID: "done", TenantStack: "demo", Status: backup.StatusSucceeded, StartedAt: time.Now()})
	cfg := &CancelConfig{Runs: runs}
	rtr := cancelRouter(cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/demo/cancel", nil)
	rtr.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["run_id"] != "" {
		t.Errorf("Cancel with nothing running: run_id should be empty; got %q", resp["run_id"])
	}
}

func TestCancel_UnknownTenantReturnsEmpty(t *testing.T) {
	runs := scheduler.NewInMemoryRunStore()
	cfg := &CancelConfig{Runs: runs}
	rtr := cancelRouter(cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/ghost/cancel", nil)
	rtr.ServeHTTP(rec, req)
	// No tenant matches → empty run_id, 200 (idempotent — operator
	// clicked Cancel on a deleted project, don't 404).
	if rec.Code != http.StatusOK {
		t.Errorf("unknown tenant: status %d, want 200", rec.Code)
	}
}
