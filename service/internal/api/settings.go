// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/notify"
)

// SettingsConfig wires the five SPA settings endpoints.
type SettingsConfig struct {
	Store           notify.Store
	Telegram        *notify.TelegramClient
	StorageLocation string // mirrored into AppSettings.storage for the SPA
	MaxParallel     int    // operator-tunable; 1 is a sensible v1 default
}

type appSettingsStorageJSON struct {
	Kind     string `json:"kind"`
	Location string `json:"location,omitempty"`
}

type appSettingsTelegramJSON struct {
	ChatID   string `json:"chat_id"`
	TokenSet bool   `json:"token_set"`
}

type appSettingsNotifyJSON struct {
	Telegram   *appSettingsTelegramJSON `json:"telegram"`
	WebhookSet bool                     `json:"webhook_set"`
}

type appSettingsJSON struct {
	Storage     appSettingsStorageJSON `json:"storage"`
	Notify      appSettingsNotifyJSON  `json:"notify"`
	MaxParallel int                    `json:"max_parallel"`
}

// Settings serves `GET /api/v1/settings`. Returns the AppSettings
// shape in service/internal/ui/src/api/types.ts — storage block (from
// the archive store's configured location), notify.telegram
// (chat_id + whether a token is set, never the token itself), and
// the max_parallel knob.
func (c *SettingsConfig) Settings(w http.ResponseWriter, r *http.Request) {
	resp := appSettingsJSON{
		Storage:     appSettingsStorageJSON{Kind: "s3", Location: c.StorageLocation},
		Notify:      appSettingsNotifyJSON{Telegram: nil, WebhookSet: false},
		MaxParallel: c.MaxParallel,
	}
	if resp.MaxParallel <= 0 {
		resp.MaxParallel = 1
	}
	if c.Store != nil {
		if t, ok, err := c.Store.GetTelegram(r.Context()); err == nil && ok {
			resp.Notify.Telegram = &appSettingsTelegramJSON{
				ChatID:   t.ChatID,
				TokenSet: t.Token != "",
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// telegramCheckRequest is the body of `POST /api/v1/settings/telegram/check`.
// Token is optional — when empty, the handler uses the stored token
// (lets the SPA re-check without re-entering the token).
type telegramCheckRequest struct {
	Token string `json:"token"`
}

// TelegramCheck serves `POST /api/v1/settings/telegram/check`. Calls
// Telegram's getMe + getUpdates and returns the TelegramCheck shape
// the SPA renders.
func (c *SettingsConfig) TelegramCheck(w http.ResponseWriter, r *http.Request) {
	var req telegramCheckRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
	}
	token := req.Token
	if token == "" && c.Store != nil {
		if t, ok, err := c.Store.GetTelegram(r.Context()); err == nil && ok {
			token = t.Token
		}
	}
	if token == "" {
		http.Error(w, `{"error":"token required"}`, http.StatusBadRequest)
		return
	}
	if c.Telegram == nil {
		http.Error(w, `{"error":"telegram client not configured"}`, http.StatusInternalServerError)
		return
	}
	result, err := c.Telegram.Check(r.Context(), token)
	if err != nil {
		if errors.Is(err, notify.ErrBadToken) {
			http.Error(w, `{"error":"telegram rejected the token"}`, http.StatusBadRequest)
			return
		}
		http.Error(w, `{"error":"telegram check failed"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// telegramSaveRequest is the body of `PUT /api/v1/settings/telegram`.
// Token is optional — when empty, the handler keeps the currently
// stored token (lets the operator update chat_id alone without
// re-entering the token).
type telegramSaveRequest struct {
	Token  string `json:"token"`
	ChatID string `json:"chat_id"`
}

// TelegramSave serves `PUT /api/v1/settings/telegram`. Persists the
// provided token + chat_id. Empty token = keep existing.
func (c *SettingsConfig) TelegramSave(w http.ResponseWriter, r *http.Request) {
	var req telegramSaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if req.ChatID == "" {
		http.Error(w, `{"error":"chat_id required"}`, http.StatusBadRequest)
		return
	}
	if c.Store == nil {
		http.Error(w, `{"error":"settings store not configured"}`, http.StatusInternalServerError)
		return
	}
	effectiveToken := req.Token
	if effectiveToken == "" {
		existing, ok, err := c.Store.GetTelegram(r.Context())
		if err != nil {
			http.Error(w, `{"error":"load existing settings failed"}`, http.StatusInternalServerError)
			return
		}
		if !ok || existing.Token == "" {
			http.Error(w, `{"error":"token required (no previous token to keep)"}`, http.StatusBadRequest)
			return
		}
		effectiveToken = existing.Token
	}
	if err := c.Store.PutTelegram(r.Context(), notify.TelegramSettings{
		Token:  effectiveToken,
		ChatID: req.ChatID,
	}); err != nil {
		http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// TelegramDisable serves `DELETE /api/v1/settings/telegram`.
func (c *SettingsConfig) TelegramDisable(w http.ResponseWriter, r *http.Request) {
	if c.Store == nil {
		http.Error(w, `{"error":"settings store not configured"}`, http.StatusInternalServerError)
		return
	}
	if err := c.Store.DeleteTelegram(r.Context()); err != nil {
		http.Error(w, `{"error":"disable failed"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TestNotify serves `POST /api/v1/settings/test-notify`. Sends a
// static test message via the saved Telegram config. The SPA
// shows `{ok: true}` on success → operator knows the wire is live
// end-to-end.
//
// Returns 200 + `{ok:false}` on a configured-but-failed send (rather
// than 4xx/5xx) so the SPA shows the honest result without a toast
// cascade.
func (c *SettingsConfig) TestNotify(w http.ResponseWriter, r *http.Request) {
	if c.Store == nil || c.Telegram == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": false})
		return
	}
	t, ok, err := c.Store.GetTelegram(r.Context())
	if err != nil || !ok || t.Token == "" || t.ChatID == "" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": false})
		return
	}
	err = c.Telegram.SendMessage(r.Context(), t.Token, t.ChatID,
		"✅ sc-component-backup test notification — your Telegram wire works.")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": err == nil})
}
