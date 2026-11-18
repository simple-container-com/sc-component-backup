// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package notify holds the backup service's outbound notification
// channels. Today: Telegram. Future: webhook, email — each as its own
// file under this package with a common SettingsStore.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Default Telegram Bot API root. Override via NewTelegramClient's
// `apiBase` arg in tests.
const defaultTelegramAPIBase = "https://api.telegram.org"

// TelegramClient is the thin HTTP client the service uses to talk to
// the Telegram Bot API. One shared http.Client per service process is
// plenty — Telegram's rate limits are per-bot, not per-connection.
type TelegramClient struct {
	http    *http.Client
	apiBase string
}

// NewTelegramClient returns a client with the production API base.
// Tests pass a httptest.Server URL via newTelegramClientForTest below.
func NewTelegramClient() *TelegramClient {
	return newTelegramClientForTest(defaultTelegramAPIBase, nil)
}

// newTelegramClientForTest is the production constructor's injection
// point. Unexported — production always goes through NewTelegramClient.
func newTelegramClientForTest(apiBase string, httpClient *http.Client) *TelegramClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &TelegramClient{http: httpClient, apiBase: apiBase}
}

// Bot is the minimal bot identity returned by Telegram's getMe. Used
// by the SPA's telegram/check flow to show the operator what they
// just authenticated as.
type Bot struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

// Chat mirrors TelegramChat in service/internal/ui/src/api/types.ts.
// `name` is the group/channel title; `username` is only set when the
// chat has a public @handle (groups often don't).
type Chat struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "private" | "group" | "supergroup" | "channel"
	Name     string `json:"name"`
	Username string `json:"username"`
}

// CheckResult bundles bot identity + recently-seen chats. The SPA
// renders the chats as a picker so the operator picks a chat_id
// without typing it.
type CheckResult struct {
	Bot   Bot    `json:"bot"`
	Chats []Chat `json:"chats"`
}

// Check validates a bot token and enumerates chats the bot has
// recently seen activity in (via getUpdates). Token validation:
// getMe returns the bot's own identity on success, 401 on bad token,
// 404 on malformed token.
//
// Returns a typed error distinguishing "bad token" from "transport
// error" so the SPA can show the right message.
func (c *TelegramClient) Check(ctx context.Context, token string) (*CheckResult, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrBadToken
	}
	me, err := c.getMe(ctx, token)
	if err != nil {
		return nil, err
	}
	chats, err := c.recentChats(ctx, token)
	if err != nil {
		// Chat enumeration is best-effort — a valid token with
		// empty updates buffer still returns a successful Check.
		// Non-retryable transport errors bubble; the SPA can show
		// the bot id but leaves the chat picker empty.
		chats = nil
	}
	return &CheckResult{Bot: *me, Chats: chats}, nil
}

func (c *TelegramClient) getMe(ctx context.Context, token string) (*Bot, error) {
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"result"`
	}
	if err := c.call(ctx, token, "getMe", nil, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, ErrBadToken
	}
	return &Bot{Username: resp.Result.Username, Name: resp.Result.FirstName}, nil
}

func (c *TelegramClient) recentChats(ctx context.Context, token string) ([]Chat, error) {
	var resp struct {
		OK     bool `json:"ok"`
		Result []struct {
			Message struct {
				Chat struct {
					ID        int64  `json:"id"`
					Type      string `json:"type"`
					Title     string `json:"title"`
					Username  string `json:"username"`
					FirstName string `json:"first_name"`
				} `json:"chat"`
			} `json:"message"`
			ChannelPost struct {
				Chat struct {
					ID       int64  `json:"id"`
					Type     string `json:"type"`
					Title    string `json:"title"`
					Username string `json:"username"`
				} `json:"chat"`
			} `json:"channel_post"`
		} `json:"result"`
	}
	if err := c.call(ctx, token, "getUpdates", nil, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, ErrTelegramAPI
	}
	// Dedup by chat id. getUpdates can return many messages from the
	// same chat; the SPA wants each chat once.
	seen := map[int64]struct{}{}
	var out []Chat
	for _, u := range resp.Result {
		chats := []struct {
			ID        int64
			Type      string
			Title     string
			Username  string
			FirstName string
		}{
			{u.Message.Chat.ID, u.Message.Chat.Type, u.Message.Chat.Title, u.Message.Chat.Username, u.Message.Chat.FirstName},
			{u.ChannelPost.Chat.ID, u.ChannelPost.Chat.Type, u.ChannelPost.Chat.Title, u.ChannelPost.Chat.Username, ""},
		}
		for _, ch := range chats {
			if ch.ID == 0 {
				continue
			}
			if _, ok := seen[ch.ID]; ok {
				continue
			}
			seen[ch.ID] = struct{}{}
			name := ch.Title
			if name == "" {
				name = ch.FirstName
			}
			out = append(out, Chat{
				ID:       fmt.Sprintf("%d", ch.ID),
				Type:     ch.Type,
				Name:     name,
				Username: ch.Username,
			})
		}
	}
	return out, nil
}

// SendMessage posts a text message to the given chat.
func (c *TelegramClient) SendMessage(ctx context.Context, token, chatID, text string) error {
	if token == "" || chatID == "" {
		return ErrBadToken
	}
	body := url.Values{"chat_id": {chatID}, "text": {text}}
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := c.call(ctx, token, "sendMessage", body, &resp); err != nil {
		return err
	}
	if !resp.OK {
		if resp.Description != "" {
			return fmt.Errorf("telegram sendMessage: %s", resp.Description)
		}
		return ErrTelegramAPI
	}
	return nil
}

// call is the shared POST wrapper. form-encoded body when non-nil,
// plain GET when nil. Deserializes into `out` on 2xx.
func (c *TelegramClient) call(ctx context.Context, token, method string, body url.Values, out any) error {
	u := c.apiBase + "/bot" + token + "/" + method
	var req *http.Request
	var err error
	if body == nil {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
		// Telegram uses 401 for bad tokens and 404 for malformed
		// ones. Either way, the SPA shows the same "bad token"
		// state.
		return ErrBadToken
	}
	if resp.StatusCode >= 400 {
		return ErrTelegramAPI
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// ErrBadToken is returned when Telegram rejects the token (401/404)
// or when a required parameter was empty. The SPA shows a
// "token invalid" message on this specific error.
var ErrBadToken = &telegramError{msg: "telegram: bad token"}

// ErrTelegramAPI is the catch-all for any other Telegram response
// (5xx, OK=false without a known description, …).
var ErrTelegramAPI = &telegramError{msg: "telegram: api error"}

type telegramError struct{ msg string }

func (e *telegramError) Error() string { return e.msg }
