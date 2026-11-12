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

// JWTMiddleware returns a middleware that verifies the HS256 JWT in the
// Authorization: Bearer header against the signing key.
//
// Rejections (401):
//   - Missing or malformed Authorization header
//   - Unsigned or wrong-alg token
//   - Signature doesn't verify
//   - Expired (exp < now) or not-yet-valid (nbf > now)
//
// On success, Claims are placed in the request context under claimsKey.
func JWTMiddleware(signingKey []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") {
				http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
				return
			}
			raw := strings.TrimPrefix(authz, "Bearer ")

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

// ClaimsFromContext pulls the verified claims out of the request context.
// Returns nil if the middleware didn't run (shouldn't happen on gated
// routes, but guard defensively).
func ClaimsFromContext(ctx context.Context) *Claims {
	if v, ok := ctx.Value(claimsKey{}).(*Claims); ok {
		return v
	}
	return nil
}
