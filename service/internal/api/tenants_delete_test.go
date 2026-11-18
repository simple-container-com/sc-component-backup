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
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// routeTo dispatches through a chi router so {name} resolves — the
// handler reads chi.URLParam.
func routeTo(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Method(method, "/api/v1/tenants/{name}", handler)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
	return rr
}

// seedTenantAndRuns puts one tenant with two runs (one with an S3
// archive, one without) so the counters can be exercised cleanly.
func seedTenantAndRuns(t *testing.T) (*tenant.InMemoryStore, *scheduler.InMemoryRunStore, *storage.InMemoryStore) {
	t.Helper()
	ten := tenant.NewInMemoryStore()
	runs := scheduler.NewInMemoryRunStore()
	arc := storage.NewInMemoryStore()
	ctx := context.Background()
	must := func(err error) {
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(ten.Upsert(ctx, tenant.Record{StackName: "alpha"}))
	// run with archive — Delete should succeed
	must(runs.PutBackup(ctx, backup.BackupRun{
		ID:          "r-with-archive",
		TenantStack: "alpha",
		Status:      backup.StatusSucceeded,
		StartedAt:   time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
		S3Path:      "alpha/archive-1.gz",
		SizeBytes:   100,
	}))
	_, _ = arc.Put(ctx, "alpha/archive-1.gz", strings.NewReader("payload"))
	// run without archive — counter should ignore it
	must(runs.PutBackup(ctx, backup.BackupRun{
		ID:          "r-no-archive",
		TenantStack: "alpha",
		Status:      backup.StatusFailed,
		StartedAt:   time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
	}))
	return ten, runs, arc
}

// TestTenantsDelete_HappyPath — tenant + runs + archive all gone,
// counters match.
func TestTenantsDelete_HappyPath(t *testing.T) {
	ten, runs, arc := seedTenantAndRuns(t)
	h := TenantsDelete(TenantsDeleteConfig{Tenants: ten, Runs: runs, Archive: arc})
	rr := routeTo(h, http.MethodDelete, "/api/v1/tenants/alpha")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body=%q", rr.Code, rr.Body.String())
	}
	var resp TenantsDeleteResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Tenant != "alpha" || resp.RunsDeleted != 2 || resp.ArchivesDeleted != 1 || resp.ArchivesAttempted != 1 {
		t.Errorf("counters: %+v", resp)
	}
	// Verify state is actually empty
	if _, ok, _ := ten.Get(context.Background(), "alpha"); ok {
		t.Error("tenant still present")
	}
	remaining, _ := runs.ListBackupsByTenant(context.Background(), "alpha")
	if len(remaining) != 0 {
		t.Errorf("runs still present: %d", len(remaining))
	}
	if _, err := arc.Get(context.Background(), "alpha/archive-1.gz"); err == nil {
		t.Error("archive still present")
	}
}

// TestTenantsDelete_UnknownTenant_404 — a typo returns 404 BEFORE
// any destructive call, so operator sees the mistake clearly.
func TestTenantsDelete_UnknownTenant_404(t *testing.T) {
	ten, runs, arc := seedTenantAndRuns(t)
	h := TenantsDelete(TenantsDeleteConfig{Tenants: ten, Runs: runs, Archive: arc})
	rr := routeTo(h, http.MethodDelete, "/api/v1/tenants/nope")
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
	// alpha untouched — the lookup failure must not have cascaded
	if _, ok, _ := ten.Get(context.Background(), "alpha"); !ok {
		t.Error("alpha was erroneously affected by a missing-tenant request")
	}
}

// TestTenantsDelete_NilArchive_SkipsS3 — nil Archive skips the S3
// scrub but still removes tenant row + run rows. Useful for
// deployments that want to keep the archives after the tenant is
// deregistered.
func TestTenantsDelete_NilArchive_SkipsS3(t *testing.T) {
	ten, runs, _ := seedTenantAndRuns(t)
	h := TenantsDelete(TenantsDeleteConfig{Tenants: ten, Runs: runs}) // Archive: nil
	rr := routeTo(h, http.MethodDelete, "/api/v1/tenants/alpha")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var resp TenantsDeleteResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.ArchivesDeleted != 0 {
		t.Errorf("ArchivesDeleted: got %d, want 0 with nil Archive", resp.ArchivesDeleted)
	}
	if resp.RunsDeleted != 2 {
		t.Errorf("RunsDeleted: got %d, want 2", resp.RunsDeleted)
	}
}

// TestTenantsDelete_NotWired_501 — a config without the required
// stores returns 501.
func TestTenantsDelete_NotWired_501(t *testing.T) {
	h := TenantsDelete(TenantsDeleteConfig{})
	rr := routeTo(h, http.MethodDelete, "/api/v1/tenants/alpha")
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("want 501, got %d", rr.Code)
	}
}
