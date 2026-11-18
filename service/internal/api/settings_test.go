// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/notify"
)

// TestSettings_EmptyStore — no telegram stored → notify.telegram
// renders as JSON null in AppSettings.notify.telegram; storage block
// is populated from the configured location.
func TestSettings_EmptyStore(t *testing.T) {
	cfg := &SettingsConfig{
		Store:           notify.NewInMemoryStore(),
		StorageLocation: "s3://my-bucket",
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	cfg.Settings(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body=%q", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	notify := resp["notify"].(map[string]any)
	if notify["telegram"] != nil {
		t.Errorf("empty-store should render notify.telegram=null, got %v", notify["telegram"])
	}
	storage := resp["storage"].(map[string]any)
	if storage["location"] != "s3://my-bucket" {
		t.Errorf("storage.location: got %v", storage["location"])
	}
	// max_parallel sane default
	if resp["max_parallel"].(float64) != 1 {
		t.Errorf("max_parallel: got %v, want 1", resp["max_parallel"])
	}
}

// TestSettings_WithTelegram — token stored → token_set true, token
// itself never crosses the response boundary.
func TestSettings_WithTelegram(t *testing.T) {
	store := notify.NewInMemoryStore()
	_ = store.PutTelegram(nil, notify.TelegramSettings{Token: "tok-SECRET", ChatID: "42"}) //nolint:staticcheck
	cfg := &SettingsConfig{Store: store}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	cfg.Settings(rr, req)
	if strings.Contains(rr.Body.String(), "tok-SECRET") {
		t.Errorf("token leaked in settings response: %q", rr.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	tg := resp["notify"].(map[string]any)["telegram"].(map[string]any)
	if tg["chat_id"] != "42" || tg["token_set"] != true {
		t.Errorf("telegram slot wrong: %+v", tg)
	}
}

// TestTelegramSave_HappyPath — new token + chat_id, store round-trips.
func TestTelegramSave_HappyPath(t *testing.T) {
	store := notify.NewInMemoryStore()
	cfg := &SettingsConfig{Store: store}
	body := strings.NewReader(`{"token":"tok-NEW","chat_id":"42"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/telegram", body)
	rr := httptest.NewRecorder()
	cfg.TelegramSave(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body=%q", rr.Code, rr.Body.String())
	}
	saved, ok, _ := store.GetTelegram(req.Context())
	if !ok || saved.Token != "tok-NEW" || saved.ChatID != "42" {
		t.Errorf("not persisted correctly: %+v", saved)
	}
}

// TestTelegramSave_KeepExistingToken — empty token + chat_id, with
// an existing token → keep the stored token.
func TestTelegramSave_KeepExistingToken(t *testing.T) {
	store := notify.NewInMemoryStore()
	_ = store.PutTelegram(nil, notify.TelegramSettings{Token: "tok-OLD", ChatID: "1"}) //nolint:staticcheck
	cfg := &SettingsConfig{Store: store}
	body := strings.NewReader(`{"chat_id":"2"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/telegram", body)
	rr := httptest.NewRecorder()
	cfg.TelegramSave(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	saved, _, _ := store.GetTelegram(req.Context())
	if saved.Token != "tok-OLD" || saved.ChatID != "2" {
		t.Errorf("want token preserved + chat updated: %+v", saved)
	}
}

// TestTelegramSave_MissingToken_NoExisting_400 — empty token + no
// prior row → 400 (we can't guess a token).
func TestTelegramSave_MissingToken_NoExisting_400(t *testing.T) {
	cfg := &SettingsConfig{Store: notify.NewInMemoryStore()}
	body := strings.NewReader(`{"chat_id":"42"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/telegram", body)
	rr := httptest.NewRecorder()
	cfg.TelegramSave(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d; body=%q", rr.Code, rr.Body.String())
	}
}

// TestTelegramDisable_HappyPath — deletes the stored telegram row.
func TestTelegramDisable_HappyPath(t *testing.T) {
	store := notify.NewInMemoryStore()
	_ = store.PutTelegram(nil, notify.TelegramSettings{Token: "tok", ChatID: "42"}) //nolint:staticcheck
	cfg := &SettingsConfig{Store: store}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/settings/telegram", nil)
	rr := httptest.NewRecorder()
	cfg.TelegramDisable(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
	_, ok, _ := store.GetTelegram(req.Context())
	if ok {
		t.Errorf("row still present after disable")
	}
}

// TestTestNotify_NoConfig_FalseOK — no token stored → {ok:false}
// at HTTP 200; SPA renders "not configured".
func TestTestNotify_NoConfig_FalseOK(t *testing.T) {
	cfg := &SettingsConfig{Store: notify.NewInMemoryStore()}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/test-notify", nil)
	cfg.TestNotify(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var out map[string]bool
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["ok"] {
		t.Error("want ok:false, got ok:true")
	}
}
