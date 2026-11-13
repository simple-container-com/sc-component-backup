// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/meta"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
)

// Slice 1e.4g — env-var-driven construction of the archive + metadata
// stores. Slice 1e.4h (this file) removed the BACKUP_STORAGE_* / S3_* /
// BACKUP_METADATA_* / MONGO_URI compat tiers; the service reads ONLY
// the STORAGE_* / METADATA_* names, which sc-plugin-aws's
// applyComponentRawToSingleImage sets from the operator's server.yaml
// `storage:` / `metadata:` blocks. Operators who still have the legacy
// env names baked into their smokes must migrate before upgrading past
// v2026.11.14.

// buildArchiveStore picks the archive backend from env + returns it
// along with a short human-readable location string for the SPA's
// Storage block (e.g. "s3://bucket/" or "file:///var/sc-backup").
//
// Env schema:
//
//	STORAGE_TYPE              s3 | gcs | filesystem (default: s3)
//	STORAGE_BUCKET            bucket/container name (required for s3 + gcs)
//	STORAGE_REGION            cloud region (s3)
//	STORAGE_ENDPOINT          S3 endpoint override (MinIO, LocalStack)
//	STORAGE_ACCESS_KEY        explicit creds (otherwise SDK default chain)
//	STORAGE_SECRET_KEY        -"-
//	STORAGE_USE_PATH_STYLE    non-empty → path-style addressing (MinIO)
//	STORAGE_ENSURE_BUCKET     non-empty → create bucket at boot
//	STORAGE_PATH              root directory (filesystem)
//	STORAGE_CREDENTIALS_FILE  GCP service-account key path (gcs)
//
// sc-plugin-aws ≥ v2026.11.14 translates the server.yaml `storage:`
// block onto these names: `storage.type` → STORAGE_TYPE,
// `storage.accessKey` → STORAGE_ACCESS_KEY, etc.
func buildArchiveStore(ctx context.Context) (storage.Store, string, error) {
	t := strings.ToLower(os.Getenv("STORAGE_TYPE"))
	if t == "" {
		t = "s3" // default stays s3 so a smoke without any env still boots
	}
	switch t {
	case "s3":
		return buildS3Archive(ctx)
	case "filesystem":
		return buildFSArchive(ctx)
	case "gcs":
		return buildGCSArchive(ctx)
	default:
		return nil, "", fmt.Errorf("STORAGE_TYPE=%q not recognised (one of s3|gcs|filesystem)", t)
	}
}

func buildS3Archive(ctx context.Context) (storage.Store, string, error) {
	bucket := os.Getenv("STORAGE_BUCKET")
	if bucket == "" {
		bucket = "sc-backup-smoke" // last-resort default for smokes
	}
	cfg := storage.S3Config{
		Bucket:       bucket,
		Region:       envOr("STORAGE_REGION", "us-east-1"),
		EndpointURL:  os.Getenv("STORAGE_ENDPOINT"),
		AccessKey:    os.Getenv("STORAGE_ACCESS_KEY"),
		SecretKey:    os.Getenv("STORAGE_SECRET_KEY"),
		UsePathStyle: os.Getenv("STORAGE_USE_PATH_STYLE") != "",
	}
	s3Store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("s3 archive init: %w", err)
	}
	if os.Getenv("STORAGE_ENSURE_BUCKET") != "" {
		if err := s3Store.EnsureBucket(ctx); err != nil {
			return nil, "", fmt.Errorf("s3 ensure-bucket: %w", err)
		}
	}
	return s3Store, fmt.Sprintf("s3://%s/", cfg.Bucket), nil
}

func buildGCSArchive(ctx context.Context) (storage.Store, string, error) {
	bucket := os.Getenv("STORAGE_BUCKET")
	if bucket == "" {
		return nil, "", fmt.Errorf("STORAGE_BUCKET is required when STORAGE_TYPE=gcs")
	}
	cfg := storage.GCSConfig{
		Bucket:          bucket,
		CredentialsFile: os.Getenv("STORAGE_CREDENTIALS_FILE"),
	}
	g, err := storage.NewGCSStore(ctx, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("gcs archive init: %w", err)
	}
	return g, fmt.Sprintf("gs://%s/", cfg.Bucket), nil
}

func buildFSArchive(ctx context.Context) (storage.Store, string, error) {
	path := os.Getenv("STORAGE_PATH")
	if path == "" {
		return nil, "", fmt.Errorf("STORAGE_PATH is required when STORAGE_TYPE=filesystem")
	}
	// Use SERVICE_JWT_SECRET as the FSStore's Presign HMAC key so a
	// multi-replica deploy can verify each other's tokens. FSStore
	// rolls a random key when unset — fine for single-replica.
	fsCfg := storage.FSConfig{
		Root:           path,
		PresignHMACKey: []byte(os.Getenv("SERVICE_JWT_SECRET")),
	}
	fs, err := storage.NewFSStore(ctx, fsCfg)
	if err != nil {
		return nil, "", fmt.Errorf("filesystem archive init: %w", err)
	}
	return fs, fmt.Sprintf("file://%s", fs.Root()), nil
}

// buildMetaStore picks the metadata backend from env.
//
// Env schema:
//
//	METADATA_TYPE              mongo | postgres | mysql | sqlite | colocated
//	                           (empty → meta.NewFromConfig default: fs → sqlite,
//	                           object store → colocated)
//	METADATA_URI               connection URI (mongo/postgres/mysql)
//	METADATA_SQLITE_PATH       sqlite file path (sqlite)
//
// sc-plugin-aws ≥ v2026.11.14 translates the server.yaml `metadata:`
// block onto these names.
func buildMetaStore(ctx context.Context, archive storage.Store) (meta.Store, error) {
	cfg := meta.Config{
		Type:       meta.Type(strings.ToLower(os.Getenv("METADATA_TYPE"))),
		URI:        os.Getenv("METADATA_URI"),
		SQLitePath: os.Getenv("METADATA_SQLITE_PATH"),
	}
	return meta.NewFromConfig(ctx, cfg, archive)
}
