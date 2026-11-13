// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FSStore backs the archive Store with one operator-owned directory on
// the deploy host's filesystem. Appropriate for VM mounts, k8s
// persistent volumes, and ECS + EFS. Not valid under Lambda — the
// ephemeral /tmp evaporates on cold start (the plugin's config
// validator refuses that combination at parse time).
//
// Presign works differently here than on S3 / GCS: a filesystem key
// has no external URL, so Presign mints a short-lived opaque token
// that the service's own `GET /api/v1/files/{token}` route accepts
// (see api.NewFilesHandler). The caller — the SPA's download link
// handler — treats the return as an opaque URL and resolves relative
// paths against the request's host header.
//
// Layout: every Store key maps to <root>/<key> with "/" as the path
// separator. Keys are sanitised to refuse "..": no key may escape the
// root directory.
type FSStore struct {
	root        string
	presignHmac []byte
	mu          sync.Mutex
	// activeTokens carries live presign tokens so an operator restart
	// doesn't immediately invalidate a page the user just loaded.
	// Expired tokens are swept at serve time.
	activeTokens map[string]fsPresignRow
}

type fsPresignRow struct {
	path    string
	expires time.Time
}

// FSConfig bundles the knobs. Root MUST be an absolute path — the
// service's working directory varies by deploy shape (/ in Lambda,
// /var/task in ECS, the SC compose dir on VM), and a relative root
// is too ambiguous to be worth supporting.
type FSConfig struct {
	Root string
	// PresignHMACKey signs presign tokens. When empty, FSStore rolls a
	// random 32-byte key at startup — fine for single-replica deploys,
	// not fine for multi-replica (tokens minted by one replica
	// wouldn't verify on another). Operators who run multiple
	// replicas should set this explicitly; the service wires it from
	// SERVICE_JWT_SECRET by default.
	PresignHMACKey []byte
}

// NewFSStore builds an FSStore after confirming Root exists + is a
// directory. Creates it if absent (useful for a first-boot k8s PV
// that hasn't been written to yet).
func NewFSStore(_ context.Context, cfg FSConfig) (*FSStore, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("storage.NewFSStore: Root is required")
	}
	if !filepath.IsAbs(cfg.Root) {
		return nil, fmt.Errorf("storage.NewFSStore: Root must be absolute, got %q", cfg.Root)
	}
	if err := os.MkdirAll(cfg.Root, 0o750); err != nil {
		return nil, fmt.Errorf("storage.NewFSStore: mkdir root %q: %w", cfg.Root, err)
	}
	st, err := os.Stat(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("storage.NewFSStore: stat root: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("storage.NewFSStore: Root %q is not a directory", cfg.Root)
	}
	key := cfg.PresignHMACKey
	if len(key) == 0 {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("storage.NewFSStore: random key: %w", err)
		}
		key = b[:]
	}
	return &FSStore{
		root:         cfg.Root,
		presignHmac:  key,
		activeTokens: map[string]fsPresignRow{},
	}, nil
}

// resolveKey validates a user-supplied key against path-traversal
// attacks + returns the absolute filesystem path. Keys that would
// escape the root are rejected with a typed error.
func (s *FSStore) resolveKey(key string) (string, error) {
	key = strings.TrimPrefix(key, "/")
	if key == "" {
		return "", errors.New("storage: empty key")
	}
	full := filepath.Join(s.root, key)
	// Clean + confirm the result still lives under root. filepath.Join
	// resolves ".." so a crafted key like "../../etc/passwd" becomes an
	// absolute path outside root; the HasPrefix guard refuses that.
	rootClean := filepath.Clean(s.root) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(full)+string(filepath.Separator), rootClean) {
		return "", fmt.Errorf("storage: key %q escapes root", key)
	}
	return full, nil
}

// Put writes body to <root>/<path>. Atomic: writes to a per-call temp
// file in the same directory + renames into place. A crash mid-write
// leaves the temp file (prefix ".tmp-"); a periodic sweep is a
// slice-1e.4-follow-up.
func (s *FSStore) Put(_ context.Context, path string, body io.Reader) (int64, error) {
	full, err := s.resolveKey(path)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return 0, fmt.Errorf("fs put: mkdir %q: %w", filepath.Dir(full), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-"+filepath.Base(full)+"-*")
	if err != nil {
		return 0, fmt.Errorf("fs put: temp file: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	n, err := io.Copy(tmp, body)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return 0, fmt.Errorf("fs put: copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return 0, fmt.Errorf("fs put: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), full); err != nil {
		cleanup()
		return 0, fmt.Errorf("fs put: rename: %w", err)
	}
	return n, nil
}

// Get returns a ReadCloser on <root>/<path>. Missing file → ErrNotFound.
func (s *FSStore) Get(_ context.Context, path string) (io.ReadCloser, error) {
	full, err := s.resolveKey(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("fs get: %w", err)
	}
	return f, nil
}

// List walks <root>/<prefix>'s subtree. Prefix may include a trailing
// substring that matches partial basenames (e.g. "foo/2026-" lists
// everything under foo/ whose name starts with "2026-"). Returns
// unsorted; callers that care sort by Path.
func (s *FSStore) List(_ context.Context, prefix string) ([]StoredObject, error) {
	prefix = strings.TrimPrefix(prefix, "/")
	// The walk root is the deepest directory that's unambiguous from
	// the prefix. If prefix is "foo/bar/", walk <root>/foo/bar. If it's
	// "foo/bar-", walk <root>/foo and filter on basename prefix.
	walkRoot := filepath.Join(s.root, prefix)
	st, err := os.Stat(walkRoot)
	if err == nil && st.IsDir() {
		return s.listDir(walkRoot)
	}
	// Prefix isn't a directory on its own — walk the parent + filter.
	parent := filepath.Dir(walkRoot)
	basePrefix := filepath.Base(walkRoot)
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fs list: readdir %q: %w", parent, err)
	}
	out := []StoredObject{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), basePrefix) {
			continue
		}
		child := filepath.Join(parent, e.Name())
		if e.IsDir() {
			sub, _ := s.listDir(child)
			out = append(out, sub...)
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(s.root, child)
		out = append(out, StoredObject{
			Path:       filepath.ToSlash(rel),
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
		})
	}
	return out, nil
}

func (s *FSStore) listDir(dir string) ([]StoredObject, error) {
	out := []StoredObject{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip in-flight temp files from a concurrent Put.
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // best effort
		}
		rel, _ := filepath.Rel(s.root, path)
		out = append(out, StoredObject{
			Path:       filepath.ToSlash(rel),
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fs list: walk %q: %w", dir, err)
	}
	return out, nil
}

// Delete removes <root>/<path>. Missing file → ErrNotFound. Trailing
// empty directories are left in place; a periodic sweep handles them
// (slice 1e.4 follow-up).
func (s *FSStore) Delete(_ context.Context, path string) error {
	full, err := s.resolveKey(path)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("fs delete: %w", err)
	}
	return nil
}

// Presign mints a short-lived opaque token. Returns a RELATIVE URL
// starting with "/api/v1/files/" — the HTTP layer rewrites this to an
// absolute URL against the request's host header. Token holds a 32-
// byte random id; the id → (path, expires) mapping lives in the
// FSStore's own in-memory state (fine for single-replica; multi-
// replica needs the shared PresignHMACKey + stateless HMAC token
// shape — follow-up).
func (s *FSStore) Presign(_ context.Context, path string, expires time.Duration) (string, error) {
	if expires <= 0 {
		expires = 5 * time.Minute
	}
	var tok [24]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return "", fmt.Errorf("fs presign: random: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(tok[:])
	s.mu.Lock()
	// Sweep expired tokens cheaply during insert.
	now := time.Now()
	for k, row := range s.activeTokens {
		if row.expires.Before(now) {
			delete(s.activeTokens, k)
		}
	}
	s.activeTokens[id] = fsPresignRow{path: path, expires: now.Add(expires)}
	s.mu.Unlock()
	return "/api/v1/files/" + id, nil
}

// ResolvePresign reads back a token minted via Presign. Expired or
// unknown tokens return an empty path + false. Used by the HTTP file-
// serve handler (api/files_handler.go — slice 1e.4a follow-up).
func (s *FSStore) ResolvePresign(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.activeTokens[token]
	if !ok {
		return "", false
	}
	if row.expires.Before(time.Now()) {
		delete(s.activeTokens, token)
		return "", false
	}
	return row.path, true
}

// Root returns the filesystem root the store is bound to — operator
// diagnostics / logs read this.
func (s *FSStore) Root() string { return s.root }
