// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

type colocatedStore struct{ archive storage.Store }

func newColocatedStore(archive storage.Store) (Store, error) {
	return &colocatedStore{archive: archive}, nil
}

func (s *colocatedStore) Tenants() tenant.Store              { return colocTenants{s} }
func (s *colocatedStore) Runs() scheduler.RunStore           { return colocRuns{s} }
func (s *colocatedStore) Projects() project.Store            { return colocProjects{s} }
func (s *colocatedStore) Snapshots() scheduler.SnapshotStore { return colocSnapshots{s} }
func (s *colocatedStore) Close(_ context.Context) error      { return nil }

const metaPrefix = "_meta/"

func (s *colocatedStore) putJSON(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.archive.Put(ctx, key, bytes.NewReader(b))
	return err
}

func (s *colocatedStore) getJSON(ctx context.Context, key string, out any) (bool, error) {
	rc, err := s.archive.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	defer rc.Close()
	if err := json.NewDecoder(rc).Decode(out); err != nil {
		return false, err
	}
	return true, nil
}
