// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// NewRestoreHandler serves `POST /api/v1/tenants/{stack}/restore`.
// Body: `{"backupRunId": "<id>"}`. Returns 202 + RestoreRun{id,...}
// immediately; the actual restore streams in a background goroutine.
func NewRestoreHandler(sch *scheduler.Scheduler, runs scheduler.RunStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stack := chi.URLParam(r, "stack")
		var body struct {
			BackupRunID string `json:"backupRunId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.BackupRunID == "" {
			http.Error(w, `{"error":"body.backupRunId required"}`, http.StatusBadRequest)
			return
		}

		// Sanity: the referenced backup run must belong to this tenant
		// (defence against operator typos naming the wrong stack).
		br, err := runs.GetBackup(r.Context(), body.BackupRunID)
		if err != nil {
			http.Error(w, `{"error":"backupRunId not found"}`, http.StatusNotFound)
			return
		}
		if br.TenantStack != stack {
			http.Error(w, `{"error":"backupRunId belongs to a different tenant"}`, http.StatusBadRequest)
			return
		}

		// Kick off the restore on a background context + return 202.
		go func() {
			_, _ = sch.RunRestore(context.Background(), body.BackupRunID, "operator")
		}()

		// For the operator's immediate response: build a provisional
		// RestoreRun row. The scheduler also writes its own authoritative
		// row; the operator can poll by id.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":      "accepted",
			"backupRunId": body.BackupRunID,
			"hint":        "poll GET /api/v1/restores by latest id for progress; or watch tenant.LastSeenAt resetting on success",
		})
	})
}

// NewRestoreStatusHandler serves `GET /api/v1/restores/{id}`.
func NewRestoreStatusHandler(runs scheduler.RunStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
			return
		}
		run, err := runs.GetRestore(r.Context(), id)
		if err != nil {
			http.Error(w, `{"error":"restore not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(run)
	})
}
