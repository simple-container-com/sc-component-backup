// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// Slice 1e.2 shipped the read-only projects surface
// (GET /api/v1/projects/{name} + /runs). Slice 1e.3 grows the full
// CRUD + setSecret against a NEW project.Store — persisted separately
// from the plugin-driven tenant.Record (see package project's doc for
// the split rationale).
//
// GET merges: tenant.Record produces engine + databases + uri_set;
// project.Config layers interval / retention / dump_args / etc on top.
// Either can be absent — an operator-created ad-hoc project has no
// tenant row, a plugin-pushed consumer with no backup-config has no
// project.Config yet.

// ProjectResponseJSON mirrors service/internal/ui/src/api/types.ts:168
// (`ProjectResponse`). `uri_set` reads the tenant's connection map; the
// rest are stub defaults until slice 1e.3 grows ProjectConfig persistence.
type ProjectResponseJSON struct {
	Config   ProjectConfigJSON `json:"config"`
	Revision string            `json:"revision"`
	URISet   bool              `json:"uri_set"`
}

type ProjectConfigJSON struct {
	Name              string        `json:"name"`
	Engine            string        `json:"engine"`
	Databases         any           `json:"databases"` // "all" | []string
	Interval          string        `json:"interval"`
	Timeout           string        `json:"timeout"`
	Retention         RetentionJSON `json:"retention"`
	Retries           int           `json:"retries"`
	ShrinkWarnPercent int           `json:"shrink_warn_percent"`
	DumpArgs          []string      `json:"dump_args"`
	Globals           bool          `json:"globals"`
	StartAt           *string       `json:"start_at,omitempty"`
}

type RetentionJSON struct {
	Last    int `json:"last"`
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

// RunsResponseJSON mirrors service/internal/ui/src/api/types.ts:206
// (`RunsResponse`). `has_more` is derived from `len == limit`; a
// proper cursor-based paginator is a follow-up when runs volume grows.
type RunsResponseJSON struct {
	Runs    []RunSummary `json:"runs"`
	HasMore bool         `json:"has_more"`
}

// ProjectsConfig carries the handler dependencies. Projects store is
// optional — a nil value makes POST/PUT/DELETE/SetSecret respond 501
// and keeps GET + /runs working against tenant.Store alone (the slice
// 1e.2 shape).
type ProjectsConfig struct {
	Tenants  tenant.Store
	Runs     scheduler.RunStore
	Projects project.Store
}

// Project is GET /api/v1/projects/{name}. Returns a ProjectResponse
// merged from tenant.Record (engine + databases + uri_set) and
// project.Config (interval / retention / etc). Either can be absent;
// ONE of them must exist for 200.
func (c *ProjectsConfig) Project(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	rec, hasTenant, tenantErr := c.Tenants.Get(r.Context(), name)
	if tenantErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "tenant lookup failed"})
		return
	}
	var cfg project.Config
	hasProject := false
	if c.Projects != nil {
		var err error
		cfg, hasProject, err = c.Projects.Get(r.Context(), name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "project config lookup failed"})
			return
		}
	}
	if !hasTenant && !hasProject {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	writeJSON(w, http.StatusOK, mergedResponse(rec, hasTenant, cfg, hasProject))
}

// CreateProject is POST /api/v1/projects with body
// `{config: ProjectConfig, uri?: string}`. Returns `{revision}` on 201.
func (c *ProjectsConfig) CreateProject(w http.ResponseWriter, r *http.Request) {
	if c.Projects == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "project.Store not wired"})
		return
	}
	var body struct {
		Config projectConfigBody `json:"config"`
		URI    string            `json:"uri,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return
	}
	if body.Config.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config.name is required", "field": "name"})
		return
	}
	if body.Config.Engine == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "config.engine is required", "field": "engine"})
		return
	}
	cfg := body.Config.toConfig()
	cfg.SecretURI = body.URI
	created, err := c.Projects.Create(r.Context(), cfg)
	if err != nil {
		if errors.Is(err, project.ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "project already exists"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create failed"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"revision": created.Revision})
}

// UpdateProject is PUT /api/v1/projects/{name} with body
// `{config: ProjectConfig, revision: string}`. Returns `{revision}` on 200.
func (c *ProjectsConfig) UpdateProject(w http.ResponseWriter, r *http.Request) {
	if c.Projects == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "project.Store not wired"})
		return
	}
	name := chi.URLParam(r, "name")
	var body struct {
		Config   projectConfigBody `json:"config"`
		Revision string            `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return
	}
	if body.Revision == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "revision is required", "field": "revision"})
		return
	}
	cfg := body.Config.toConfig()
	cfg.Name = name
	cfg.Revision = body.Revision
	updated, err := c.Projects.Update(r.Context(), cfg)
	if err != nil {
		switch {
		case errors.Is(err, project.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		case errors.Is(err, project.ErrRevisionMismatch):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "revision is stale"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "update failed"})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"revision": updated.Revision})
}

// DeleteProject is DELETE /api/v1/projects/{name}?revision=X. Returns 204.
func (c *ProjectsConfig) DeleteProject(w http.ResponseWriter, r *http.Request) {
	if c.Projects == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "project.Store not wired"})
		return
	}
	name := chi.URLParam(r, "name")
	revision := r.URL.Query().Get("revision")
	if revision == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "revision query param required"})
		return
	}
	err := c.Projects.SoftDelete(r.Context(), name, revision)
	if err != nil {
		switch {
		case errors.Is(err, project.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		case errors.Is(err, project.ErrRevisionMismatch):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "revision is stale"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delete failed"})
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetSecret is PUT /api/v1/projects/{name}/secret with body `{uri: string}`.
// Returns 204. Deliberately separate from UpdateProject — the SPA
// treats the secret as out-of-band from the config revision check.
func (c *ProjectsConfig) SetSecret(w http.ResponseWriter, r *http.Request) {
	if c.Projects == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "project.Store not wired"})
		return
	}
	name := chi.URLParam(r, "name")
	var body struct {
		URI string `json:"uri"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return
	}
	if body.URI == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uri is required", "field": "uri"})
		return
	}
	err := c.Projects.SetSecret(r.Context(), name, body.URI)
	if err != nil {
		if errors.Is(err, project.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "set secret failed"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// projectConfigBody mirrors the SPA's ProjectConfig TypeScript type on
// the wire; toConfig maps it onto the project.Config persistence shape.
// Separate struct keeps the request-parsing shape aligned with the SPA
// even when the persistence struct grows fields the SPA doesn't send.
type projectConfigBody struct {
	Name              string               `json:"name"`
	Engine            string               `json:"engine"`
	Databases         any                  `json:"databases"` // "all" | []string
	Interval          string               `json:"interval"`
	Timeout           string               `json:"timeout"`
	Retention         projectRetentionBody `json:"retention"`
	Retries           int                  `json:"retries"`
	ShrinkWarnPercent int                  `json:"shrink_warn_percent"`
	DumpArgs          []string             `json:"dump_args"`
	Globals           bool                 `json:"globals"`
	StartAt           *string              `json:"start_at,omitempty"`
}

type projectRetentionBody struct {
	Last    int `json:"last"`
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

func (b projectConfigBody) toConfig() project.Config {
	return project.Config{
		Name:              b.Name,
		Engine:            b.Engine,
		Databases:         b.Databases,
		Interval:          b.Interval,
		Timeout:           b.Timeout,
		Retention:         project.Retention(b.Retention),
		Retries:           b.Retries,
		ShrinkWarnPercent: b.ShrinkWarnPercent,
		DumpArgs:          b.DumpArgs,
		Globals:           b.Globals,
		StartAt:           b.StartAt,
	}
}

// RunsForProject is GET /api/v1/projects/{name}/runs?limit=N&before_id=X.
// Returns runs for one tenant, newest-first.
func (c *ProjectsConfig) RunsForProject(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	limit := parseLimit(r.URL.Query().Get("limit"), 50)

	rows, err := c.Runs.ListBackupsByTenant(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list failed"})
		return
	}
	// Newest first (store's ListBackupsByTenant already orders this way;
	// belt + braces).
	sort.Slice(rows, func(i, j int) bool { return rows[i].StartedAt.After(rows[j].StartedAt) })

	// Cursor-based paging via `before_id`: skip runs until we find the
	// given ID, then return the next `limit`. No ID → return the first
	// `limit`. Linear scan is fine while runs count per tenant stays low;
	// slice 1e.3 can push the filter into the RunStore.
	beforeID := r.URL.Query().Get("before_id")
	if beforeID != "" {
		for i := range rows {
			if rows[i].ID == beforeID {
				rows = rows[i+1:]
				break
			}
		}
	}
	more := false
	if len(rows) > limit {
		rows = rows[:limit]
		more = true
	}
	out := make([]RunSummary, 0, len(rows))
	for _, run := range rows {
		out = append(out, backupRunToSummary(run))
	}
	writeJSON(w, http.StatusOK, RunsResponseJSON{Runs: out, HasMore: more})
}

// mergedResponse builds a ProjectResponse from either / both sources.
// Precedence rule: project.Config fields WIN over tenant.Record when
// both are present, because project.Config is operator-edited
// explicitly and tenant.Record is plugin-derived state.
func mergedResponse(rec tenant.Record, hasTenant bool, cfg project.Config, hasProject bool) ProjectResponseJSON {
	resp := ProjectResponseJSON{
		Config: ProjectConfigJSON{
			Databases:         "all",
			Interval:          "1h",
			Timeout:           "1h",
			Retention:         RetentionJSON{Last: 7},
			Retries:           1,
			ShrinkWarnPercent: 50,
			DumpArgs:          []string{},
		},
	}
	if hasTenant {
		resp.Config.Name = rec.StackName
		databases := []string{}
		seen := map[string]struct{}{}
		for _, res := range rec.Resources {
			d := res.Connection["driver"]
			if d == "" {
				continue
			}
			if resp.Config.Engine == "" {
				resp.Config.Engine = d
			}
			if db := res.Connection["database"]; db != "" {
				if _, ok := seen[db]; !ok {
					seen[db] = struct{}{}
					databases = append(databases, db)
				}
			}
			if res.Connection["uri"] != "" {
				resp.URISet = true
			}
		}
		sort.Strings(databases)
		if len(databases) > 0 {
			resp.Config.Databases = databases
		}
		resp.Revision = rec.Version
	}
	if hasProject {
		if cfg.Name != "" {
			resp.Config.Name = cfg.Name
		}
		if cfg.Engine != "" {
			resp.Config.Engine = cfg.Engine
		}
		if cfg.Databases != nil {
			resp.Config.Databases = cfg.Databases
		}
		if cfg.Interval != "" {
			resp.Config.Interval = cfg.Interval
		}
		if cfg.Timeout != "" {
			resp.Config.Timeout = cfg.Timeout
		}
		if cfg.Retries != 0 {
			resp.Config.Retries = cfg.Retries
		}
		if cfg.ShrinkWarnPercent != 0 {
			resp.Config.ShrinkWarnPercent = cfg.ShrinkWarnPercent
		}
		if cfg.DumpArgs != nil {
			resp.Config.DumpArgs = cfg.DumpArgs
		}
		resp.Config.Retention = RetentionJSON(cfg.Retention)
		resp.Config.Globals = cfg.Globals
		resp.Config.StartAt = cfg.StartAt
		resp.Revision = cfg.Revision
		if cfg.SecretURI != "" {
			resp.URISet = true
		}
	}
	return resp
}

func parseLimit(raw string, def int) int {
	if raw == "" {
		return def
	}
	// Tiny atoi — keep it literal to avoid importing strconv just for a 3-char value.
	n := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		if n > 1000 {
			return 1000
		}
	}
	if n <= 0 {
		return def
	}
	return n
}
