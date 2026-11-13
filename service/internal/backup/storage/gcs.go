// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// GCSStore is the Google Cloud Storage archive backend. Skeleton only
// in slice 1e.4a — the full impl lands in slice 1e.4f (adds the
// `cloud.google.com/go/storage` dep + V4 signed URL support +
// Workload-Identity auth + IfGenerationMatch conditional writes).
//
// Keeping the type declared now lets `meta.NewFromConfig` accept
// `storage.type: gcs` at parse time and fail with a clear
// not-implemented error rather than an unknown-type one.

// ErrGCSNotImplemented is returned by every GCSStore method until
// slice 1e.4f.
var ErrGCSNotImplemented = errors.New("storage: GCS backend lands in slice 1e.4f of sc-components-platform")

// GCSConfig bundles the knobs. CredentialsFile is a path to a GCP
// service-account JSON; empty uses Application Default Credentials
// (Workload Identity on GKE / GCE / Cloud Run).
type GCSConfig struct {
	Bucket          string
	CredentialsFile string
}

// GCSStore implements Store. Zero-value fields today; populated in
// slice 1e.4f.
type GCSStore struct {
	cfg GCSConfig
}

// NewGCSStore returns an UNUSABLE GCSStore today — every method
// returns ErrGCSNotImplemented. The config-parse path still succeeds
// so an operator's server.yaml referencing `storage.type: gcs` is a
// valid but not-yet-functional configuration, flagged at boot.
func NewGCSStore(_ context.Context, cfg GCSConfig) (*GCSStore, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("storage.NewGCSStore: Bucket is required")
	}
	return &GCSStore{cfg: cfg}, nil
}

func (s *GCSStore) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, ErrGCSNotImplemented
}

func (s *GCSStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, ErrGCSNotImplemented
}

func (s *GCSStore) List(context.Context, string) ([]StoredObject, error) {
	return nil, ErrGCSNotImplemented
}

func (s *GCSStore) Delete(context.Context, string) error {
	return ErrGCSNotImplemented
}

func (s *GCSStore) Presign(context.Context, string, time.Duration) (string, error) {
	return "", ErrGCSNotImplemented
}
