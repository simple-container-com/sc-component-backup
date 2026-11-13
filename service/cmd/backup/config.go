// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/meta"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
)

// Slice 1e.4g — env-var-driven construction of the archive + metadata
// stores. The plugin's Provision (slice 1e.4g.2 — follow-up) will set
// the new BACKUP_STORAGE_* / BACKUP_METADATA_* vars from the operator's
// server.yaml `storage:` / `metadata:` blocks. For a release window,
// legacy S3_* + MONGO_URI env names still work with a deprecation
// warning; slice 1e.4h removes them.

// buildArchiveStore picks the archive backend from env + returns it
// along with a short human-readable location string for the SPA's
// Storage block (e.g. "s3://bucket/" or "file:///var/sc-backup").
//
// Env var resolution order per field:
//
//  1. STORAGE_<FIELD>              — slice 1e.4g.2 generic shape
//     (sc-plugin-aws's applyComponentRawToSingleImage translates the
//     operator's server.yaml `storage:` block onto env with this
//     convention: storage.type → STORAGE_TYPE, storage.accessKey →
//     STORAGE_ACCESS_KEY, etc.)
//  2. BACKUP_STORAGE_<FIELD>       — slice 1e.4g env-first pre-plugin-dispatch
//  3. S3_<FIELD>                   — pre-1e.4 legacy
//
// All three fall back with a deprecation warning when the newer name
// is unset. Slice 1e.4h removes the two compat tiers.
func buildArchiveStore(ctx context.Context) (storage.Store, string, error) {
	t := strings.ToLower(firstEnv("STORAGE_TYPE", "BACKUP_STORAGE_TYPE"))
	// Legacy fallback: if neither new nor BACKUP_-prefixed TYPE is set
	// but any pre-1e.4 S3_* env is set, infer s3 + log a deprecation.
	if t == "" {
		if hasAnyLegacyS3Env() {
			slog.Warn("legacy S3_* env vars detected — migrate to storage: block in server.yaml (sc-plugin-aws ≥ v2026.11.14 translates to STORAGE_* env; removed in slice 1e.4h)")
			t = "s3"
		} else {
			t = "s3" // default stays s3 so a smoke without any env still boots
		}
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
	bucket := firstEnv("STORAGE_BUCKET", "BACKUP_STORAGE_BUCKET", "S3_BUCKET")
	if bucket == "" {
		bucket = "sc-backup-smoke" // last-resort default for smokes
	}
	cfg := storage.S3Config{
		Bucket:       bucket,
		Region:       firstEnvOr("us-east-1", "STORAGE_REGION", "BACKUP_STORAGE_REGION", "S3_REGION"),
		EndpointURL:  firstEnv("STORAGE_ENDPOINT", "BACKUP_STORAGE_ENDPOINT", "S3_ENDPOINT_URL"),
		AccessKey:    firstEnv("STORAGE_ACCESS_KEY", "BACKUP_STORAGE_ACCESS_KEY", "S3_ACCESS_KEY"),
		SecretKey:    firstEnv("STORAGE_SECRET_KEY", "BACKUP_STORAGE_SECRET_KEY", "S3_SECRET_KEY"),
		UsePathStyle: firstEnv("STORAGE_USE_PATH_STYLE", "BACKUP_STORAGE_USE_PATH_STYLE", "S3_USE_PATH_STYLE") != "",
	}
	s3Store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("s3 archive init: %w", err)
	}
	if firstEnv("STORAGE_ENSURE_BUCKET", "BACKUP_STORAGE_ENSURE_BUCKET", "S3_ENSURE_BUCKET") != "" {
		if err := s3Store.EnsureBucket(ctx); err != nil {
			return nil, "", fmt.Errorf("s3 ensure-bucket: %w", err)
		}
	}
	return s3Store, fmt.Sprintf("s3://%s/", cfg.Bucket), nil
}

func buildGCSArchive(ctx context.Context) (storage.Store, string, error) {
	bucket := firstEnv("STORAGE_BUCKET", "BACKUP_STORAGE_BUCKET")
	if bucket == "" {
		return nil, "", fmt.Errorf("STORAGE_BUCKET is required when STORAGE_TYPE=gcs")
	}
	cfg := storage.GCSConfig{
		Bucket:          bucket,
		CredentialsFile: firstEnv("STORAGE_CREDENTIALS_FILE", "BACKUP_STORAGE_CREDENTIALS_FILE"),
	}
	g, err := storage.NewGCSStore(ctx, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("gcs archive init: %w", err)
	}
	return g, fmt.Sprintf("gs://%s/", cfg.Bucket), nil
}

func buildFSArchive(ctx context.Context) (storage.Store, string, error) {
	path := firstEnv("STORAGE_PATH", "BACKUP_STORAGE_PATH")
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

// buildMetaStore picks the metadata backend from env. Resolution
// order per field (slice 1e.4g.2):
//
//  1. METADATA_<FIELD>              — generic (sc-plugin-aws translates
//     server.yaml `metadata:` block to env)
//  2. BACKUP_METADATA_<FIELD>       — slice 1e.4g env-first pre-plugin-dispatch
//  3. MONGO_URI                     — pre-1e.4 legacy (implies metadata.type: mongo)
//
// Falls through to meta.NewFromConfig's default when Type is empty
// (default depends on the archive type — FS → sqlite, object store → colocated).
func buildMetaStore(ctx context.Context, archive storage.Store) (meta.Store, error) {
	cfg := meta.Config{
		Type:       meta.Type(strings.ToLower(firstEnv("METADATA_TYPE", "BACKUP_METADATA_TYPE"))),
		URI:        firstEnv("METADATA_URI", "BACKUP_METADATA_URI"),
		SQLitePath: firstEnv("METADATA_SQLITE_PATH", "BACKUP_METADATA_SQLITE_PATH"),
	}
	// Legacy MONGO_URI: operators on the fleet deployment haven't
	// migrated yet.
	if cfg.Type == meta.TypeDefault {
		if legacy := os.Getenv("MONGO_URI"); legacy != "" {
			slog.Warn("legacy MONGO_URI env detected — migrate to metadata: block in server.yaml (sc-plugin-aws ≥ v2026.11.14 translates to METADATA_* env; removed in slice 1e.4h)")
			cfg.Type = meta.TypeMongo
			cfg.URI = legacy
		}
	}
	return meta.NewFromConfig(ctx, cfg, archive)
}

// firstEnv returns the first non-empty value among the named envs.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// firstEnvOr returns the first non-empty value, or fallback.
func firstEnvOr(fallback string, names ...string) string {
	if v := firstEnv(names...); v != "" {
		return v
	}
	return fallback
}

// hasAnyLegacyS3Env reports whether any of the pre-1e.4g S3_* env
// vars are set — legacy-detection lets the service infer s3 without
// requiring the operator to set BACKUP_STORAGE_TYPE=s3 during the
// migration window.
func hasAnyLegacyS3Env() bool {
	for _, n := range []string{
		"S3_BUCKET", "S3_REGION", "S3_ENDPOINT_URL",
		"S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_USE_PATH_STYLE",
		"S3_ENSURE_BUCKET",
	} {
		if os.Getenv(n) != "" {
			return true
		}
	}
	return false
}
