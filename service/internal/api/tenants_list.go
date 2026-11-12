// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// TenantsList returns the handler for GET /api/v1/tenants.
//
// Consumed by the embedded SPA. Returns all currently-registered tenants,
// including soft-deleted ones for operator visibility. JWT-authed via the
// middleware; the handler does NOT further restrict by claims (a signed
// token is enough for the backup — real components may gate by operator
// role).
func TenantsList(store tenant.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.List(r.Context())
		if err != nil {
			slog.Error("tenants list failed", "err", err)
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		if rows == nil {
			rows = []tenant.Record{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenants": rows,
			"count":   len(rows),
		})
	}
}
