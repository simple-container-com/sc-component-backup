// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// NewBackupNowHandler serves `POST /api/v1/tenants/{stack}/backup-now`.
// Picks the first DB-flavoured resource (connection.driver present)
// unless the request body overrides with {"resource": "<name>"}.
func NewBackupNowHandler(sch *scheduler.Scheduler, tenants tenant.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stack := chi.URLParam(r, "stack")
		if stack == "" {
			http.Error(w, `{"error":"stack path param required"}`, http.StatusBadRequest)
			return
		}
		var body struct {
			Resource string `json:"resource,omitempty"`
		}
		if r.ContentLength > 0 {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}

		t, found, err := tenants.Get(r.Context(), stack)
		if err != nil || !found {
			http.Error(w, `{"error":"tenant not found"}`, http.StatusNotFound)
			return
		}
		resourceName := body.Resource
		if resourceName == "" {
			resourceName = firstDBResource(t.Resources)
			if resourceName == "" {
				http.Error(w, `{"error":"tenant has no DB-flavoured resource"}`, http.StatusBadRequest)
				return
			}
		}

		// Kick the scheduler on a background context so a deploy-host
		// client that times out doesn't orphan the in-flight dump.
		runCh := make(chan backup.BackupRun, 1)
		errCh := make(chan error, 1)
		go func() {
			bg := context.Background()
			run, err := sch.RunOnce(bg, stack, resourceName, "operator")
			if err != nil {
				errCh <- err
				return
			}
			runCh <- run
		}()
		select {
		case run := <-runCh:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(run)
		case err := <-errCh:
			// classify: "already in flight" is 409
			if strings.Contains(err.Error(), "already in flight") {
				http.Error(w, `{"error":"`+jsonEscape(err.Error())+`"}`, http.StatusConflict)
				return
			}
			http.Error(w, `{"error":"`+jsonEscape(err.Error())+`"}`, http.StatusInternalServerError)
		}
	})
}

func firstDBResource(resources []tenant.Resource) string {
	for _, r := range resources {
		if strings.TrimSpace(r.Connection["driver"]) != "" {
			return r.Name
		}
	}
	return ""
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}
