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
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
)

// buildStatsRouter wires the three stats handlers to a chi router so
// the httptest cases can hit real URL paths with path params.
func buildStatsRouter(cfg *StatsConfig) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/stats", cfg.ProjectStats)
	r.Get("/api/v1/projects/{name}/stats/objects", cfg.ObjectStats)
	r.Get("/api/v1/projects/{name}/stats/object-history", cfg.ObjectHistory)
	return r
}

// seedStats populates an InMemorySnapshotStore with 7 days of snapshots
// for a single (project, database) pair so the aggregator has enough
// rows to produce a non-trivial response.
func seedStats(t *testing.T, project, database string) (*scheduler.InMemorySnapshotStore, scheduler.RunStore) {
	t.Helper()
	runs := scheduler.NewInMemoryRunStore()
	store := scheduler.NewInMemorySnapshotStore(runs)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 7; i++ {
		ts := now.Add(-time.Duration(i) * 24 * time.Hour)
		id := int64(100 + i)
		snap := stats.DbSnapshot{
			ID: id, Project: project, Database: database,
			Engine: "mongodb", Ts: ts, Status: "ok",
			ArchiveBytes: int64(1000 + i*10),
		}
		if err := store.PutDbSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		if err := store.PutObjectSamples(ctx, project, database, "k", id, []stats.ObjectSample{
			{Name: "coll-a", Kind: "collection"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// One succeeded backup run so the DbResult projection sees
	// something non-empty.
	_ = runs.PutBackup(ctx, backup.BackupRun{
		ID: "r1", TenantStack: project, StartedAt: now.Add(-time.Hour),
		Status: backup.StatusSucceeded,
	})
	return store, runs
}

func TestProjectStats_OK(t *testing.T) {
	store, runs := seedStats(t, "p1", "db1")
	cfg := &StatsConfig{Snapshots: store, Runs: runs}
	r := buildStatsRouter(cfg)

	req := httptest.NewRequest("GET", "/api/v1/projects/p1/stats?days=7&metric=archive&tz=UTC", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// Spot-check the shape — ProjectStatsResponse carries `timeline`
	// (with inner `series`) + `databases` + `summary`.
	if _, ok := resp["timeline"]; !ok {
		t.Errorf("missing timeline: %v", resp)
	}
	if _, ok := resp["databases"]; !ok {
		t.Errorf("missing databases: %v", resp)
	}
	if _, ok := resp["summary"]; !ok {
		t.Errorf("missing summary: %v", resp)
	}
}

func TestProjectStats_BadDays(t *testing.T) {
	store, runs := seedStats(t, "p1", "db1")
	cfg := &StatsConfig{Snapshots: store, Runs: runs}
	r := buildStatsRouter(cfg)

	req := httptest.NewRequest("GET", "/api/v1/projects/p1/stats?days=42&metric=archive", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["field"] != "days" {
		t.Errorf("want field=days, got %v", resp)
	}
}

func TestProjectStats_BadMetric(t *testing.T) {
	store, runs := seedStats(t, "p1", "db1")
	cfg := &StatsConfig{Snapshots: store, Runs: runs}
	r := buildStatsRouter(cfg)

	req := httptest.NewRequest("GET", "/api/v1/projects/p1/stats?days=7&metric=bogus", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["field"] != "metric" {
		t.Errorf("want field=metric, got %v", resp)
	}
}

func TestObjectStats_RequiresDatabase(t *testing.T) {
	store, runs := seedStats(t, "p1", "db1")
	cfg := &StatsConfig{Snapshots: store, Runs: runs}
	r := buildStatsRouter(cfg)

	req := httptest.NewRequest("GET", "/api/v1/projects/p1/stats/objects?days=7&metric=data", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestObjectStats_OK(t *testing.T) {
	store, runs := seedStats(t, "p1", "db1")
	cfg := &StatsConfig{Snapshots: store, Runs: runs}
	r := buildStatsRouter(cfg)

	req := httptest.NewRequest("GET", "/api/v1/projects/p1/stats/objects?days=7&metric=data&database=db1&tz=UTC", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestObjectHistory_OK(t *testing.T) {
	store, runs := seedStats(t, "p1", "db1")
	cfg := &StatsConfig{Snapshots: store, Runs: runs}
	r := buildStatsRouter(cfg)

	req := httptest.NewRequest("GET", "/api/v1/projects/p1/stats/object-history?days=7&database=db1&tz=UTC", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestObjectHistory_RejectsAllTime(t *testing.T) {
	store, runs := seedStats(t, "p1", "db1")
	cfg := &StatsConfig{Snapshots: store, Runs: runs}
	r := buildStatsRouter(cfg)

	// ObjectHistory refuses days==0 per stats.ObjectHistory validation.
	req := httptest.NewRequest("GET", "/api/v1/projects/p1/stats/object-history?days=0&database=db1&tz=UTC", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// days=0 *is* an accepted value for `parseDays`, but ObjectHistory's
	// inner validation then refuses it with a StatsError; the handler
	// should surface that as 400.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
