// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"net/http"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// NewBackupsListHandler serves `GET /api/v1/backups`.
//
// Query params match the SPA's `allRuns()` call shape
// (service/internal/ui/src/api/types.ts + api/client.ts allRuns):
//
//   - `project` (optional) — tenant stack name. When set, filters to one tenant.
//   - `tenant`  (optional) — legacy alias for `project` (plugin era).
//     At least one of `project`/`tenant` is REQUIRED while
//     ListAllBackups isn't on the RunStore interface (filed as slice 1e.3
//     follow-up — needed for the SPA's History page cross-project view).
//   - `limit` (optional, default 50, max 1000) — page size.
//   - `before_id` (optional) — cursor.
//   - `status` / `since` — accepted for forward compat; filtering happens
//     client-side until the backend grows query params matching allRuns.
//
// Response shape is `{runs: [RunSummary], has_more: bool}` to match
// RunsResponseJSON — clients that used the old `{backups: [BackupRun]}`
// shape need to switch.
func NewBackupsListHandler(runs scheduler.RunStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		project := q.Get("project")
		if project == "" {
			project = q.Get("tenant") // legacy alias
		}
		if project == "" {
			http.Error(w, `{"error":"project (or tenant) query param required — cross-tenant list is slice 1e.3"}`, http.StatusBadRequest)
			return
		}
		rows, err := runs.ListBackupsByTenant(r.Context(), project)
		if err != nil {
			http.Error(w, `{"error":"list backups failed"}`, http.StatusInternalServerError)
			return
		}
		limit := parseLimit(q.Get("limit"), 50)
		if beforeID := q.Get("before_id"); beforeID != "" {
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RunsResponseJSON{Runs: out, HasMore: more})
	})
}
