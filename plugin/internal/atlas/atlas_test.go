// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package atlas

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// digestChallengeHandler emits a one-shot Atlas-shaped Digest flow: first
// request → 401 + WWW-Authenticate, second request (with a Digest Authorization
// header) → delegate to the inner handler. No actual auth verification — the
// test only exercises the challenge/response round-trip path.
func digestChallengeHandler(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Digest realm="MMS Public API", nonce="abc123", qop=auth, opaque="ff"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func TestCreateDatabaseUser_Generates_Password(t *testing.T) {
	var received DatabaseUser
	srv := httptest.NewServer(digestChallengeHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// Atlas's own response strips the password; mirror that.
		resp := received
		resp.Password = ""
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})))
	defer srv.Close()

	c := NewClient("pub", "priv")
	c.BaseURL = srv.URL

	in := DatabaseUser{
		Username: "backup-smoke-tenant",
		Roles:    []Role{{DatabaseName: "smoke-db", RoleName: "readWrite"}},
	}
	out, err := c.CreateDatabaseUser(context.Background(), "proj1", in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.Username != "backup-smoke-tenant" {
		t.Fatalf("username: got %q", out.Username)
	}
	if len(out.Password) != 24 {
		t.Fatalf("expected 24-char generated password, got %d chars", len(out.Password))
	}
	if received.Password != out.Password {
		t.Fatalf("server saw password %q, client returned %q — mismatch", received.Password, out.Password)
	}
	if received.DatabaseName != "admin" {
		t.Fatalf("expected default DatabaseName=admin on wire, got %q", received.DatabaseName)
	}
	if received.GroupID != "proj1" {
		t.Fatalf("expected GroupID=proj1 on wire, got %q", received.GroupID)
	}
}

func TestCreateDatabaseUser_PreservesCallerPassword(t *testing.T) {
	var received DatabaseUser
	srv := httptest.NewServer(digestChallengeHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		resp := received
		resp.Password = ""
		_ = json.NewEncoder(w).Encode(resp)
	})))
	defer srv.Close()
	c := NewClient("pub", "priv")
	c.BaseURL = srv.URL
	out, err := c.CreateDatabaseUser(context.Background(), "proj1",
		DatabaseUser{Username: "x", Password: "operator-set"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Password != "operator-set" {
		t.Fatalf("explicit password overwritten: %q", out.Password)
	}
}

func TestCreateDatabaseUser_Non2xx(t *testing.T) {
	srv := httptest.NewServer(digestChallengeHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"USER_ALREADY_EXISTS"}`))
	})))
	defer srv.Close()
	c := NewClient("pub", "priv")
	c.BaseURL = srv.URL
	_, err := c.CreateDatabaseUser(context.Background(), "proj1", DatabaseUser{Username: "x"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("expected 409 error, got %v", err)
	}
}

func TestDeleteDatabaseUser_404_IsNotError(t *testing.T) {
	srv := httptest.NewServer(digestChallengeHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "admin/missing") {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	})))
	defer srv.Close()
	c := NewClient("pub", "priv")
	c.BaseURL = srv.URL
	if err := c.DeleteDatabaseUser(context.Background(), "proj1", "missing", ""); err != nil {
		t.Fatalf("404 should be nil: %v", err)
	}
}

func TestDeleteDatabaseUser_NonNotFoundIsError(t *testing.T) {
	srv := httptest.NewServer(digestChallengeHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"nope"}`))
	})))
	defer srv.Close()
	c := NewClient("pub", "priv")
	c.BaseURL = srv.URL
	err := c.DeleteDatabaseUser(context.Background(), "proj1", "x", "")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 error, got %v", err)
	}
}

func TestParseDigestChallenge(t *testing.T) {
	in := `Digest realm="MMS Public API", nonce="abcdef1234", qop=auth, opaque="opaque-val"`
	m := parseDigestChallenge(in)
	for k, want := range map[string]string{
		"realm":  "MMS Public API",
		"nonce":  "abcdef1234",
		"qop":    "auth",
		"opaque": "opaque-val",
	} {
		if got := m[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestBuildDigestAuthz_Shape(t *testing.T) {
	a, err := buildDigestAuthz("pub", "priv", "POST", "/api/atlas/v1.0/groups/x/databaseUsers",
		`Digest realm="MMS Public API", nonce="n", qop=auth, opaque="o"`, []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`username="pub"`, `realm="MMS Public API"`, `nonce="n"`,
		`uri="/api/atlas/v1.0/groups/x/databaseUsers"`, `qop=auth`,
		`algorithm=MD5`, `nc=00000001`, `cnonce=`, `response=`, `opaque="o"`,
	} {
		if !strings.Contains(a, want) {
			t.Errorf("Authorization missing %q — got: %s", want, a)
		}
	}
}

func TestGeneratePassword_LengthAndCharset(t *testing.T) {
	for _, n := range []int{8, 24, 64} {
		pw, err := generatePassword(n)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != n {
			t.Fatalf("len %d", len(pw))
		}
		for _, c := range pw {
			if !strings.ContainsRune(
				"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_", c,
			) {
				t.Fatalf("unexpected rune in password: %q", c)
			}
		}
	}
	pw, err := generatePassword(0)
	if err != nil {
		t.Fatal(err)
	}
	if pw != "" {
		t.Fatalf("expected empty, got %q", pw)
	}
}
