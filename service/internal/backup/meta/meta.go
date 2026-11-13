// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package meta — the metadata-store umbrella for sc-component-backup.
//
// Slice 1e.4b of sc-components-platform. The service persists four
// row families — tenants, projects, backup + restore runs,
// snapshots + object samples — each with its own narrow interface
// (tenant.Store / scheduler.RunStore / project.Store /
// scheduler.SnapshotStore). This package aggregates the four into a
// single Store contract so cmd/backup/main.go picks ONE metadata
// backend at startup.
//
// Design note on the aggregation shape: the four narrow interfaces
// have COLLIDING method names (tenant.Store.Get vs project.Store.Get,
// tenant.Store.SoftDelete vs project.Store.SoftDelete) with
// different signatures, so we can't embed them in a single aggregate
// interface. Instead, meta.Store is a thin FACET container:
//
//	type Store interface {
//	    Tenants()   tenant.Store
//	    Runs()      scheduler.RunStore
//	    Projects()  project.Store
//	    Snapshots() scheduler.SnapshotStore
//	    Close(ctx context.Context) error
//	}
//
// Handlers reach each facet via its accessor + keep their existing
// narrow dependencies. The backend decision lives in ONE place
// (cmd/backup/main.go's `meta.NewFromConfig` call); every handler
// continues to depend on exactly the narrow interface it needs.
//
// Backends:
//
//   - mongo     — already shipped (slice 1e.2/1e.3); composed here
//     into the aggregate.
//   - postgres  — slice 1e.4e.
//   - mysql     — slice 1e.4e.
//   - sqlite    — slice 1e.4d.
//   - colocated — slice 1e.4c; wraps storage.Store with JSON keys
//     under a _meta/ prefix.
//
// Default resolution (when operator doesn't set Config.Type):
//
//   - archive is object-store (s3/gcs)  → "colocated"
//   - archive is filesystem             → "sqlite"
//
// Design: forge docs/design/2026-11-13/sc-component-backup-pluggable-storage.
package meta

import (
	"context"
	"fmt"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// Store aggregates the four narrow stores via accessor methods — see
// the package doc for why this isn't a simple interface-embedding
// shape.
type Store interface {
	Tenants() tenant.Store
	Runs() scheduler.RunStore
	Projects() project.Store
	Snapshots() scheduler.SnapshotStore
	Close(ctx context.Context) error
}

// Type is the metadata backend selector. Operator-faced; parsed from
// `metadata.type:` in server.yaml (slice 1e.4g).
type Type string

const (
	TypeDefault   Type = ""
	TypeMongo     Type = "mongo"
	TypePostgres  Type = "postgres"
	TypeMySQL     Type = "mysql"
	TypeSQLite    Type = "sqlite"
	TypeColocated Type = "colocated"
)

// Config picks the metadata backend + carries its connection info.
// Mirrors the server.yaml `metadata:` block from the design doc.
type Config struct {
	// Type selects the backend. Empty is resolved to a default based
	// on the paired archive store — see package doc.
	Type Type
	// URI is the DB connection string for mongo/postgres/mysql.
	URI string
	// SQLitePath overrides the default SQLite file location
	// (<storage.root>/_meta/sc-component-backup.sqlite). Ignored for
	// non-SQLite backends.
	SQLitePath string
}

// NewFromConfig builds a meta.Store for the chosen backend. The
// `archive` argument is the paired storage.Store — used by the
// colocated backend AND by default-type resolution (filesystem
// archive → sqlite default; object-store archive → colocated
// default).
//
// Errors are early + explicit: an unsupported Type pair (e.g.
// sqlite + s3 archive) fails here rather than at first write.
func NewFromConfig(ctx context.Context, cfg Config, archive storage.Store) (Store, error) {
	t, err := resolveType(cfg.Type, archive)
	if err != nil {
		return nil, err
	}
	switch t {
	case TypeMongo:
		if cfg.URI == "" {
			return nil, fmt.Errorf("meta: mongo backend requires a URI")
		}
		return newMongoStore(ctx, cfg.URI)
	case TypeColocated:
		return newColocatedStore(archive)
	case TypeSQLite:
		return newSQLiteStore(ctx, cfg, archive)
	case TypePostgres:
		return newPostgresStore(ctx, cfg.URI)
	case TypeMySQL:
		return newMySQLStore(ctx, cfg.URI)
	default:
		return nil, fmt.Errorf("meta: unknown backend type %q (one of mongo/postgres/mysql/sqlite/colocated)", t)
	}
}

// resolveType turns an unset Type into a default based on the archive
// backend. Also rejects invalid pairings up front.
func resolveType(t Type, archive storage.Store) (Type, error) {
	if t != TypeDefault {
		// Up-front rejection: SQLite with a non-filesystem archive
		// doesn't work. Object stores don't do random writes (download-
		// modify-upload is worse than colocated JSON), and the
		// InMemoryStore would leave archives in RAM while metadata lands
		// on disk — tests that want SQLite round-trips should pair it
		// with an FSStore under t.TempDir().
		if t == TypeSQLite {
			if _, isFS := archive.(*storage.FSStore); !isFS {
				return "", fmt.Errorf("meta: sqlite backend requires an archive of type filesystem (got %T)", archive)
			}
		}
		return t, nil
	}
	// Default resolution.
	switch archive.(type) {
	case *storage.FSStore:
		return TypeSQLite, nil
	case *storage.InMemoryStore:
		return TypeColocated, nil // tests default to colocated so they can round-trip without SQLite deps
	default:
		return TypeColocated, nil // any object store → colocated
	}
}
