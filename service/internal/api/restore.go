// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// NewRestoreHandler serves `POST /api/v1/tenants/{stack}/restore`.
// Body: `{"backupRunId": "<id>"}`. Runs the restore synchronously
// (goroutine + select, matching backup-now) and returns 202 with the
// finished RestoreRun. Lambda freezes the container after a response
// returns, so fire-and-forget would strand the goroutine — live-caught
// 2026-11-13 on the 1f smoke: a 202 landed, Lambda froze, and widgets
// stayed empty because the restore never progressed past accept.
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

		br, err := runs.GetBackup(r.Context(), body.BackupRunID)
		if err != nil {
			http.Error(w, `{"error":"backupRunId not found"}`, http.StatusNotFound)
			return
		}
		if br.TenantStack != stack {
			http.Error(w, `{"error":"backupRunId belongs to a different tenant"}`, http.StatusBadRequest)
			return
		}

		runCh := make(chan backup.RestoreRun, 1)
		errCh := make(chan error, 1)
		go func() {
			bg := context.Background()
			rr, err := sch.RunRestore(bg, body.BackupRunID, "operator")
			if err != nil {
				errCh <- err
				return
			}
			runCh <- rr
		}()
		select {
		case rr := <-runCh:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(rr)
		case err := <-errCh:
			http.Error(w, `{"error":"`+jsonEscape(err.Error())+`"}`, http.StatusInternalServerError)
		}
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
