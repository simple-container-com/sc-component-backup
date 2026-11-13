// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
)

// SnapshotStore persists `db_snapshots` + `object_samples` rows
// (schema mirrors local-db-backup `history.py`) and surfaces the three
// read patterns the stats package needs.
//
// Reads return newest-first by Ts — the stats aggregators then re-sort
// ASC internally, but newest-first here lets the handler apply a
// cutoff cheaply.
//
// Separate from RunStore because the row lifecycle differs — snapshots
// are kept forever (observation history is independent of the archive
// retention sweep).
type SnapshotStore interface {
	// PutDbSnapshot inserts a single DbSnapshot. Caller sets
	// Snapshot.ID (unique). Idempotent on ID.
	PutDbSnapshot(ctx context.Context, s stats.DbSnapshot) error

	// PutObjectSamples replaces the object samples bound to one
	// snapshot. `snapshotID` is the parent DbSnapshot's ID; writes are
	// atomic per (project, database, key) tuple — a replay with the
	// same key drops the previous rows and writes the new ones.
	PutObjectSamples(ctx context.Context, project, database, key string, snapshotID int64, samples []stats.ObjectSample) error

	// SnapshotsForProject returns the snapshots of `project`
	// since `since` (inclusive). When `since` is zero, returns every
	// recorded snapshot. Newest first by Ts.
	SnapshotsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbSnapshot, error)

	// ObjectSamplesForProject returns every ObjectSample bound to a
	// snapshot of (project, database) whose parent snapshot is newer
	// than `since`. Ordering by snapshot Ts DESC is preserved so the
	// aggregator can walk them without an extra sort.
	ObjectSamplesForProject(ctx context.Context, project, database string, since time.Time) ([]stats.ObjectSample, error)

	// RunResultsForProject returns the DbResult (one per BackupRun)
	// for the project since `since`. Delegated to the RunStore —
	// prefer that single source of truth over a parallel collection.
	RunResultsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbResult, error)

	Close(ctx context.Context) error
}

// ProjectDbResults is a pure helper exposed for handlers that already
// have a `[]BackupRun` on hand — maps the run slice to the stats.DbResult
// shape without going through Mongo twice. `since` zero disables the
// cutoff.
func ProjectDbResults(runs []backup.BackupRun, since time.Time) []stats.DbResult {
	out := make([]stats.DbResult, 0, len(runs))
	for _, r := range runs {
		if !since.IsZero() && r.StartedAt.Before(since) {
			continue
		}
		out = append(out, stats.DbResult{
			StartedAt: r.StartedAt,
			Status:    mapRunStatus(r.Status),
		})
	}
	return out
}

// mapRunStatus narrows the BackupRun status vocabulary to the two
// labels DbResult carries — the stats health strip only tracks
// ok/failed counts.
func mapRunStatus(s backup.Status) string {
	switch s {
	case backup.StatusSucceeded:
		return "ok"
	case backup.StatusFailed:
		return "failed"
	default:
		return string(s)
	}
}

// InMemorySnapshotStore satisfies SnapshotStore for dev + tests. The
// production implementation is `store_snapshots_mongo.go`.
type InMemorySnapshotStore struct {
	mu        sync.RWMutex
	snapshots []stats.DbSnapshot
	// samples keyed by (project, database, key) so a replay replaces.
	samples map[string][]stats.ObjectSample
	runs    RunStore
}

// NewInMemorySnapshotStore builds an in-memory snapshot store. `runs`
// is the RunStore used to project DbResults — pass the same one the
// scheduler writes to so the health strip reflects live data.
func NewInMemorySnapshotStore(runs RunStore) *InMemorySnapshotStore {
	return &InMemorySnapshotStore{
		snapshots: []stats.DbSnapshot{},
		samples:   map[string][]stats.ObjectSample{},
		runs:      runs,
	}
}

func (s *InMemorySnapshotStore) PutDbSnapshot(_ context.Context, snap stats.DbSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Replace on ID collision.
	for i, existing := range s.snapshots {
		if existing.ID == snap.ID {
			s.snapshots[i] = snap
			return nil
		}
	}
	s.snapshots = append(s.snapshots, snap)
	return nil
}

func (s *InMemorySnapshotStore) PutObjectSamples(_ context.Context, project, database, key string, snapshotID int64, samples []stats.ObjectSample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := project + "\x00" + database + "\x00" + key
	// Decorate each sample with its parent snapshot id so the read path can group.
	owned := make([]stats.ObjectSample, 0, len(samples))
	for _, so := range samples {
		so.SnapshotID = snapshotID
		owned = append(owned, so)
	}
	s.samples[k] = owned
	return nil
}

func (s *InMemorySnapshotStore) SnapshotsForProject(_ context.Context, project string, since time.Time) ([]stats.DbSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]stats.DbSnapshot, 0, len(s.snapshots))
	for _, sn := range s.snapshots {
		if sn.Project != project {
			continue
		}
		if !since.IsZero() && sn.Ts.Before(since) {
			continue
		}
		out = append(out, sn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts.After(out[j].Ts) })
	return out, nil
}

func (s *InMemorySnapshotStore) ObjectSamplesForProject(_ context.Context, project, database string, since time.Time) ([]stats.ObjectSample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Build a lookup of snapshot_id → Ts for the project+database so
	// we can filter samples by their parent's Ts.
	parentTs := map[int64]time.Time{}
	for _, sn := range s.snapshots {
		if sn.Project != project || sn.Database != database {
			continue
		}
		parentTs[sn.ID] = sn.Ts
	}

	// Gather samples whose parent snapshot matches.
	collected := []stats.ObjectSample{}
	for k, batch := range s.samples {
		// key shape: project\x00database\x00archive-key
		if len(k) < len(project)+len(database)+2 {
			continue
		}
		if k[:len(project)] != project || k[len(project)+1:len(project)+1+len(database)] != database {
			continue
		}
		for _, so := range batch {
			ts, ok := parentTs[so.SnapshotID]
			if !ok {
				continue
			}
			if !since.IsZero() && ts.Before(since) {
				continue
			}
			collected = append(collected, so)
		}
	}
	// Order by parent-snapshot Ts DESC so the aggregator sees newest-first.
	sort.Slice(collected, func(i, j int) bool {
		return parentTs[collected[i].SnapshotID].After(parentTs[collected[j].SnapshotID])
	})
	return collected, nil
}

func (s *InMemorySnapshotStore) RunResultsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbResult, error) {
	if s.runs == nil {
		return []stats.DbResult{}, nil
	}
	runs, err := s.runs.ListBackupsByTenant(ctx, project)
	if err != nil {
		return nil, err
	}
	return ProjectDbResults(runs, since), nil
}

func (s *InMemorySnapshotStore) Close(_ context.Context) error { return nil }
