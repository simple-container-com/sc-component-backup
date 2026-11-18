// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTelegram serves a minimum of Bot-API shapes the client uses.
// Each endpoint can be flipped to error-shape via the fields on this
// struct; no shared-state surprises.
type fakeTelegram struct {
	server      *httptest.Server
	getMeBad    bool
	updatesBody string
	sendErr     bool
	sentChatID  string
	sentText    string
	updatesOK   bool
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{updatesOK: true}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			if f.getMeBad {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"scbot","first_name":"SC Backup"}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			if !f.updatesOK {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			body := f.updatesBody
			if body == "" {
				body = `{"ok":true,"result":[]}`
			}
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			if f.sendErr {
				_, _ = w.Write([]byte(`{"ok":false,"description":"Forbidden: bot can't initiate conversation"}`))
				return
			}
			body, _ := io.ReadAll(r.Body)
			parsed, _ := parseForm(string(body))
			f.sentChatID = parsed["chat_id"]
			f.sentText = parsed["text"]
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func parseForm(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, "&") {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		k := part[:eq]
		v, err := formUnescape(part[eq+1:])
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// formUnescape trims the dependency — go's net/url does this but
// pulling the package in for one call wasn't paying for itself.
func formUnescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '+':
			b.WriteByte(' ')
		case '%':
			if i+2 >= len(s) {
				return "", errors.New("short %-escape")
			}
			var hi, lo byte
			if h, err := hexNibble(s[i+1]); err == nil {
				hi = h
			} else {
				return "", err
			}
			if l, err := hexNibble(s[i+2]); err == nil {
				lo = l
			} else {
				return "", err
			}
			b.WriteByte(hi<<4 | lo)
			i += 2
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, errors.New("bad hex")
}

// TestCheck_HappyPath — valid token + two chats returns the bot
// identity + both chats (deduplicated).
func TestCheck_HappyPath(t *testing.T) {
	f := newFakeTelegram(t)
	f.updatesBody = `{"ok":true,"result":[
		{"message":{"chat":{"id":111,"type":"private","first_name":"Alice","username":"alice"}}},
		{"message":{"chat":{"id":111,"type":"private","first_name":"Alice","username":"alice"}}},
		{"message":{"chat":{"id":-100222,"type":"supergroup","title":"Ops","username":"ops_grp"}}}
	]}`
	c := newTelegramClientForTest(f.server.URL, nil)
	result, err := c.Check(context.Background(), "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Bot.Username != "scbot" || result.Bot.Name != "SC Backup" {
		t.Errorf("bot mismatch: %+v", result.Bot)
	}
	if len(result.Chats) != 2 {
		t.Fatalf("want 2 deduped chats, got %d: %+v", len(result.Chats), result.Chats)
	}
	if result.Chats[0].ID != "111" || result.Chats[1].ID != "-100222" {
		t.Errorf("chat ids wrong: %+v", result.Chats)
	}
}

// TestCheck_BadToken — Telegram returns 401, client surfaces
// ErrBadToken so the SPA shows the right message.
func TestCheck_BadToken(t *testing.T) {
	f := newFakeTelegram(t)
	f.getMeBad = true
	c := newTelegramClientForTest(f.server.URL, nil)
	_, err := c.Check(context.Background(), "wrong")
	if !errors.Is(err, ErrBadToken) {
		t.Errorf("want ErrBadToken, got %v", err)
	}
}

// TestCheck_EmptyToken — handler-level validation: empty token is
// refused before any HTTP call.
func TestCheck_EmptyToken(t *testing.T) {
	c := NewTelegramClient()
	_, err := c.Check(context.Background(), "")
	if !errors.Is(err, ErrBadToken) {
		t.Errorf("want ErrBadToken, got %v", err)
	}
}

// TestSendMessage_HappyPath — body carries chat_id + text, server
// returns ok:true.
func TestSendMessage_HappyPath(t *testing.T) {
	f := newFakeTelegram(t)
	c := newTelegramClientForTest(f.server.URL, nil)
	if err := c.SendMessage(context.Background(), "tok", "42", "hello"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if f.sentChatID != "42" || f.sentText != "hello" {
		t.Errorf("sent payload wrong: chat=%q text=%q", f.sentChatID, f.sentText)
	}
}

// TestSendMessage_ServerFalse — Telegram returns ok:false with a
// description; client surfaces it as an error naming the description.
func TestSendMessage_ServerFalse(t *testing.T) {
	f := newFakeTelegram(t)
	f.sendErr = true
	c := newTelegramClientForTest(f.server.URL, nil)
	err := c.SendMessage(context.Background(), "tok", "42", "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("error does not carry Telegram description: %v", err)
	}
}
