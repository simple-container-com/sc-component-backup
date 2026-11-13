// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// newProjectsRouter builds a chi router with the CRUD handlers mounted —
// the handlers use chi.URLParam, so a httptest.Recorder against a bare
// handler wouldn't resolve {name}. Tests go through the router.
func newProjectsRouter(cfg *ProjectsConfig) chi.Router {
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}", cfg.Project)
	r.Post("/api/v1/projects", cfg.CreateProject)
	r.Put("/api/v1/projects/{name}", cfg.UpdateProject)
	r.Delete("/api/v1/projects/{name}", cfg.DeleteProject)
	r.Put("/api/v1/projects/{name}/secret", cfg.SetSecret)
	return r
}

func newProjectsCfgWithStores() *ProjectsConfig {
	return &ProjectsConfig{
		Tenants:  tenant.NewInMemoryStore(),
		Runs:     scheduler.NewInMemoryRunStore(),
		Projects: project.NewInMemoryStore(),
	}
}

func doJSON(t *testing.T, r chi.Router, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	}
	return rec, resp
}

func TestCreateProject_Happy(t *testing.T) {
	cfg := newProjectsCfgWithStores()
	rtr := newProjectsRouter(cfg)

	body := map[string]any{
		"config": map[string]any{
			"name":      "demo",
			"engine":    "mongodb",
			"databases": "all",
			"interval":  "1h",
			"timeout":   "1h",
			"retention": map[string]int{"last": 7},
			"retries":   3,
		},
		"uri": "mongodb://u:p@h/",
	}
	rec, resp := doJSON(t, rtr, http.MethodPost, "/api/v1/projects", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	revision, _ := resp["revision"].(string)
	if revision == "" {
		t.Fatal("expected a revision in the response")
	}

	// Follow-up GET returns the same shape.
	rec2, resp2 := doJSON(t, rtr, http.MethodGet, "/api/v1/projects/demo", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET after create: status %d, body %s", rec2.Code, rec2.Body.String())
	}
	if gotRev, _ := resp2["revision"].(string); gotRev != revision {
		t.Errorf("GET revision drift: %q vs %q", gotRev, revision)
	}
	if uriSet, _ := resp2["uri_set"].(bool); !uriSet {
		t.Error("GET after create with URI: uri_set should be true")
	}
}

func TestCreateProject_ConflictOnDuplicate(t *testing.T) {
	cfg := newProjectsCfgWithStores()
	rtr := newProjectsRouter(cfg)

	body := map[string]any{"config": map[string]any{"name": "demo", "engine": "mongodb"}}
	_, _ = doJSON(t, rtr, http.MethodPost, "/api/v1/projects", body)
	rec, resp := doJSON(t, rtr, http.MethodPost, "/api/v1/projects", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate create; got %d, %v", rec.Code, resp)
	}
}

func TestCreateProject_ValidationErrors(t *testing.T) {
	cfg := newProjectsCfgWithStores()
	rtr := newProjectsRouter(cfg)

	// Missing name.
	rec, resp := doJSON(t, rtr, http.MethodPost, "/api/v1/projects", map[string]any{
		"config": map[string]any{"engine": "mongodb"},
	})
	if rec.Code != http.StatusBadRequest || resp["field"] != "name" {
		t.Errorf("missing name: status %d, field %v", rec.Code, resp["field"])
	}

	// Missing engine.
	rec, resp = doJSON(t, rtr, http.MethodPost, "/api/v1/projects", map[string]any{
		"config": map[string]any{"name": "demo"},
	})
	if rec.Code != http.StatusBadRequest || resp["field"] != "engine" {
		t.Errorf("missing engine: status %d, field %v", rec.Code, resp["field"])
	}

	// Malformed JSON.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	rtr.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status %d, body %s", rec2.Code, rec2.Body.String())
	}
}

func TestUpdateProject_RevisionCheck(t *testing.T) {
	cfg := newProjectsCfgWithStores()
	rtr := newProjectsRouter(cfg)

	_, create := doJSON(t, rtr, http.MethodPost, "/api/v1/projects", map[string]any{
		"config": map[string]any{"name": "demo", "engine": "mongodb"},
	})
	rev := create["revision"].(string)

	// Stale revision → 409.
	rec, _ := doJSON(t, rtr, http.MethodPut, "/api/v1/projects/demo", map[string]any{
		"config":   map[string]any{"name": "demo", "engine": "mongodb", "interval": "6h"},
		"revision": "stale",
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("stale revision: status %d, want 409", rec.Code)
	}

	// Correct revision → 200 + new revision.
	rec, resp := doJSON(t, rtr, http.MethodPut, "/api/v1/projects/demo", map[string]any{
		"config":   map[string]any{"name": "demo", "engine": "mongodb", "interval": "6h"},
		"revision": rev,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status %d, body %s", rec.Code, rec.Body.String())
	}
	newRev := resp["revision"].(string)
	if newRev == rev {
		t.Error("Update should mint a new revision")
	}
	// Missing revision → 400.
	rec, _ = doJSON(t, rtr, http.MethodPut, "/api/v1/projects/demo", map[string]any{
		"config": map[string]any{"name": "demo", "engine": "mongodb"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing revision: status %d, want 400", rec.Code)
	}
}

func TestDeleteProject(t *testing.T) {
	cfg := newProjectsCfgWithStores()
	rtr := newProjectsRouter(cfg)

	_, create := doJSON(t, rtr, http.MethodPost, "/api/v1/projects", map[string]any{
		"config": map[string]any{"name": "demo", "engine": "mongodb"},
	})
	rev := create["revision"].(string)

	// Missing revision → 400.
	rec, _ := doJSON(t, rtr, http.MethodDelete, "/api/v1/projects/demo", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing revision: status %d, want 400", rec.Code)
	}

	// Stale → 409.
	rec, _ = doJSON(t, rtr, http.MethodDelete, "/api/v1/projects/demo?revision=stale", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("stale revision: status %d, want 409", rec.Code)
	}

	// Correct → 204.
	rec, _ = doJSON(t, rtr, http.MethodDelete, fmt.Sprintf("/api/v1/projects/demo?revision=%s", rev), nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("delete: status %d, want 204", rec.Code)
	}

	// Follow-up GET → 404.
	rec, _ = doJSON(t, rtr, http.MethodGet, "/api/v1/projects/demo", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete: status %d, want 404", rec.Code)
	}
}

func TestSetSecret(t *testing.T) {
	cfg := newProjectsCfgWithStores()
	rtr := newProjectsRouter(cfg)

	_, _ = doJSON(t, rtr, http.MethodPost, "/api/v1/projects", map[string]any{
		"config": map[string]any{"name": "demo", "engine": "mongodb"},
	})

	// Set.
	rec, _ := doJSON(t, rtr, http.MethodPut, "/api/v1/projects/demo/secret",
		map[string]string{"uri": "mongodb://u:p@h/"})
	if rec.Code != http.StatusNoContent {
		t.Errorf("set-secret: status %d, want 204", rec.Code)
	}

	// Missing uri → 400.
	rec, _ = doJSON(t, rtr, http.MethodPut, "/api/v1/projects/demo/secret",
		map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing uri: status %d, want 400", rec.Code)
	}

	// Unknown project → 404.
	rec, _ = doJSON(t, rtr, http.MethodPut, "/api/v1/projects/ghost/secret",
		map[string]string{"uri": "mongodb://u:p@h/"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: status %d, want 404", rec.Code)
	}

	// GET sees uri_set=true.
	_, resp := doJSON(t, rtr, http.MethodGet, "/api/v1/projects/demo", nil)
	if uriSet, _ := resp["uri_set"].(bool); !uriSet {
		t.Error("uri_set should be true after SetSecret")
	}
}

func TestProjectGet_TenantOnlyNoConfig(t *testing.T) {
	// A plugin-pushed tenant exists but no project.Config yet.
	tenants := tenant.NewInMemoryStore()
	_ = tenants.Upsert(context.Background(), tenant.Record{
		StackName: "pushed-by-plugin",
		Resources: []tenant.Resource{{Name: "mongo", Connection: map[string]string{"driver": "mongodb", "database": "db1", "uri": "mongodb://h/"}}},
	})
	cfg := &ProjectsConfig{Tenants: tenants, Runs: scheduler.NewInMemoryRunStore(), Projects: project.NewInMemoryStore()}
	rtr := newProjectsRouter(cfg)

	rec, resp := doJSON(t, rtr, http.MethodGet, "/api/v1/projects/pushed-by-plugin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: status %d, body %s", rec.Code, rec.Body.String())
	}
	config := resp["config"].(map[string]any)
	if config["engine"] != "mongodb" {
		t.Errorf("tenant-sourced engine: %v", config["engine"])
	}
	if uriSet, _ := resp["uri_set"].(bool); !uriSet {
		t.Error("tenant with URI should set uri_set=true")
	}
}

func TestProjectGet_ConfigWinsOverTenant(t *testing.T) {
	ctx := context.Background()
	tenants := tenant.NewInMemoryStore()
	_ = tenants.Upsert(ctx, tenant.Record{
		StackName: "demo",
		Resources: []tenant.Resource{{Name: "mongo", Connection: map[string]string{"driver": "mongodb", "database": "db1"}}},
	})
	projects := project.NewInMemoryStore()
	_, _ = projects.Create(ctx, project.Config{
		Name:     "demo",
		Engine:   "postgres", // operator-overrode engine
		Interval: "6h",       // operator-overrode interval
		Retries:  5,          // operator-overrode retries
	})

	cfg := &ProjectsConfig{Tenants: tenants, Runs: scheduler.NewInMemoryRunStore(), Projects: projects}
	rtr := newProjectsRouter(cfg)

	_, resp := doJSON(t, rtr, http.MethodGet, "/api/v1/projects/demo", nil)
	config := resp["config"].(map[string]any)
	if config["engine"] != "postgres" {
		t.Errorf("project.Config.Engine should win over tenant: got %v", config["engine"])
	}
	if config["interval"] != "6h" {
		t.Errorf("project.Config.Interval should win: got %v", config["interval"])
	}
	if config["retries"].(float64) != 5 {
		t.Errorf("project.Config.Retries should win: got %v", config["retries"])
	}
}
