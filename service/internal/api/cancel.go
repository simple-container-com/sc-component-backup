// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// POST /api/v1/projects/{name}/cancel — the SPA's "Cancel" button on
// a running backup. The handler marks the most-recent running
// BackupRun as cancelled in the store; the actual worker reads the
// status on its next heartbeat and stops. If no run is in flight,
// the response is still 200 with run_id = "" (idempotent — the
// operator clicked Cancel on a run that already finished).
//
// In the v1 single-process smoke this is a straight status flip. The
// worker-side cancel plumbing (ecs.StopTask, mongodump signal
// handling) is a slice 1e.4 follow-up — flagged inline so the SPA's
// "cancelling" state stays accurate even without the worker-side
// enforcement.

// CancelConfig carries the handler's deps.
type CancelConfig struct {
	Runs scheduler.RunStore
}

// Cancel handles POST /api/v1/projects/{name}/cancel.
func (c *CancelConfig) Cancel(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ctx := r.Context()
	rows, err := c.Runs.ListBackupsByTenant(ctx, name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list runs failed"})
		return
	}
	// Newest first; the store's ListBackupsByTenant already returns
	// newest-first but belt + braces so a future store rearrangement
	// doesn't silently cancel the wrong run.
	sort.Slice(rows, func(i, j int) bool { return rows[i].StartedAt.After(rows[j].StartedAt) })
	for _, row := range rows {
		if row.Status != backup.StatusRunning && row.Status != backup.StatusPending {
			continue
		}
		row.Status = backup.StatusCancelled
		row.FinishedAt = time.Now().UTC()
		if err := c.Runs.UpdateBackup(ctx, row); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "update failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"run_id": row.ID})
		return
	}
	// No in-flight run — idempotent 200 with empty run_id.
	writeJSON(w, http.StatusOK, map[string]string{"run_id": ""})
}
