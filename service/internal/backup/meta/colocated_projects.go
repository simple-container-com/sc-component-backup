// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

import (
	"context"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/retention"
)

type colocProjects struct{ s *colocatedStore }

func projectKey(name string) string {
	return metaPrefix + "projects/" + retention.Enc(name) + ".json"
}

func (p colocProjects) Create(ctx context.Context, cfg project.Config) (project.Config, error) {
	now := time.Now().UTC()
	existing, ok, err := p.readRaw(ctx, cfg.Name)
	if err != nil {
		return project.Config{}, err
	}
	if ok && !existing.SoftDeleted {
		return project.Config{}, project.ErrConflict
	}
	cfg.CreatedAt = now
	cfg.UpdatedAt = now
	cfg.SoftDeleted = false
	if cfg.Revision == "" {
		cfg.Revision = project.NewRevision()
	}
	if err := p.s.putJSON(ctx, projectKey(cfg.Name), toStored(cfg)); err != nil {
		return project.Config{}, err
	}
	return cfg, nil
}

func (p colocProjects) Get(ctx context.Context, name string) (project.Config, bool, error) {
	cfg, ok, err := p.readRaw(ctx, name)
	if err != nil || !ok {
		return project.Config{}, false, err
	}
	if cfg.SoftDeleted {
		return project.Config{}, false, nil
	}
	return cfg, true, nil
}

func (p colocProjects) Update(ctx context.Context, cfg project.Config) (project.Config, error) {
	existing, ok, err := p.readRaw(ctx, cfg.Name)
	if err != nil {
		return project.Config{}, err
	}
	if !ok || existing.SoftDeleted {
		return project.Config{}, project.ErrNotFound
	}
	if existing.Revision != cfg.Revision {
		return project.Config{}, project.ErrRevisionMismatch
	}
	cfg.CreatedAt = existing.CreatedAt
	cfg.UpdatedAt = time.Now().UTC()
	cfg.SoftDeleted = false
	cfg.SecretURI = existing.SecretURI
	cfg.Revision = project.NewRevision()
	if err := p.s.putJSON(ctx, projectKey(cfg.Name), toStored(cfg)); err != nil {
		return project.Config{}, err
	}
	return cfg, nil
}

func (p colocProjects) SetSecret(ctx context.Context, name, secretURI string) error {
	cfg, ok, err := p.readRaw(ctx, name)
	if err != nil {
		return err
	}
	if !ok || cfg.SoftDeleted {
		return project.ErrNotFound
	}
	cfg.SecretURI = secretURI
	cfg.UpdatedAt = time.Now().UTC()
	return p.s.putJSON(ctx, projectKey(name), toStored(cfg))
}

func (p colocProjects) SoftDelete(ctx context.Context, name, revision string) error {
	cfg, ok, err := p.readRaw(ctx, name)
	if err != nil {
		return err
	}
	if !ok || cfg.SoftDeleted {
		return project.ErrNotFound
	}
	if cfg.Revision != revision {
		return project.ErrRevisionMismatch
	}
	cfg.SoftDeleted = true
	cfg.UpdatedAt = time.Now().UTC()
	return p.s.putJSON(ctx, projectKey(name), toStored(cfg))
}

func (p colocProjects) List(ctx context.Context) ([]project.Config, error) {
	rows, err := p.s.archive.List(ctx, metaPrefix+"projects/")
	if err != nil {
		return nil, err
	}
	out := make([]project.Config, 0, len(rows))
	for _, obj := range rows {
		var stored projectStored
		if ok, err := p.s.getJSON(ctx, obj.Path, &stored); err != nil || !ok {
			continue
		}
		if stored.SoftDeleted {
			continue
		}
		out = append(out, stored.toConfig())
	}
	return out, nil
}

func (p colocProjects) Close(_ context.Context) error { return nil }

func (p colocProjects) readRaw(ctx context.Context, name string) (project.Config, bool, error) {
	var stored projectStored
	ok, err := p.s.getJSON(ctx, projectKey(name), &stored)
	if err != nil || !ok {
		return project.Config{}, false, err
	}
	return stored.toConfig(), true, nil
}

type projectStored struct {
	Config      project.Config `json:"config"`
	Revision    string         `json:"revision"`
	SecretURI   string         `json:"secretUri,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	SoftDeleted bool           `json:"softDeleted,omitempty"`
}

func toStored(cfg project.Config) projectStored {
	return projectStored{
		Config: cfg, Revision: cfg.Revision, SecretURI: cfg.SecretURI,
		CreatedAt: cfg.CreatedAt, UpdatedAt: cfg.UpdatedAt, SoftDeleted: cfg.SoftDeleted,
	}
}

func (s projectStored) toConfig() project.Config {
	cfg := s.Config
	cfg.Revision = s.Revision
	cfg.SecretURI = s.SecretURI
	cfg.CreatedAt = s.CreatedAt
	cfg.UpdatedAt = s.UpdatedAt
	cfg.SoftDeleted = s.SoftDeleted
	return cfg
}
