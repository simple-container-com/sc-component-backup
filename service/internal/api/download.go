// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
)

// DownloadConfig carries the dependencies the archive-download handler
// needs: the Store that owns the archive bucket (shared with the scheduler
// + restore paths).
type DownloadConfig struct {
	// Archive is the storage backend that holds backup objects. Every
	// GET ?key=X is validated to live under a real tenant prefix in
	// the same bucket — we never Presign cross-bucket or absolute URLs.
	Archive storage.Store

	// PresignTTL bounds how long the signed URL is valid. The browser
	// follows our 302 within milliseconds; the TTL only matters if the
	// URL is copied out of the redirect chain. 10 minutes is enough for
	// a slow operator browser + no longer than needed.
	//
	// Zero value → 10 minutes.
	PresignTTL time.Duration
}

// Download serves `GET /api/v1/download?key=<path>` — the SPA's archive-
// download link (ArchivesTab's `<a href={downloadUrl(a.key)}>`).
//
// Auth is enforced by the surrounding JWT middleware — this handler only
// runs for authenticated operator sessions.
//
// Flow:
//  1. Read `key` from query; refuse empty, absolute URLs, path-traversal.
//  2. Ask the storage backend to `Presign` the key. For S3 / GCS backends
//     this mints a short-lived signed GET URL. For the FSStore it mints
//     an opaque HMAC token path the file-server handler then serves
//     locally.
//  3. 302-redirect to the resulting URL. Browser follows, downloads.
//
// Returning 302 (vs. streaming the file through this Lambda) keeps the
// Lambda's response body size cost near zero — archives can be hundreds
// of MB and API Gateway caps response bodies at 6 MB regardless of
// invokeMode. The function-URL path has no such cap but the 302 pattern
// is still the right shape: the client talks to S3 directly, byte-range
// resumable, and we don't pay egress twice.
func (c *DownloadConfig) Download(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, `{"error":"key query param required"}`, http.StatusBadRequest)
		return
	}
	// Reject absolute URLs and scheme-prefixed inputs. A malicious
	// `key=https://example.com/pwned` would otherwise land verbatim in
	// the Presign path and, for some backends, could Presign against
	// arbitrary hosts.
	if strings.Contains(key, "://") {
		http.Error(w, `{"error":"key must be a relative path within the archive bucket, not a URL"}`, http.StatusBadRequest)
		return
	}
	// Refuse path traversal. Cleaned key segments that include `..`
	// could escape into another tenant's prefix or outside the bucket
	// root. The Store impls generally normalise this too, but a loud
	// 400 at the HTTP boundary keeps the diagnostic right where the
	// operator sees it.
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			http.Error(w, `{"error":"key must not contain traversal segments"}`, http.StatusBadRequest)
			return
		}
	}
	// Leading slashes are harmless (Store impls TrimPrefix), but leave
	// no room for the operator to wonder whether `/foo` and `foo` are
	// different objects — normalise once.
	key = strings.TrimPrefix(key, "/")

	ttl := c.PresignTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	url, err := c.Archive.Presign(r.Context(), key, ttl)
	if err != nil {
		if err == storage.ErrNotFound {
			http.Error(w, `{"error":"archive not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"failed to presign archive URL"}`, http.StatusInternalServerError)
		return
	}
	// The browser follows the 302 and starts the download. We do NOT
	// echo the presigned URL in a JSON body because the SPA opens the
	// `/download?key=…` URL directly (no fetch wrapper) — the cheapest
	// "trigger a download" shape in the browser is a 302.
	http.Redirect(w, r, url, http.StatusFound)
}
