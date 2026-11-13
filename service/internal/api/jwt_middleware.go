// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// claimsKey is the context key the auth middleware uses to propagate the
// JWT claims to downstream handlers. Private to this package so no caller
// can fabricate claims.
type claimsKey struct{}

// Claims is the shape the plugin's mintJWT (in plugin/internal/compute.go)
// produces. Verified against the HS256 key injected as SERVICE_JWT_SECRET.
type Claims struct {
	Op     string `json:"op"`
	Tenant string `json:"tenant"`
	jwt.RegisteredClaims
}

// SessionCookieName is the HttpOnly cookie the SPA's login flow sets +
// subsequent /api/v1/* calls read. Scoped to sc-component-backup —
// this component is a REUSABLE SC Components template, not forge-fleet
// specific, so the cookie name must not leak fleet branding. A
// deployment that sits alongside another sc-component-* service on the
// same origin is still the common case; keep this name distinct from
// theirs.
const SessionCookieName = "sc_backup_session"

// JWTMiddleware returns a middleware that verifies the HS256 JWT in
// either the `Authorization: Bearer ...` header OR the `sc_backup_session`
// cookie. Cookie form is what the SPA uses after login
// (session_handlers.go); Bearer stays for scheduler-tick / machine
// callers minted by the component plugin.
//
// Rejections (401):
//   - Both header AND cookie missing
//   - Unsigned or wrong-alg token
//   - Signature doesn't verify
//   - Expired (exp < now) or not-yet-valid (nbf > now)
//
// On success, Claims are placed in the request context under claimsKey.
func JWTMiddleware(signingKey []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := tokenFromRequest(r)
			if raw == "" {
				http.Error(w, "missing or malformed credentials", http.StatusUnauthorized)
				return
			}

			claims := &Claims{}
			tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, jwt.ErrSignatureInvalid
				}
				return signingKey, nil
			}, jwt.WithValidMethods([]string{"HS256"}))
			if err != nil || !tok.Valid {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			r = r.WithContext(context.WithValue(r.Context(), claimsKey{}, claims))
			next.ServeHTTP(w, r)
		})
	}
}

// tokenFromRequest reads an HS256 JWT from either the Authorization
// header or the sc_backup_session cookie. Bearer wins when both are present
// so a caller with both can still override via header (useful for curl
// testing against an SPA-authed browser). Exported to internal callers
// only — the session handler uses it to answer "authenticated?".
func tokenFromRequest(r *http.Request) string {
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		return strings.TrimPrefix(authz, "Bearer ")
	}
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// ClaimsFromContext pulls the verified claims out of the request context.
// Returns nil if the middleware didn't run (shouldn't happen on gated
// routes, but guard defensively).
func ClaimsFromContext(ctx context.Context) *Claims {
	if v, ok := ctx.Value(claimsKey{}).(*Claims); ok {
		return v
	}
	return nil
}
