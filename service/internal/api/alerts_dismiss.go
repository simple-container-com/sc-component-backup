// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/alerts"
)

// AlertsConfig wires the two dismissal handlers. The read side lives
// on the Overview handler — it synthesizes alerts from recent runs
// and filters through the same Dismissals state.
type AlertsConfig struct {
	Dismissals alerts.Dismissals
	// Now is injected for deterministic tests (DismissAll's cutoff).
	// Zero value → time.Now().
	Now func() time.Time
}

// DismissOne serves `POST /api/v1/alerts/{id}/dismiss`.
//
// The SPA's useDismissAlert optimistic-updates the local list +
// expects a null-shaped 200. We 204 here — no body — which the
// SPA's `request<null>` helper accepts (checks < 400, doesn't
// decode the body).
func (c *AlertsConfig) DismissOne(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "id")
	if raw == "" {
		http.Error(w, `{"error":"alert id required"}`, http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"alert id must be a signed 64-bit integer"}`, http.StatusBadRequest)
		return
	}
	if id < 0 {
		// Alert IDs are 53-bit FNV hashes, always non-negative.
		// Negative input is a malformed caller — refuse rather
		// than silently store a bogus id we would never match.
		http.Error(w, `{"error":"alert id must be non-negative"}`, http.StatusBadRequest)
		return
	}
	if err := c.Dismissals.Dismiss(r.Context(), id); err != nil {
		http.Error(w, `{"error":"dismiss failed"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DismissAll serves `POST /api/v1/alerts/dismiss-all`. Advances the
// before-cutoff to `now` so every alert whose run finished at or
// before now gets filtered.
//
// Advance is monotonic — the store refuses to move backward, which
// keeps the dismissal state a true high-water mark. Operator who
// clicks "dismiss all" twice in the same second sees one cutoff
// bump.
func (c *AlertsConfig) DismissAll(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	if err := c.Dismissals.DismissAll(r.Context(), now); err != nil {
		http.Error(w, `{"error":"dismiss-all failed"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
