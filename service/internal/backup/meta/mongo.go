// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"fmt"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// mongoStore composes the four existing Mongo-backed narrow stores
// (shipped in slices 1e.2/1e.3) under one Close + one lifecycle.
//
// Each concrete store owns its own *mongo.Client because that's the
// shape they shipped with — connecting four times to the same URI is
// cheap + keeps each constructor's Ping + index-create isolated. A
// future slice can collapse them to one shared client; not worth the
// refactor today.
type mongoStore struct {
	tenants  *tenant.MongoStore
	runs     *scheduler.MongoRunStore
	snaps    *scheduler.MongoSnapshotStore
	projects *project.MongoStore
}

func newMongoStore(ctx context.Context, uri string) (Store, error) {
	tenants, err := tenant.NewMongoStore(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("meta mongo: tenant store: %w", err)
	}
	runs, err := scheduler.NewMongoRunStore(ctx, uri, "")
	if err != nil {
		_ = tenants.Close(ctx)
		return nil, fmt.Errorf("meta mongo: run store: %w", err)
	}
	// Snapshot store needs the run store to project DbResults.
	snaps, err := scheduler.NewMongoSnapshotStore(ctx, uri, "", runs)
	if err != nil {
		_ = tenants.Close(ctx)
		_ = runs.Close(ctx)
		return nil, fmt.Errorf("meta mongo: snapshot store: %w", err)
	}
	projects, err := project.NewMongoStore(ctx, uri)
	if err != nil {
		_ = tenants.Close(ctx)
		_ = runs.Close(ctx)
		_ = snaps.Close(ctx)
		return nil, fmt.Errorf("meta mongo: project store: %w", err)
	}
	return &mongoStore{tenants: tenants, runs: runs, snaps: snaps, projects: projects}, nil
}

func (s *mongoStore) Tenants() tenant.Store              { return s.tenants }
func (s *mongoStore) Runs() scheduler.RunStore           { return s.runs }
func (s *mongoStore) Projects() project.Store            { return s.projects }
func (s *mongoStore) Snapshots() scheduler.SnapshotStore { return s.snaps }

func (s *mongoStore) Close(ctx context.Context) error {
	var firstErr error
	for _, closer := range []func(context.Context) error{
		s.projects.Close,
		s.snaps.Close,
		s.runs.Close,
		s.tenants.Close,
	} {
		if err := closer(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
