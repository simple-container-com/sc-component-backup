// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"net/http"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// NewBackupsListHandler serves `GET /api/v1/backups?tenant=<stack>`.
func NewBackupsListHandler(runs scheduler.RunStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := r.URL.Query().Get("tenant")
		if tenant == "" {
			http.Error(w, `{"error":"tenant query param required"}`, http.StatusBadRequest)
			return
		}
		rows, err := runs.ListBackupsByTenant(r.Context(), tenant)
		if err != nil {
			http.Error(w, `{"error":"list backups failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"backups": rows})
	})
}
