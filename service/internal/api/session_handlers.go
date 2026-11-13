// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Slice 1e.2 of sc-components-platform (2026-11-13). The SPA
// (service/internal/ui) shipped the login flow in cc22f7d — its
// api/client.ts hits GET /api/v1/session + POST /api/v1/login + POST
// /api/v1/logout. These three handlers close that loop against the
// existing HS256 JWT infrastructure.
//
// Auth model: ONE operator password (env `BACKUP_OPERATOR_PASSWORD`),
// shared across all operators of this backup component. Matches the
// Python reference (local-db-backup) — the backup SPA is operator-only,
// no per-user accounts, no RBAC. A successful login mints an HS256 JWT
// scoped to `op=operator` and drops it as an HttpOnly SameSite=Lax
// cookie (`sc_backup_session` — see JWTMiddleware for the rename
// rationale).
//
// CSRF: the SPA refreshes a CSRF token after each session rotation
// (see service/internal/ui/src/api/client.ts refreshCsrf). We don't
// enforce CSRF today (the HttpOnly + SameSite=Lax cookie covers the
// common CSRF vectors for a same-origin SPA) — the Session.csrf field
// stays nil, and the SPA's flow degrades gracefully. Filed as a
// slice-1e.3 follow-up when operators start caring.

// SessionResponse mirrors the SPA's Session type
// (service/internal/ui/src/api/types.ts:5).
type SessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	CSRF          string `json:"csrf,omitempty"` // present iff we grow CSRF
	PasswordSet   bool   `json:"password_set"`
}

// SessionConfig holds the tiny bit of shared state session/login/logout
// need: the HS256 signing key, the operator password (from env), and
// the cookie TTL. Separate from JWTMiddleware's config because the
// middleware is stateless — it verifies, it doesn't mint.
type SessionConfig struct {
	SigningKey []byte
	// OperatorPassword is the single shared-secret the SPA's login
	// form validates against. Zero-length = no password set; the SPA
	// surfaces this as `password_set: false` so operators know to
	// deploy with the env var set.
	OperatorPassword string
	// CookieTTL defaults to 12h when zero — long enough to keep an
	// operator's browser tab signed in across a typical work day while
	// still forcing a re-auth before an unattended cookie survives
	// overnight.
	CookieTTL time.Duration
	// CookieSecure forces Secure cookies off for local dev (set to
	// false when developing against http://localhost:8080). Defaults
	// to true in production deploys (behind HTTPS).
	CookieSecure bool
}

// NewSessionConfigFromEnv builds a SessionConfig from env, used from
// cmd/backup/main.go. BACKUP_OPERATOR_PASSWORD is the operator password;
// BACKUP_COOKIE_SECURE=false disables Secure flag (local dev).
func NewSessionConfigFromEnv(signingKey []byte) SessionConfig {
	cfg := SessionConfig{
		SigningKey:       signingKey,
		OperatorPassword: os.Getenv("BACKUP_OPERATOR_PASSWORD"),
		CookieTTL:        12 * time.Hour,
		CookieSecure:     true,
	}
	if v := strings.ToLower(os.Getenv("BACKUP_COOKIE_SECURE")); v == "false" || v == "0" {
		cfg.CookieSecure = false
	}
	return cfg
}

// Session is the GET /api/v1/session handler — returns whether the
// incoming request carries a valid JWT (cookie OR Bearer). The SPA
// calls this on load to decide whether to show the LoginPage or the
// authenticated shell.
//
// This handler MUST be mounted outside the JWTMiddleware group: an
// unauthenticated request should get a 200 with authenticated=false,
// not a 401.
func (c *SessionConfig) Session(w http.ResponseWriter, r *http.Request) {
	resp := SessionResponse{PasswordSet: c.OperatorPassword != ""}
	raw := tokenFromRequest(r)
	if raw != "" && c.verifyToken(raw) {
		resp.Authenticated = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// Login is the POST /api/v1/login handler. Body: `{"password": "<op pwd>"}`.
// On success, sets the HttpOnly cookie + returns 204; on failure, 401.
//
// The constant-time comparison matters: without it a timing side-channel
// leaks the shared-secret byte-by-byte. subtle.ConstantTimeCompare is
// what Go's crypto/http2 + net/smtp use for the same shape.
func (c *SessionConfig) Login(w http.ResponseWriter, r *http.Request) {
	if c.OperatorPassword == "" {
		http.Error(w, `{"error":"BACKUP_OPERATOR_PASSWORD not set on the server"}`, http.StatusForbidden)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		http.Error(w, `{"error":"missing password"}`, http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(c.OperatorPassword)) != 1 {
		http.Error(w, `{"error":"wrong password"}`, http.StatusUnauthorized)
		return
	}
	tok, err := c.mintOperatorJWT()
	if err != nil {
		http.Error(w, `{"error":"mint failed"}`, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   c.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(c.cookieTTL()),
	})
	w.WriteHeader(http.StatusNoContent)
}

// Logout clears the session cookie by setting it empty + expired.
// Always returns 204 (idempotent — a double-logout isn't an error).
func (c *SessionConfig) Logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   c.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (c *SessionConfig) mintOperatorJWT() (string, error) {
	now := time.Now()
	claims := &Claims{
		Op: "operator",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "sc-component-backup",
			Subject:   "operator",
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(c.cookieTTL())),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(c.SigningKey)
}

func (c *SessionConfig) verifyToken(raw string) bool {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return c.SigningKey, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	return err == nil && tok.Valid
}

func (c *SessionConfig) cookieTTL() time.Duration {
	if c.CookieTTL == 0 {
		return 12 * time.Hour
	}
	return c.CookieTTL
}

// writeJSON is a shared JSON-response helper. Separate here (not in a
// util file) because every new handler in this slice reaches for it and
// we don't want cross-file test dependencies.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
