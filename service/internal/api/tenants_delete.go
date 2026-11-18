// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// TenantsDeleteConfig bundles the dependencies for the hard-delete
// handler. Each is required; nil Archive skips the S3 scrub (useful
// when the operator wants to drop the tenant but keep the archives).
type TenantsDeleteConfig struct {
	Tenants tenant.Store
	Runs    scheduler.RunStore
	Archive storage.Store
}

// TenantsDeleteResponse is the JSON body returned on success. Three
// counters so the operator can eyeball what actually got removed; the
// SPA can render this in a toast.
type TenantsDeleteResponse struct {
	Tenant            string `json:"tenant"`
	RunsDeleted       int    `json:"runs_deleted"`
	ArchivesDeleted   int    `json:"archives_deleted"`
	ArchivesAttempted int    `json:"archives_attempted"`
}

// TenantsDelete returns the handler for `DELETE /api/v1/tenants/{name}`.
//
// The full orphan-cleanup path for a consumer stack that was destroyed
// without a "consumer destroyed" event reaching the backup plugin
// (plugin/internal/pre_destroy.go:22). Order chosen so a crash
// midway through leaves something you can retry from, never a
// dangling tenant row pointing at missing archives:
//
//  1. Enumerate backup_runs for this tenant → collect S3 keys.
//  2. Delete each archive object from storage (best-effort; a failure
//     logs but does NOT abort — retention-sweep will catch orphans).
//  3. Delete backup_runs rows (bulk).
//  4. Hard-delete the tenant row.
//
// A missing tenant is 404 (before any deletes fire — the operator's
// request is unambiguous, so idempotent "already gone" would hide
// typos). Everything else is 200 with the counters.
func TenantsDelete(cfg TenantsDeleteConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		if name == "" {
			http.Error(w, `{"error":"tenant name is required"}`, http.StatusBadRequest)
			return
		}
		if cfg.Tenants == nil || cfg.Runs == nil {
			http.Error(w, `{"error":"tenant delete not wired"}`, http.StatusNotImplemented)
			return
		}

		// 404 up front — catch typos before any destructive call fires.
		_, ok, err := cfg.Tenants.Get(r.Context(), name)
		if err != nil {
			http.Error(w, `{"error":"tenant lookup failed"}`, http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, `{"error":"tenant not found"}`, http.StatusNotFound)
			return
		}

		rows, err := cfg.Runs.ListBackupsByTenant(r.Context(), name)
		if err != nil {
			http.Error(w, `{"error":"list runs failed"}`, http.StatusInternalServerError)
			return
		}
		archivesDeleted := 0
		for _, run := range rows {
			if run.S3Path == "" || cfg.Archive == nil {
				continue
			}
			if err := cfg.Archive.Delete(r.Context(), run.S3Path); err == nil {
				archivesDeleted++
			}
		}

		runsDeleted, err := cfg.Runs.DeleteBackupsByTenant(r.Context(), name)
		if err != nil {
			http.Error(w, `{"error":"delete runs failed"}`, http.StatusInternalServerError)
			return
		}
		if err := cfg.Tenants.HardDelete(r.Context(), name); err != nil {
			http.Error(w, `{"error":"delete tenant failed"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TenantsDeleteResponse{
			Tenant:            name,
			RunsDeleted:       runsDeleted,
			ArchivesDeleted:   archivesDeleted,
			ArchivesAttempted: countS3Paths(rows),
		})
	})
}

// countS3Paths counts runs that had an archive path, so the operator
// sees the attempt vs success split even when storage deletion fails.
func countS3Paths(rows []backup.BackupRun) int {
	n := 0
	for _, r := range rows {
		if r.S3Path != "" {
			n++
		}
	}
	return n
}
