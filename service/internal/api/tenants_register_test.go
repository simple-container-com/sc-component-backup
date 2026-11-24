// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"bytes"
	"context"
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

// TestTenantsRegister_RejectsNonRegisterOp pins the `claims.Op !=
// "register"` gate in the handler. Live-caught 2026-11-24 by the
// mutation harness: TestTenantsRegister_JWTMismatch happens to also
// fail when the op check is removed, BUT only because the tenant↔body
// check below fires first — and mintTestJWT always mints
// `op:"register"`, so no other test ever presented a non-register op.
// A handler where the op check was quietly removed would hand a
// scheduler or operator token the ability to overwrite a tenant row.
func TestTenantsRegister_RejectsNonRegisterOp(t *testing.T) {
	RegisterTestingT(t)

	store := tenant.NewInMemoryStore()
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/tenants/register", JWTMiddleware([]byte(testKey))(TenantsRegister(store)))

	// Mint a valid, tenant-matching token with the WRONG op. If the op
	// check is removed, the tenant↔body check still passes (they
	// match), and the handler would 200. With the op check present,
	// 403.
	now := time.Now().UTC()
	claims := jwt.MapClaims{
		"op":     "operator",
		"tenant": "x",
		"iat":    now.Unix(),
		"exp":    now.Add(1 * time.Minute).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	raw, err := tok.SignedString([]byte(testKey))
	Expect(err).ToNot(HaveOccurred())

	body, _ := json.Marshal(map[string]any{"stackName": "x", "version": "v1"})
	req := httptest.NewRequest("POST", "/api/v1/tenants/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	Expect(rr.Code).To(Equal(http.StatusForbidden), "non-register op must be refused BEFORE the body check; got body: %s", rr.Body.String())
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

// TestTenantsRegister_EmptyResourcesPreservesExisting is the regression
// guard for the 2026-11-18 forge-conductor incident: an UNRELATED deploy
// re-ran ComputeProcess, discovery came up empty, and the blind Upsert
// replaced a record carrying a live scoped-user URI with one carrying
// nothing — silently making the fleet's largest DB (1.78 GB)
// unbackupable, with `backup-now` answering 400 and no alert anywhere.
func TestTenantsRegister_EmptyResourcesPreservesExisting(t *testing.T) {
	RegisterTestingT(t)

	store := tenant.NewInMemoryStore()
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/tenants/register", JWTMiddleware([]byte(testKey))(TenantsRegister(store)))

	// A good record already exists, as a working consumer would have.
	good := tenant.Record{
		StackName: "live-consumer",
		Version:   "v1",
		Resources: []tenant.Resource{{
			Type: "mongodb",
			Name: "live-consumer",
			Connection: map[string]string{
				"driver":   "mongodb",
				"uri":      "mongodb://peer-live-consumer:secret@host/live-consumer",
				"database": "live-consumer",
				"cluster":  "mongodb",
			},
		}},
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour),
	}
	Expect(store.Upsert(context.Background(), good)).To(Succeed())

	// A later deploy registers with NO resources (discovery miss).
	body, _ := json.Marshal(map[string]any{
		"stackName": "live-consumer",
		"version":   "v2",
		"resources": []any{},
	})
	req := httptest.NewRequest("POST", "/api/v1/tenants/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintTestJWT(t, "live-consumer"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	Expect(rr.Code).To(Equal(http.StatusOK))

	got, found, err := store.Get(req.Context(), "live-consumer")
	Expect(err).ToNot(HaveOccurred())
	Expect(found).To(BeTrue())

	// The resources survived — the tenant is still backupable.
	Expect(got.Resources).To(HaveLen(1), "empty-resources registration must not wipe a live record")
	Expect(got.Resources[0].Connection["database"]).To(Equal("live-consumer"))
	Expect(got.Resources[0].Connection["uri"]).To(ContainSubstring("peer-live-consumer"))
	// CreatedAt is preserved so the tenant's age isn't reset by the skew.
	Expect(got.CreatedAt).To(Equal(good.CreatedAt))
	// Everything else still updates.
	Expect(got.Version).To(Equal("v2"))
	Expect(got.LastSeenAt).To(BeTemporally(">", good.CreatedAt))
}

// TestTenantsRegister_EmptyResourcesOnFreshTenantStillRegisters — the
// preserve rule must not block a genuinely new, genuinely stateless
// consumer from registering.
func TestTenantsRegister_EmptyResourcesOnFreshTenantStillRegisters(t *testing.T) {
	RegisterTestingT(t)

	store := tenant.NewInMemoryStore()
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/tenants/register", JWTMiddleware([]byte(testKey))(TenantsRegister(store)))

	body, _ := json.Marshal(map[string]any{
		"stackName": "stateless-consumer",
		"version":   "v1",
		"resources": []any{},
	})
	req := httptest.NewRequest("POST", "/api/v1/tenants/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintTestJWT(t, "stateless-consumer"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	Expect(rr.Code).To(Equal(http.StatusOK))

	got, found, err := store.Get(req.Context(), "stateless-consumer")
	Expect(err).ToNot(HaveOccurred())
	Expect(found).To(BeTrue())
	Expect(got.Resources).To(BeEmpty())
}

// TestTenantsRegister_NonEmptyResourcesStillReplace — a registration
// that DOES carry resources replaces the old set (rotated credentials,
// added/removed databases must propagate).
func TestTenantsRegister_NonEmptyResourcesStillReplace(t *testing.T) {
	RegisterTestingT(t)

	store := tenant.NewInMemoryStore()
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/tenants/register", JWTMiddleware([]byte(testKey))(TenantsRegister(store)))

	Expect(store.Upsert(context.Background(), tenant.Record{
		StackName: "rotating-consumer",
		Version:   "v1",
		Resources: []tenant.Resource{{
			Type:       "mongodb",
			Name:       "rotating-consumer",
			Connection: map[string]string{"driver": "mongodb", "uri": "mongodb://old:old@host/db", "database": "db"},
		}},
	})).To(Succeed())

	body, _ := json.Marshal(map[string]any{
		"stackName": "rotating-consumer",
		"version":   "v2",
		"resources": []map[string]any{{
			"type": "mongodb",
			"name": "rotating-consumer",
			"connection": map[string]string{
				"driver": "mongodb", "uri": "mongodb://new:new@host/db", "database": "db",
			},
		}},
	})
	req := httptest.NewRequest("POST", "/api/v1/tenants/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintTestJWT(t, "rotating-consumer"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	Expect(rr.Code).To(Equal(http.StatusOK))

	got, _, _ := store.Get(req.Context(), "rotating-consumer")
	Expect(got.Resources).To(HaveLen(1))
	Expect(got.Resources[0].Connection["uri"]).To(ContainSubstring("new:new"), "rotated credentials must propagate")
}
