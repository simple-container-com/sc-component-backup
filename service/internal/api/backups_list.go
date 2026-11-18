// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// NewBackupsListHandler serves `GET /api/v1/backups`.
//
// Query params match the SPA's `allRuns()` call shape
// (service/internal/ui/src/api/types.ts + api/client.ts allRuns):
//
//   - `project` (optional) — tenant stack name. When set, filters to one tenant.
//   - `tenant`  (optional) — legacy alias for `project` (plugin era).
//     When BOTH are empty the handler returns the global cross-tenant
//     view, newest first; this is what the SPA's `/history` page calls
//     without a project filter.
//   - `limit` (optional, default 50, max 1000) — page size. For the
//     cross-tenant path the handler asks the store for `limit*4` rows
//     and then applies status/since filters in memory, so a filtered
//     page still produces close to `limit` rows.
//   - `before_id` (optional) — cursor (BackupRun.ID).
//   - `status` (optional) — one of running|ok|failed|interrupted|partial;
//     filters AFTER the store read so the DB index stays on startedAt.
//   - `since` (optional) — RFC3339 instant; filters runs started on
//     or after the instant.
//
// Response shape is `{runs: [RunSummary], has_more: bool}` to match
// RunsResponseJSON — the SPA's useAllRuns reads this directly.
func NewBackupsListHandler(runs scheduler.RunStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		project := q.Get("project")
		if project == "" {
			project = q.Get("tenant") // legacy alias
		}
		limit := parseLimit(q.Get("limit"), 50)
		statusFilter := q.Get("status")
		var sinceTime time.Time
		if s := q.Get("since"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				http.Error(w, `{"error":"since must be RFC3339"}`, http.StatusBadRequest)
				return
			}
			sinceTime = t
		}

		// Ask the store for enough rows that in-memory filtering still
		// produces close to `limit`. A factor of 4 is coarse but keeps
		// the cross-tenant /history useful even when most recent runs
		// are being filtered out (e.g. a "with problems only" view on
		// a fleet that mostly succeeds).
		storeLimit := limit * 4
		if storeLimit < limit {
			storeLimit = limit
		}

		var (
			rows []backup.BackupRun
			err  error
		)
		if project != "" {
			rows, err = runs.ListBackupsByTenant(r.Context(), project)
		} else {
			rows, err = runs.ListAllBackups(r.Context(), storeLimit)
		}
		if err != nil {
			http.Error(w, `{"error":"list backups failed"}`, http.StatusInternalServerError)
			return
		}

		if beforeID := q.Get("before_id"); beforeID != "" {
			for i := range rows {
				if rows[i].ID == beforeID {
					rows = rows[i+1:]
					break
				}
			}
		}

		filtered := rows[:0]
		for _, run := range rows {
			if statusFilter != "" && mapStatus(run.Status) != statusFilter {
				continue
			}
			if !sinceTime.IsZero() && run.StartedAt.Before(sinceTime) {
				continue
			}
			filtered = append(filtered, run)
		}
		rows = filtered

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
