// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

// Worker mode — the one-shot execution shape used when the Backup
// Component is deployed to aws-lambda. Design: the sc-component-backup
// refactor doc (external to this public repo; see the sc-components-platform
// milestone's §5.3 for the full rationale).
//
// Lambda's 15-minute cap makes it unsuitable for the actual mongodump of
// a non-trivial DB. The deployed Lambda stays short: tenant store +
// API + 1-minute scheduler tick. Each tick fires an ECS Fargate
// one-shot task running THIS SAME IMAGE with BACKUP_MODE=worker. The
// worker reads its tenant from env (the Lambda injected it when it
// called ecs.RunTask), dumps, uploads to S3, exits 0.
//
// Same image, two entry modes — one binary, one Dockerhub repo.
//
// For non-lambda deploys (ecs-fargate / vm-compose / k8s), the service
// runs long-lived and dumps in-process — worker mode is unused.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/driver"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
)

// runWorker is the one-shot entrypoint. Returns the exit code the
// process should propagate (0 success, non-zero failure).
//
// Reads env (injected by the Lambda's ecs.RunTask call per-tenant):
//   - BACKUP_DRIVER                 mongodb | postgres | mysql
//   - BACKUP_TENANT_URI             connection URI (user+pass+host+options)
//   - BACKUP_TENANT_DATABASE        target DB name
//   - BACKUP_TENANT_HOST            (optional; URI-only is enough for v1)
//   - BACKUP_TENANT_USER / PASSWORD (optional; URI-only is enough for v1)
//   - BACKUP_TENANT_TLS_CA_PEM      (optional)
//   - BACKUP_S3_BUCKET              target S3 bucket
//   - BACKUP_S3_REGION              target S3 region
//   - BACKUP_S3_KEY                 full S3 object key (consumer-stack / db / ts.archive.gz)
//   - BACKUP_S3_ENDPOINT_URL        (optional; MinIO)
//   - BACKUP_S3_ACCESS_KEY / _SECRET_KEY (optional; Fargate task role is
//     preferred, but explicit creds work for smoke + non-AWS object stores)
//
// Tenant metadata env keys live under BACKUP_TENANT_* so they can't be
// confused with the service-mode MONGO_URI / S3_BUCKET env the Lambda
// itself uses for its tenant store.
func runWorker(ctx context.Context) int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	driverName := mustEnv("BACKUP_DRIVER")
	tenantURI := mustEnv("BACKUP_TENANT_URI")
	tenantDB := os.Getenv("BACKUP_TENANT_DATABASE") // optional: driver may scope on its own
	s3Bucket := mustEnv("BACKUP_S3_BUCKET")
	s3Region := envOr("BACKUP_S3_REGION", "eu-central-1")
	s3Key := mustEnv("BACKUP_S3_KEY")

	slog.Info(
		"backup worker starting",
		"driver", driverName,
		"database", tenantDB,
		"s3_bucket", s3Bucket,
		"s3_key", s3Key,
		"tenant_uri_masked", maskMongoURI(tenantURI),
	)

	// Build the driver. requireCmd inside Dump will fail cleanly if the
	// CLI isn't on PATH — image build pins all three (mongodump /
	// pg_dump / mariadb-dump) via the Dockerfile verify step.
	d, err := driver.ByName(driverName)
	if err != nil {
		slog.Error("unknown driver", "driver", driverName, "err", err)
		return 2
	}

	// S3 store with the same shape service-mode uses. Explicit creds
	// win if provided; otherwise the SDK falls back to the Fargate
	// task role.
	s3, err := storage.NewS3Store(ctx, storage.S3Config{
		Bucket:       s3Bucket,
		Region:       s3Region,
		EndpointURL:  envOr("BACKUP_S3_ENDPOINT_URL", ""),
		AccessKey:    envOr("BACKUP_S3_ACCESS_KEY", ""),
		SecretKey:    envOr("BACKUP_S3_SECRET_KEY", ""),
		UsePathStyle: envOr("BACKUP_S3_USE_PATH_STYLE", "") != "",
	})
	if err != nil {
		slog.Error("s3 store init failed", "err", err)
		return 1
	}

	// Pipe mongodump → S3 PUT. The driver writes to the pipe writer; the
	// S3 uploader consumes the pipe reader. No intermediate file —
	// matters for a 100GB DB on a Fargate task with fixed ephemeral
	// storage. If the driver fails mid-stream, closing the writer with
	// an error propagates to the uploader which aborts the multi-part
	// upload cleanly.
	pr, pw := io.Pipe()
	go func() {
		start := time.Now()
		slog.Info("dump starting")
		err := d.Dump(ctx, driver.ConnectionSpec{
			URI:      tenantURI,
			Database: tenantDB,
			TLSCAPem: os.Getenv("BACKUP_TENANT_TLS_CA_PEM"),
		}, pw)
		slog.Info("dump finished", "elapsed", time.Since(start).String(), "err", errString(err))
		// Propagate any dump error through the pipe so the uploader sees
		// it on Read + aborts the multi-part upload cleanly.
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
	}()

	start := time.Now()
	slog.Info("upload starting", "key", s3Key)
	n, err := s3.Put(ctx, s3Key, pr)
	if err != nil {
		slog.Error("upload failed", "err", err, "elapsed", time.Since(start).String())
		return 1
	}
	slog.Info("upload finished", "elapsed", time.Since(start).String(), "key", s3Key, "bytes", n)
	return 0
}

func mustEnv(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		slog.Error("required worker env missing", "name", name)
		os.Exit(2)
	}
	return v
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%v", err)
}
