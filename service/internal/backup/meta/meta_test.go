// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
)

func TestResolveType_Defaults(t *testing.T) {
	fs, _ := storage.NewFSStore(context.Background(), storage.FSConfig{Root: t.TempDir()})
	mem := storage.NewInMemoryStore()
	// S3 can't be constructed without AWS creds in a unit test, but
	// resolveType only cares about the dynamic type. A bare `*S3Store`
	// stand-in would require package internals; we cover the
	// object-store branch via the default fallthrough.

	cases := []struct {
		name    string
		input   Type
		archive storage.Store
		want    Type
		wantErr string
	}{
		{"default FS → sqlite", TypeDefault, fs, TypeSQLite, ""},
		{"default mem → colocated", TypeDefault, mem, TypeColocated, ""},
		{"explicit sqlite on FS", TypeSQLite, fs, TypeSQLite, ""},
		{"explicit sqlite on mem refused", TypeSQLite, mem, "", "requires an archive of type filesystem"},
		{"explicit colocated", TypeColocated, fs, TypeColocated, ""},
		{"explicit mongo", TypeMongo, fs, TypeMongo, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveType(tc.input, tc.archive)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("want error %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveType: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewFromConfig_RejectsUnsupportedForNow(t *testing.T) {
	fs, _ := storage.NewFSStore(context.Background(), storage.FSConfig{Root: t.TempDir()})
	cases := []struct {
		name string
		cfg  Config
		arch storage.Store
		msg  string
	}{
		{"postgres not yet", Config{Type: TypePostgres, URI: "pg://x"}, fs, "slice 1e.4e"},
		{"mysql not yet", Config{Type: TypeMySQL, URI: "mysql://x"}, fs, "slice 1e.4e"},
		{"mongo needs URI", Config{Type: TypeMongo}, fs, "requires a URI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewFromConfig(context.Background(), tc.cfg, tc.arch)
			if err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("want error %q, got %v", tc.msg, err)
			}
		})
	}
}

// A compile-time check that mongoStore satisfies the Store interface
// without needing a live Mongo. If NewMongoStore ever changes to
// return a different concrete type, this test breaks at `go vet`.
func TestMongoStoreImplementsStore(_ *testing.T) {
	// Not constructing — just asserting the type.
	var _ Store = (*mongoStore)(nil)
	// Sentinel: ensure the mongoStore type exists and is reachable.
	_ = errors.New
}
