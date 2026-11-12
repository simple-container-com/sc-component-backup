// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// TenantsRegister returns the handler for POST /api/v1/tenants/register.
//
// The plugin's push-register call sends:
//
//	{
//	  "stackName": "<consumer-stack>",
//	  "version":   "<consumer-version>",
//	  "resources": [...]
//	}
//
// Idempotent on (stackName, version): same tuple arriving twice is a
// no-op (identical record → 200, changed record → update + 200). See
// sc-components-platform design doc §P3 push-model operational contract.
func TenantsRegister(store tenant.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := ClaimsFromContext(r.Context())
		if claims == nil || claims.Op != "register" {
			http.Error(w, "forbidden: token op must be 'register'", http.StatusForbidden)
			return
		}

		var record tenant.Record
		if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
			http.Error(w, "malformed JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if record.StackName == "" {
			http.Error(w, "stackName is required", http.StatusBadRequest)
			return
		}
		// The JWT's tenant claim MUST match the body's stackName —
		// prevents a consumer from registering against another consumer's
		// name using its own JWT.
		if claims.Tenant != record.StackName {
			http.Error(w, "forbidden: JWT tenant does not match body stackName", http.StatusForbidden)
			return
		}

		record.LastSeenAt = time.Now().UTC()
		if record.CreatedAt.IsZero() {
			record.CreatedAt = record.LastSeenAt
		}

		if err := store.Upsert(r.Context(), record); err != nil {
			slog.Error("tenants upsert failed", "stackName", record.StackName, "err", err)
			http.Error(w, "upsert failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "ok",
			"stackName": record.StackName,
			"version":   record.Version,
		})
	}
}
