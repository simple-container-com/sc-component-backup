// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// Slice 1e.2: read-only projects surface. The SPA (cc22f7d) expects
// `GET /api/v1/projects/{name}` + `GET /api/v1/projects/{name}/runs`
// for its project-detail page; this file wires both against the existing
// tenant.Store + scheduler.RunStore.
//
// Not implemented in this slice (filed as 1e.3 follow-up on the roadmap
// item): POST/PUT/DELETE/setSecret endpoints. Those require a mutable
// per-project ProjectConfig (interval / retention / dump_args / secret
// URI) that the tenant.Record doesn't carry yet — the backup component's
// current tenant row is immutable from the plugin's push-register call,
// not operator-edited. Growing that shape lands in slice 1e.3 when the
// SPA's NewProject form needs to actually create something.

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

// ProjectsConfig carries the handler dependencies.
type ProjectsConfig struct {
	Tenants tenant.Store
	Runs    scheduler.RunStore
}

// Project is GET /api/v1/projects/{name}. Returns a ProjectResponse
// synthesised from the tenant.Record keyed by stackName.
func (c *ProjectsConfig) Project(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	rec, ok, err := c.Tenants.Get(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	writeJSON(w, http.StatusOK, projectRecordToResponse(rec))
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

func projectRecordToResponse(rec tenant.Record) ProjectResponseJSON {
	cfg := ProjectConfigJSON{
		Name:              rec.StackName,
		Interval:          "1h",
		Timeout:           "1h",
		Retention:         RetentionJSON{Last: 7},
		Retries:           1,
		ShrinkWarnPercent: 50,
		DumpArgs:          []string{},
		Databases:         "all",
	}
	databases := []string{}
	uriSet := false
	seen := map[string]struct{}{}
	for _, res := range rec.Resources {
		d := res.Connection["driver"]
		if d == "" {
			continue
		}
		if cfg.Engine == "" {
			cfg.Engine = d
		}
		if db := res.Connection["database"]; db != "" {
			if _, ok := seen[db]; !ok {
				seen[db] = struct{}{}
				databases = append(databases, db)
			}
		}
		if res.Connection["uri"] != "" {
			uriSet = true
		}
	}
	sort.Strings(databases)
	if len(databases) > 0 {
		cfg.Databases = databases
	}
	return ProjectResponseJSON{
		Config:   cfg,
		Revision: rec.Version,
		URISet:   uriSet,
	}
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
