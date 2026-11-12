// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	. "github.com/onsi/gomega"

	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

const testKey = "a-fake-signing-key-that-is-at-least-32-bytes-long"

func mintTestJWT(t *testing.T, tenantName string) string {
	t.Helper()
	now := time.Now().UTC()
	claims := jwt.MapClaims{
		"op":     "register",
		"tenant": tenantName,
		"iat":    now.Unix(),
		"exp":    now.Add(1 * time.Minute).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(testKey))
	Expect(err).ToNot(HaveOccurred())
	return s
}

func TestTenantsRegister_HappyPath(t *testing.T) {
	RegisterTestingT(t)

	store := tenant.NewInMemoryStore()
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/tenants/register", JWTMiddleware([]byte(testKey))(TenantsRegister(store)))

	body, _ := json.Marshal(map[string]any{
		"stackName": "my-consumer-stack",
		"version":   "v1",
		"resources": []any{},
	})
	req := httptest.NewRequest("POST", "/api/v1/tenants/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintTestJWT(t, "my-consumer-stack"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	Expect(rr.Code).To(Equal(http.StatusOK))

	got, found, err := store.Get(req.Context(), "my-consumer-stack")
	Expect(err).ToNot(HaveOccurred())
	Expect(found).To(BeTrue())
	Expect(got.Version).To(Equal("v1"))
	Expect(got.LastSeenAt).ToNot(BeZero())
}

func TestTenantsRegister_Idempotent(t *testing.T) {
	RegisterTestingT(t)

	store := tenant.NewInMemoryStore()
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/tenants/register", JWTMiddleware([]byte(testKey))(TenantsRegister(store)))

	tok := "Bearer " + mintTestJWT(t, "my-stack")
	body, _ := json.Marshal(map[string]any{"stackName": "my-stack", "version": "v1"})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/api/v1/tenants/register", bytes.NewReader(body))
		req.Header.Set("Authorization", tok)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		Expect(rr.Code).To(Equal(http.StatusOK), "attempt %d must be 200 (idempotent)", i+1)
	}

	rows, err := store.List(t.Context())
	Expect(err).ToNot(HaveOccurred())
	Expect(rows).To(HaveLen(1), "idempotent register must produce exactly one row")
}

func TestTenantsRegister_JWTMismatch(t *testing.T) {
	RegisterTestingT(t)

	store := tenant.NewInMemoryStore()
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/tenants/register", JWTMiddleware([]byte(testKey))(TenantsRegister(store)))

	// Token claims tenant "a", body says stack "b" — must be rejected.
	body, _ := json.Marshal(map[string]any{"stackName": "b", "version": "v1"})
	req := httptest.NewRequest("POST", "/api/v1/tenants/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintTestJWT(t, "a"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	Expect(rr.Code).To(Equal(http.StatusForbidden))
}

func TestTenantsRegister_Unsigned(t *testing.T) {
	RegisterTestingT(t)

	store := tenant.NewInMemoryStore()
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/tenants/register", JWTMiddleware([]byte(testKey))(TenantsRegister(store)))

	req := httptest.NewRequest("POST", "/api/v1/tenants/register",
		bytes.NewReader([]byte(`{"stackName":"x"}`)))
	req.Header.Set("Authorization", "Bearer not.a.jwt")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	Expect(rr.Code).To(Equal(http.StatusUnauthorized))
}
