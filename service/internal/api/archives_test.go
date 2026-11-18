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
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

func archivesRouter(cfg *ArchivesConfig) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/archives", cfg.Archives)
	return r
}

func fmtRun(id, stack string, finished time.Time, size int64, s3Path string, status backup.Status) backup.BackupRun {
	return backup.BackupRun{
		ID:          id,
		TenantStack: stack,
		Driver:      "mongodb",
		StartedAt:   finished.Add(-time.Minute),
		FinishedAt:  finished,
		Status:      status,
		S3Path:      s3Path,
		SizeBytes:   size,
	}
}

func decodeArchives(t *testing.T, rr *httptest.ResponseRecorder) archivesResponseJSON {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body=%q", rr.Code, rr.Body.String())
	}
	var resp archivesResponseJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%q", err, rr.Body.String())
	}
	return resp
}

// TestArchives_HappyPath_GroupsByDatabase — tenant + succeeded runs +
// a project retention policy yield a shape the SPA can render:
// one databases entry, archives newest-first, kept_by populated.
func TestArchives_HappyPath_GroupsByDatabase(t *testing.T) {
	ctx := context.Background()
	tenants := tenant.NewInMemoryStore()
	_ = tenants.Upsert(ctx, tenant.Record{
		StackName: "smoke-tier4",
		Resources: []tenant.Resource{{
			Type:       "mongodb-atlas",
			Name:       "smoke-mongo",
			Connection: map[string]string{"database": "widgets"},
		}},
	})
	runs := scheduler.NewInMemoryRunStore()
	// Five successful + one failed. Timestamps in UTC, formatted via
	// retention.FormatTS so retention.KeepReasons finds the archives.
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		finished := now.Add(-time.Duration(i) * 24 * time.Hour)
		ts := finished.UTC().Format("2006-01-02T15-04-05Z")
		run := fmtRun(
			"run-"+ts,
			"smoke-tier4",
			finished,
			int64(1024*(i+1)),
			"smoke-tier4/"+ts+".archive.gz",
			backup.StatusSucceeded,
		)
		_ = runs.PutBackup(ctx, run)
	}
	_ = runs.PutBackup(ctx, fmtRun("run-failed", "smoke-tier4", now, 0, "", backup.StatusFailed))

	projects := project.NewInMemoryStore()
	_, _ = projects.Create(ctx, project.Config{
		Name:      "smoke-tier4",
		Engine:    "mongodb-atlas",
		Retention: project.Retention{Last: 3, Daily: 2},
	})

	cfg := &ArchivesConfig{Tenants: tenants, Runs: runs, Projects: projects}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/smoke-tier4/archives", nil)
	rr := httptest.NewRecorder()
	archivesRouter(cfg).ServeHTTP(rr, r)

	resp := decodeArchives(t, rr)
	if n := len(resp.Databases); n != 1 {
		t.Fatalf("want 1 database group, got %d", n)
	}
	grp := resp.Databases[0]
	if grp.Database != "widgets" {
		t.Errorf("database label: got %q, want widgets", grp.Database)
	}
	if n := len(grp.Archives); n != 5 {
		t.Fatalf("want 5 succeeded archives, got %d", n)
	}
	// Newest first.
	for i := 1; i < len(grp.Archives); i++ {
		prev, _ := time.Parse(time.RFC3339, grp.Archives[i-1].TS)
		cur, _ := time.Parse(time.RFC3339, grp.Archives[i].TS)
		if !prev.After(cur) {
			t.Errorf("archive %d not newer than %d: %s vs %s", i-1, i, prev, cur)
		}
	}
	// Retention populated — at least one kept_by on the newest archive.
	if n := len(grp.Archives[0].KeptBy); n == 0 {
		t.Errorf("newest archive has no kept_by reasons")
	}
	// Restore hint set for the mongodb engine.
	if resp.Restore.Command == "" {
		t.Errorf("restore.command empty")
	}
}

// TestArchives_NoTenant_EmptyRender — SPA shows the ArchivesTab
// before any consumer push-registers. Return an empty render (no
// 404) so the operator sees an empty state, not an error.
func TestArchives_NoTenant_EmptyRender(t *testing.T) {
	cfg := &ArchivesConfig{
		Tenants:  tenant.NewInMemoryStore(),
		Runs:     scheduler.NewInMemoryRunStore(),
		Projects: project.NewInMemoryStore(),
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/absent-project/archives", nil)
	rr := httptest.NewRecorder()
	archivesRouter(cfg).ServeHTTP(rr, r)
	resp := decodeArchives(t, rr)
	if len(resp.Databases) != 0 || len(resp.Globals) != 0 {
		t.Errorf("absent tenant should return empty render, got %+v", resp)
	}
}

// TestArchives_FailedRunsExcluded — only StatusSucceeded runs appear;
// failed / cancelled / running runs would otherwise clutter the list
// with archives that don't exist on the bucket.
func TestArchives_FailedRunsExcluded(t *testing.T) {
	ctx := context.Background()
	tenants := tenant.NewInMemoryStore()
	_ = tenants.Upsert(ctx, tenant.Record{
		StackName: "p",
		Resources: []tenant.Resource{{Type: "mongodb", Connection: map[string]string{"database": "d"}}},
	})
	runs := scheduler.NewInMemoryRunStore()
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	_ = runs.PutBackup(ctx, fmtRun("ok", "p", now, 10, "p/2026-11-18T00-00-00Z.archive.gz", backup.StatusSucceeded))
	_ = runs.PutBackup(ctx, fmtRun("fail", "p", now, 0, "", backup.StatusFailed))
	_ = runs.PutBackup(ctx, fmtRun("cancel", "p", now, 0, "", backup.StatusCancelled))
	_ = runs.PutBackup(ctx, fmtRun("running", "p", now, 0, "p/partial.gz", backup.StatusRunning))

	cfg := &ArchivesConfig{Tenants: tenants, Runs: runs, Projects: project.NewInMemoryStore()}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/archives", nil)
	rr := httptest.NewRecorder()
	archivesRouter(cfg).ServeHTTP(rr, r)
	resp := decodeArchives(t, rr)
	if len(resp.Databases) != 1 || len(resp.Databases[0].Archives) != 1 {
		t.Errorf("only succeeded runs should appear, got %+v", resp)
	}
}

// TestArchives_UnknownEngine_GenericHint — the restore hint handler
// has a fallback when the tenant's engine isn't on the known list.
// SPA still renders a hint; operator reads the key, picks the tool.
func TestArchives_UnknownEngine_GenericHint(t *testing.T) {
	ctx := context.Background()
	tenants := tenant.NewInMemoryStore()
	_ = tenants.Upsert(ctx, tenant.Record{
		StackName: "p",
		Resources: []tenant.Resource{{Type: "unknown-engine"}},
	})
	cfg := &ArchivesConfig{
		Tenants:  tenants,
		Runs:     scheduler.NewInMemoryRunStore(),
		Projects: project.NewInMemoryStore(),
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/archives", nil)
	rr := httptest.NewRecorder()
	archivesRouter(cfg).ServeHTTP(rr, r)
	resp := decodeArchives(t, rr)
	if resp.Restore.Command == "" {
		t.Errorf("restore.command empty for unknown engine")
	}
}
