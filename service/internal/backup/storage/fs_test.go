// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFSStore_PutGetRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := NewFSStore(context.Background(), FSConfig{Root: root})
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	n, err := s.Put(context.Background(), "demo/a.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != 5 {
		t.Errorf("Put size: got %d, want 5", n)
	}
	rc, err := s.Get(context.Background(), "demo/a.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello" {
		t.Errorf("Get body: %q", got)
	}
}

func TestFSStore_GetMissingReturnsErrNotFound(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(context.Background(), FSConfig{Root: root})
	_, err := s.Get(context.Background(), "nope.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: want ErrNotFound, got %v", err)
	}
}

func TestFSStore_PutIsAtomic(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(context.Background(), FSConfig{Root: root})
	// Concurrent Put to the same key: the final file is one of the
	// bodies, never a mix, and no leftover tmp file remains.
	done := make(chan struct{})
	bodies := []string{strings.Repeat("A", 1024), strings.Repeat("B", 1024)}
	for i := 0; i < 2; i++ {
		go func(i int) {
			_, _ = s.Put(context.Background(), "race.bin", bytes.NewReader([]byte(bodies[i])))
			done <- struct{}{}
		}(i)
	}
	<-done
	<-done
	rc, _ := s.Get(context.Background(), "race.bin")
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != bodies[0] && string(got) != bodies[1] {
		t.Errorf("Put race produced a torn body: len %d", len(got))
	}
	// No lingering tmp files.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("lingering temp file: %s", e.Name())
		}
	}
}

func TestFSStore_PathTraversalRefused(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(context.Background(), FSConfig{Root: root})
	// `..` based escapes: these MUST be refused.
	for _, key := range []string{
		"../../etc/passwd",
		"foo/../../../etc/passwd",
	} {
		_, err := s.Put(context.Background(), key, strings.NewReader("x"))
		if err == nil {
			t.Errorf("Put(%q): expected refusal, got none", key)
		}
	}
	// Leading `/` on a Store key is semantically "root of the store",
	// not "root of the filesystem" — S3Store strips it identically.
	// Accepted + lands under <root>/absolute/elsewhere.
	if _, err := s.Put(context.Background(), "/absolute/elsewhere", strings.NewReader("x")); err != nil {
		t.Errorf("Put(/absolute/elsewhere): leading-slash should be accepted, got %v", err)
	}
	// Sanity: root dir has no stray files written.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") || strings.HasSuffix(e.Name(), "passwd") {
			t.Errorf("path-traversal attempt leaked file: %s", e.Name())
		}
	}
}

func TestFSStore_ListByPrefix(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(context.Background(), FSConfig{Root: root})
	for _, p := range []string{
		"a/one.txt", "a/two.txt", "b/three.txt", "a/sub/nested.txt",
	} {
		_, _ = s.Put(context.Background(), p, strings.NewReader(p))
	}
	rows, err := s.List(context.Background(), "a/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("List under a/: got %d, want 3; rows=%v", len(rows), rows)
	}
	// Check basename-prefix shape: list "a/sub" rather than "a/sub/".
	rows, _ = s.List(context.Background(), "a/sub")
	if len(rows) != 1 || !strings.HasSuffix(rows[0].Path, "nested.txt") {
		t.Errorf("List under a/sub: got %v", rows)
	}
	// Check basename-partial: "a/t" matches a/two.txt.
	rows, _ = s.List(context.Background(), "a/t")
	if len(rows) != 1 || !strings.HasSuffix(rows[0].Path, "two.txt") {
		t.Errorf("List basename-partial a/t: got %v", rows)
	}
}

func TestFSStore_Delete(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(context.Background(), FSConfig{Root: root})
	_, _ = s.Put(context.Background(), "a.txt", strings.NewReader("x"))
	if err := s.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Second delete → ErrNotFound.
	if err := s.Delete(context.Background(), "a.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Second delete: want ErrNotFound, got %v", err)
	}
}

func TestFSStore_PresignRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(context.Background(), FSConfig{Root: root})
	_, _ = s.Put(context.Background(), "demo.bin", strings.NewReader("data"))

	url, err := s.Presign(context.Background(), "demo.bin", 5*time.Minute)
	if err != nil {
		t.Fatalf("Presign: %v", err)
	}
	const prefix = "/api/v1/files/"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("Presign URL shape: %q", url)
	}
	token := strings.TrimPrefix(url, prefix)
	path, ok := s.ResolvePresign(token)
	if !ok {
		t.Fatalf("ResolvePresign: not found")
	}
	if path != "demo.bin" {
		t.Errorf("ResolvePresign path: %q", path)
	}
}

func TestFSStore_PresignExpires(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(context.Background(), FSConfig{Root: root})
	_, _ = s.Put(context.Background(), "demo.bin", strings.NewReader("data"))

	url, _ := s.Presign(context.Background(), "demo.bin", 10*time.Millisecond)
	token := strings.TrimPrefix(url, "/api/v1/files/")
	time.Sleep(20 * time.Millisecond)
	if _, ok := s.ResolvePresign(token); ok {
		t.Error("expected expired token to fail resolve")
	}
}

func TestFSStore_NewRefusesRelativeRoot(t *testing.T) {
	_, err := NewFSStore(context.Background(), FSConfig{Root: "relative/path"})
	if err == nil {
		t.Error("expected refusal on relative Root")
	}
}

func TestFSStore_RootCreatedOnDemand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "will-be-created")
	s, err := NewFSStore(context.Background(), FSConfig{Root: root})
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	st, err := os.Stat(s.Root())
	if err != nil || !st.IsDir() {
		t.Error("Root was not created on-demand")
	}
}
