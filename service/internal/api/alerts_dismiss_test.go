// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/alerts"
)

var errBoom = errors.New("boom")

func newAlertsRouter(cfg *AlertsConfig) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/api/v1/alerts/{id}/dismiss", cfg.DismissOne)
	r.Post("/api/v1/alerts/dismiss-all", cfg.DismissAll)
	return r
}

// TestDismissOne_HappyPath — valid id → 204 + persisted in the store.
func TestDismissOne_HappyPath(t *testing.T) {
	store := alerts.NewInMemoryDismissals()
	cfg := &AlertsConfig{Dismissals: store}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/42/dismiss", nil)
	newAlertsRouter(cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d; body=%q", rr.Code, rr.Body.String())
	}
	state, _ := store.State(req.Context())
	if _, ok := state.IDs[42]; !ok {
		t.Error("id not persisted")
	}
}

// TestDismissOne_BadID — the SPA shouldn't send non-numeric or
// negative ids; return 400 so a bad caller sees it.
func TestDismissOne_BadID(t *testing.T) {
	cfg := &AlertsConfig{Dismissals: alerts.NewInMemoryDismissals()}
	for _, id := range []string{"abc", "-1", "1.5", ""} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/"+id+"/dismiss", nil)
		newAlertsRouter(cfg).ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
			// Chi returns 404 for an empty path param (no route match).
			t.Errorf("id %q: want 400 or 404, got %d", id, rr.Code)
		}
	}
}

// TestDismissAll_HappyPath — 204 + the before-cutoff gets set to
// the injected `now`.
func TestDismissAll_HappyPath(t *testing.T) {
	store := alerts.NewInMemoryDismissals()
	at := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	cfg := &AlertsConfig{Dismissals: store, Now: func() time.Time { return at }}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/dismiss-all", nil)
	newAlertsRouter(cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
	state, _ := store.State(req.Context())
	if !state.Before.Equal(at.UTC()) {
		t.Errorf("Before: got %s, want %s", state.Before, at)
	}
}

// TestDismissOne_StoreError_500 — a store failure bubbles as a
// generic 500; details sink to the server log.
func TestDismissOne_StoreError_500(t *testing.T) {
	cfg := &AlertsConfig{Dismissals: errDismissals{}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/"+strconv.FormatInt(42, 10)+"/dismiss", nil)
	newAlertsRouter(cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"error"`) {
		t.Errorf("want JSON error body, got %q", rr.Body.String())
	}
}

// errDismissals returns a sentinel error from every mutating call.
type errDismissals struct{}

func (errDismissals) Dismiss(_ context.Context, _ int64) error {
	return errBoom
}

func (errDismissals) DismissAll(_ context.Context, _ time.Time) error {
	return errBoom
}

func (errDismissals) State(_ context.Context) (alerts.State, error) {
	return alerts.State{}, errBoom
}
