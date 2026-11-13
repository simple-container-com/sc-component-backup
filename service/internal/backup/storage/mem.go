// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// InMemoryStore backs the Store interface with an in-process map.
// Used by every package's unit tests that want a store but don't want
// a real S3 / filesystem roundtrip.
type InMemoryStore struct {
	mu      sync.RWMutex
	objects map[string]memObject
	tokens  map[string]string // id -> path
}

type memObject struct {
	body       []byte
	modifiedAt time.Time
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		objects: map[string]memObject{},
		tokens:  map[string]string{},
	}
}

func (s *InMemoryStore) Put(_ context.Context, path string, body io.Reader) (int64, error) {
	path = strings.TrimPrefix(path, "/")
	buf, err := io.ReadAll(body)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.objects[path] = memObject{body: buf, modifiedAt: time.Now().UTC()}
	s.mu.Unlock()
	return int64(len(buf)), nil
}

func (s *InMemoryStore) Get(_ context.Context, path string) (io.ReadCloser, error) {
	path = strings.TrimPrefix(path, "/")
	s.mu.RLock()
	obj, ok := s.objects[path]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(obj.body)), nil
}

func (s *InMemoryStore) List(_ context.Context, prefix string) ([]StoredObject, error) {
	prefix = strings.TrimPrefix(prefix, "/")
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]StoredObject, 0, len(s.objects))
	for k, v := range s.objects {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		out = append(out, StoredObject{Path: k, Size: int64(len(v.body)), ModifiedAt: v.modifiedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *InMemoryStore) Delete(_ context.Context, path string) error {
	path = strings.TrimPrefix(path, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[path]; !ok {
		return ErrNotFound
	}
	delete(s.objects, path)
	return nil
}

// Presign mints a stub token — the InMemoryStore has no HTTP serve
// layer, callers that want the bytes read Get() directly. Returned
// token still resolves via ResolvePresign so SPA-smoke tests exercising
// the "download URL" shape work end-to-end.
func (s *InMemoryStore) Presign(_ context.Context, path string, _ time.Duration) (string, error) {
	if _, ok := s.objects[strings.TrimPrefix(path, "/")]; !ok {
		return "", errors.New("mem presign: key not found")
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := base64.RawURLEncoding.EncodeToString(b[:])
	s.mu.Lock()
	s.tokens[id] = strings.TrimPrefix(path, "/")
	s.mu.Unlock()
	return "/api/v1/files/" + id, nil
}

// ResolvePresign is the test-side companion to Presign.
func (s *InMemoryStore) ResolvePresign(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.tokens[token]
	return p, ok
}
