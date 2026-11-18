// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
)

// fakePresignStore is a tiny Store that only satisfies Presign — the
// download handler doesn't call Put / Get / List / Delete.
type fakePresignStore struct {
	presignCalls []presignCall
	returnURL    string
	returnErr    error
}

type presignCall struct {
	key     string
	expires time.Duration
}

func (f *fakePresignStore) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, errors.New("not implemented")
}

func (f *fakePresignStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (f *fakePresignStore) List(context.Context, string) ([]storage.StoredObject, error) {
	return nil, errors.New("not implemented")
}

func (f *fakePresignStore) Delete(context.Context, string) error {
	return errors.New("not implemented")
}

func (f *fakePresignStore) Presign(_ context.Context, path string, expires time.Duration) (string, error) {
	f.presignCalls = append(f.presignCalls, presignCall{key: path, expires: expires})
	if f.returnErr != nil {
		return "", f.returnErr
	}
	return f.returnURL, nil
}

func mustGet(t *testing.T, cfg *DownloadConfig, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/download?"+rawQuery, nil)
	rr := httptest.NewRecorder()
	cfg.Download(rr, req)
	return rr
}

// TestDownload_HappyPath_RedirectsToPresignedURL — handler accepts a
// valid key, calls Presign with default 10-min TTL, 302s to the URL.
func TestDownload_HappyPath_RedirectsToPresignedURL(t *testing.T) {
	store := &fakePresignStore{returnURL: "https://s3.eu-central-1.amazonaws.com/signed?X-Amz=..."}
	cfg := &DownloadConfig{Archive: store}
	rr := mustGet(t, cfg, "key=tenant-x/2026/11/18/mongo.archive.gz")
	if rr.Code != http.StatusFound {
		t.Fatalf("want 302 Found, got %d; body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != store.returnURL {
		t.Errorf("Location: got %q, want %q", got, store.returnURL)
	}
	if n := len(store.presignCalls); n != 1 {
		t.Fatalf("expected 1 Presign call, got %d", n)
	}
	if got := store.presignCalls[0].key; got != "tenant-x/2026/11/18/mongo.archive.gz" {
		t.Errorf("Presign key: got %q, want %q", got, "tenant-x/2026/11/18/mongo.archive.gz")
	}
	if got := store.presignCalls[0].expires; got != 10*time.Minute {
		t.Errorf("Presign expires: got %s, want 10m", got)
	}
}

// TestDownload_CustomTTL — explicit non-zero TTL is passed through to
// the store. Operator-tunable if a future workflow needs a longer link.
func TestDownload_CustomTTL(t *testing.T) {
	store := &fakePresignStore{returnURL: "https://signed"}
	cfg := &DownloadConfig{Archive: store, PresignTTL: 2 * time.Hour}
	_ = mustGet(t, cfg, "key=a/b")
	if got := store.presignCalls[0].expires; got != 2*time.Hour {
		t.Errorf("custom TTL not forwarded: got %s", got)
	}
}

// TestDownload_EmptyKey_400 — a missing / blank `key` is the most
// common misuse; return a clear 400 rather than a silent 302 to a
// malformed URL.
func TestDownload_EmptyKey_400(t *testing.T) {
	for _, q := range []string{"", "key=", "key=%20%20"} {
		rr := mustGet(t, &DownloadConfig{Archive: &fakePresignStore{}}, q)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("query %q: want 400, got %d", q, rr.Code)
		}
	}
}

// TestDownload_URLKey_400 — reject keys that look like URLs. A
// malicious caller could otherwise coerce the store into Presigning
// against an arbitrary host.
func TestDownload_URLKey_400(t *testing.T) {
	for _, q := range []string{
		"key=https://example.com/pwned",
		"key=http://example.com/",
		"key=file:///etc/passwd",
	} {
		rr := mustGet(t, &DownloadConfig{Archive: &fakePresignStore{}}, q)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("query %q: want 400, got %d", q, rr.Code)
		}
	}
}

// TestDownload_Traversal_400 — path traversal is explicitly refused.
// The Store normalisers may already strip these, but a 400 at the HTTP
// boundary is the right diagnostic location.
func TestDownload_Traversal_400(t *testing.T) {
	for _, q := range []string{
		"key=tenant/../../etc/passwd",
		"key=..",
		"key=a/../b",
	} {
		rr := mustGet(t, &DownloadConfig{Archive: &fakePresignStore{}}, q)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("query %q: want 400, got %d; body=%s", q, rr.Code, rr.Body.String())
		}
	}
}

// TestDownload_StoreNotFound_404 — Store returns ErrNotFound → the
// handler surfaces it as 404 so the SPA renders the right empty state.
func TestDownload_StoreNotFound_404(t *testing.T) {
	store := &fakePresignStore{returnErr: storage.ErrNotFound}
	rr := mustGet(t, &DownloadConfig{Archive: store}, "key=missing/key")
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestDownload_StoreError_500 — any other Store error → 500 with a
// generic JSON body (details sink to the service log).
func TestDownload_StoreError_500(t *testing.T) {
	store := &fakePresignStore{returnErr: errors.New("boom")}
	rr := mustGet(t, &DownloadConfig{Archive: store}, "key=k")
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"error"`) {
		t.Errorf("expected JSON error body, got %q", rr.Body.String())
	}
}

// TestDownload_LeadingSlash_Normalised — a key with a leading `/` and
// the same key without must yield identical Presign calls.
func TestDownload_LeadingSlash_Normalised(t *testing.T) {
	store := &fakePresignStore{returnURL: "https://signed"}
	cfg := &DownloadConfig{Archive: store}
	_ = mustGet(t, cfg, "key=/tenant/archive.gz")
	_ = mustGet(t, cfg, "key=tenant/archive.gz")
	if len(store.presignCalls) != 2 {
		t.Fatalf("want 2 Presign calls, got %d", len(store.presignCalls))
	}
	if store.presignCalls[0].key != store.presignCalls[1].key {
		t.Errorf("leading slash not normalised: %q vs %q",
			store.presignCalls[0].key, store.presignCalls[1].key)
	}
}
