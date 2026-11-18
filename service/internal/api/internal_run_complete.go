// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// InternalRunCompleteConfig bundles the handler's deps.
type InternalRunCompleteConfig struct {
	Runs scheduler.RunStore
}

// InternalRunCompleteRequest is the body the Fargate worker POSTs on
// exit. The worker is one-shot — exactly one POST per run (dump +
// upload success OR failure). All fields are optional except `status`;
// bytes/s3_path fill in on success, error on failure.
type InternalRunCompleteRequest struct {
	Status  string `json:"status"` // ok | failed
	Bytes   int64  `json:"bytes,omitempty"`
	S3Path  string `json:"s3_path,omitempty"`
	Error   string `json:"error,omitempty"`
	Started string `json:"started,omitempty"` // RFC3339 — optional, for integrity cross-check
}

// InternalRunComplete serves `POST /api/v1/internal/runs/{id}/complete`.
// Called by the Fargate worker when it exits. Service-token authed
// via the same HS256 JWT middleware the SPA uses — the worker's
// token is minted by backup-now (or scheduler-tick) with an `op:
// "worker"` claim scoped to the run's tenant.
//
// 200 on success; 404 if the run id is unknown; 409 if the run is
// already in a terminal state (idempotent — the worker retries after
// a transient network hiccup shouldn't flip the row twice).
//
// The row's Status + FinishedAt + SizeBytes + S3Path + Error are the
// only fields this handler writes — tenant/driver/started/etc. stay
// as backup-now wrote them.
func InternalRunComplete(cfg InternalRunCompleteConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.Runs == nil {
			http.Error(w, `{"error":"run store not wired"}`, http.StatusNotImplemented)
			return
		}
		runID := chi.URLParam(r, "id")
		if runID == "" {
			http.Error(w, `{"error":"run id path param required"}`, http.StatusBadRequest)
			return
		}
		var body InternalRunCompleteRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
		switch strings.ToLower(body.Status) {
		case "ok", "failed":
			// OK
		default:
			http.Error(w, `{"error":"status must be ok|failed"}`, http.StatusBadRequest)
			return
		}

		run, err := cfg.Runs.GetBackup(r.Context(), runID)
		if err != nil {
			http.Error(w, `{"error":"run not found"}`, http.StatusNotFound)
			return
		}
		if run.Status != backup.StatusRunning && run.Status != backup.StatusPending {
			// Already terminal — idempotent 200 with the same body shape
			// (worker retried a successful POST after a flaky connection,
			// or the watchdog already reconciled it). Don't flip the row.
			writeJSON(w, http.StatusOK, map[string]string{"run_id": run.ID, "status": string(run.Status), "note": "already terminal"})
			return
		}

		run.FinishedAt = time.Now().UTC()
		if strings.EqualFold(body.Status, "ok") {
			run.Status = backup.StatusSucceeded
			run.S3Path = body.S3Path
			run.SizeBytes = body.Bytes
			run.Error = ""
		} else {
			run.Status = backup.StatusFailed
			if body.Error != "" {
				run.Error = truncateError(body.Error, 512)
			} else {
				run.Error = "worker reported failure (no error detail)"
			}
		}
		if err := cfg.Runs.UpdateBackup(r.Context(), run); err != nil {
			http.Error(w, `{"error":"update failed"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"run_id": run.ID, "status": string(run.Status)})
	})
}

// truncateError clamps the worker-reported error string so a verbose
// mongodump failure doesn't bloat the row past Mongo's 16MB doc cap.
// Mirrors scheduler.truncate but kept local to avoid an export. The
// ellipsis is "…" (3 bytes in UTF-8), so the output is at most `max`
// bytes total.
func truncateError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	if max <= len(ellipsis) {
		return s[:max]
	}
	return s[:max-len(ellipsis)] + ellipsis
}
