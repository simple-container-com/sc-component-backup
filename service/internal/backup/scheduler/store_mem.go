// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// InMemoryRunStore satisfies RunStore for dev + smoke. Mongo-backed
// store sits alongside; both share the interface.
type InMemoryRunStore struct {
	mu       sync.RWMutex
	backups  map[string]backup.BackupRun
	restores map[string]backup.RestoreRun
}

func NewInMemoryRunStore() *InMemoryRunStore {
	return &InMemoryRunStore{
		backups:  map[string]backup.BackupRun{},
		restores: map[string]backup.RestoreRun{},
	}
}

func (s *InMemoryRunStore) PutBackup(_ context.Context, run backup.BackupRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backups[run.ID] = run
	return nil
}

func (s *InMemoryRunStore) UpdateBackup(ctx context.Context, run backup.BackupRun) error {
	return s.PutBackup(ctx, run)
}

func (s *InMemoryRunStore) GetBackup(_ context.Context, id string) (backup.BackupRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.backups[id]
	if !ok {
		return backup.BackupRun{}, fmt.Errorf("backup-run %q: not found", id)
	}
	return r, nil
}

func (s *InMemoryRunStore) ListBackupsByTenant(_ context.Context, tenantStack string) ([]backup.BackupRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]backup.BackupRun, 0, len(s.backups))
	for _, r := range s.backups {
		if r.TenantStack == tenantStack {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

func (s *InMemoryRunStore) PutRestore(_ context.Context, run backup.RestoreRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restores[run.ID] = run
	return nil
}

func (s *InMemoryRunStore) UpdateRestore(ctx context.Context, run backup.RestoreRun) error {
	return s.PutRestore(ctx, run)
}

func (s *InMemoryRunStore) GetRestore(_ context.Context, id string) (backup.RestoreRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.restores[id]
	if !ok {
		return backup.RestoreRun{}, fmt.Errorf("restore-run %q: not found", id)
	}
	return r, nil
}
