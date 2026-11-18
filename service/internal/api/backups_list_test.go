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

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// fixtureRuns seeds three runs across two tenants with distinct
// start times and statuses, so each test can exercise a different
// filter combination without re-seeding.
func fixtureRuns(t *testing.T) *scheduler.InMemoryRunStore {
	t.Helper()
	s := scheduler.NewInMemoryRunStore()
	ctx := context.Background()
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	must := func(err error) {
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(s.PutBackup(ctx, backup.BackupRun{
		ID:           "r-a1-newest",
		TenantStack:  "alpha",
		ResourceName: "db1",
		Driver:       "mongodb",
		Status:       backup.StatusSucceeded,
		StartedAt:    t0.Add(2 * time.Hour),
		FinishedAt:   t0.Add(2*time.Hour + 5*time.Second),
		SizeBytes:    200,
	}))
	must(s.PutBackup(ctx, backup.BackupRun{
		ID:           "r-b1-mid",
		TenantStack:  "beta",
		ResourceName: "db2",
		Driver:       "postgres",
		Status:       backup.StatusFailed,
		StartedAt:    t0.Add(1 * time.Hour),
		FinishedAt:   t0.Add(1*time.Hour + 7*time.Second),
		Error:        "boom",
	}))
	must(s.PutBackup(ctx, backup.BackupRun{
		ID:           "r-a2-oldest",
		TenantStack:  "alpha",
		ResourceName: "db1",
		Driver:       "mongodb",
		Status:       backup.StatusSucceeded,
		StartedAt:    t0,
		FinishedAt:   t0.Add(3 * time.Second),
		SizeBytes:    100,
	}))
	return s
}

func decodeBackupsResp(t *testing.T, rr *httptest.ResponseRecorder) RunsResponseJSON {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body=%q", rr.Code, rr.Body.String())
	}
	var resp RunsResponseJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// TestBackupsList_CrossTenant — no project/tenant → walk every tenant,
// newest first. Fixes the "Bad request" 400 the SPA's /history page
// got until now.
func TestBackupsList_CrossTenant(t *testing.T) {
	h := NewBackupsListHandler(fixtureRuns(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/backups", nil))
	resp := decodeBackupsResp(t, rr)
	if len(resp.Runs) != 3 {
		t.Fatalf("want 3 runs across both tenants, got %d", len(resp.Runs))
	}
	if resp.Runs[0].ID != "r-a1-newest" || resp.Runs[1].ID != "r-b1-mid" || resp.Runs[2].ID != "r-a2-oldest" {
		t.Errorf("wrong ordering: %v %v %v", resp.Runs[0].ID, resp.Runs[1].ID, resp.Runs[2].ID)
	}
}

// TestBackupsList_ProjectFilter — explicit project= filters to just
// that tenant.
func TestBackupsList_ProjectFilter(t *testing.T) {
	h := NewBackupsListHandler(fixtureRuns(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/backups?project=alpha", nil))
	resp := decodeBackupsResp(t, rr)
	if len(resp.Runs) != 2 {
		t.Fatalf("want 2 alpha runs, got %d", len(resp.Runs))
	}
	for _, r := range resp.Runs {
		if r.Project != "alpha" {
			t.Errorf("non-alpha run leaked: %q", r.Project)
		}
	}
}

// TestBackupsList_LegacyTenantAlias — `tenant` is accepted as an alias
// for `project` (plugin-era client).
func TestBackupsList_LegacyTenantAlias(t *testing.T) {
	h := NewBackupsListHandler(fixtureRuns(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/backups?tenant=beta", nil))
	resp := decodeBackupsResp(t, rr)
	if len(resp.Runs) != 1 || resp.Runs[0].ID != "r-b1-mid" {
		t.Errorf("tenant alias did not route to beta: %+v", resp.Runs)
	}
}

// TestBackupsList_StatusFilter — status= filters AFTER the store read.
func TestBackupsList_StatusFilter(t *testing.T) {
	h := NewBackupsListHandler(fixtureRuns(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/backups?status=failed", nil))
	resp := decodeBackupsResp(t, rr)
	if len(resp.Runs) != 1 || resp.Runs[0].ID != "r-b1-mid" {
		t.Errorf("status filter wrong: %+v", resp.Runs)
	}
}

// TestBackupsList_SinceFilter — since=<RFC3339> cuts off older runs.
func TestBackupsList_SinceFilter(t *testing.T) {
	h := NewBackupsListHandler(fixtureRuns(t))
	// Threshold after the oldest, before the mid — keeps 2 runs.
	since := time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC).Format(time.RFC3339)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/backups?since="+since, nil))
	resp := decodeBackupsResp(t, rr)
	if len(resp.Runs) != 2 {
		t.Fatalf("want 2 runs since %s, got %d", since, len(resp.Runs))
	}
	for _, r := range resp.Runs {
		if r.ID == "r-a2-oldest" {
			t.Errorf("oldest leaked through since filter")
		}
	}
}

// TestBackupsList_SinceBadFormat — a non-RFC3339 since is a 400 (not
// a silent empty result).
func TestBackupsList_SinceBadFormat(t *testing.T) {
	h := NewBackupsListHandler(fixtureRuns(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/backups?since=not-a-date", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400 on bad since, got %d; body=%q", rr.Code, rr.Body.String())
	}
}

// TestBackupsList_PagingHasMore — with limit=1 the cross-tenant page
// must report has_more.
func TestBackupsList_PagingHasMore(t *testing.T) {
	h := NewBackupsListHandler(fixtureRuns(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/backups?limit=1", nil))
	resp := decodeBackupsResp(t, rr)
	if len(resp.Runs) != 1 || !resp.HasMore {
		t.Errorf("want 1 run + has_more=true, got %d runs has_more=%v", len(resp.Runs), resp.HasMore)
	}
	if resp.Runs[0].ID != "r-a1-newest" {
		t.Errorf("first page should be newest; got %q", resp.Runs[0].ID)
	}
}

// TestBackupsList_BeforeIDCursor — after the first page the cursor
// skips past that ID.
func TestBackupsList_BeforeIDCursor(t *testing.T) {
	h := NewBackupsListHandler(fixtureRuns(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/backups?limit=1&before_id=r-a1-newest", nil))
	resp := decodeBackupsResp(t, rr)
	if len(resp.Runs) != 1 || resp.Runs[0].ID != "r-b1-mid" {
		t.Errorf("cursor second page wrong: %+v", resp.Runs)
	}
}

// TestBackupsList_RunsHaveDatabases — the cross-tenant list uses the
// same backupRunToSummary path, so the databases array must land on
// every run (SPA contract; absence crashes DbTable).
func TestBackupsList_RunsHaveDatabases(t *testing.T) {
	h := NewBackupsListHandler(fixtureRuns(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/backups", nil))
	resp := decodeBackupsResp(t, rr)
	for _, r := range resp.Runs {
		if r.Databases == nil {
			t.Fatalf("run %q has nil Databases — SPA will crash", r.ID)
		}
		if len(r.Databases) != 1 {
			t.Errorf("run %q: want 1 db row, got %d", r.ID, len(r.Databases))
		}
	}
}
