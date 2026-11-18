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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
// confused with the service-mode STORAGE_* / METADATA_* env the Lambda
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
		postCompleteCallback(ctx, workerCallbackPayload{Status: "failed", Error: "unknown driver: " + driverName})
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
		postCompleteCallback(ctx, workerCallbackPayload{Status: "failed", Error: "s3 init: " + err.Error()})
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
		postCompleteCallback(ctx, workerCallbackPayload{
			Status: "failed",
			Error:  "upload: " + err.Error(),
		})
		return 1
	}
	slog.Info("upload finished", "elapsed", time.Since(start).String(), "key", s3Key, "bytes", n)
	// Callback to the Lambda — flips the BackupRun row from `running` to
	// `succeeded` with the final byte count + S3 key. Env comes from the
	// RunTask overrides in ecs_dispatch.go.
	postCompleteCallback(ctx, workerCallbackPayload{
		Status: "ok",
		Bytes:  n,
		S3Path: s3Key,
	})
	return 0
}

// workerCallbackPayload mirrors api.InternalRunCompleteRequest. Kept a
// local type because the worker binary intentionally does not import
// the service's HTTP handlers (same package would make the worker
// link in every handler's deps).
type workerCallbackPayload struct {
	Status string `json:"status"`
	Bytes  int64  `json:"bytes,omitempty"`
	S3Path string `json:"s3_path,omitempty"`
	Error  string `json:"error,omitempty"`
}

// postCompleteCallback POSTs the run outcome back to the Lambda. Env
// required: BACKUP_AGENT_URL, BACKUP_AGENT_TOKEN, BACKUP_RUN_ID. When
// any is missing (legacy dispatch path that didn't inject them, or a
// test harness) the worker logs + skips — the stuck-run watchdog
// reclassifies the row at next Lambda startup as the fallback.
//
// Best-effort with a short timeout + log-only error: a Fargate task
// still exited 0 if the dump + upload succeeded; a failed callback
// means the SPA shows the row as `running` until the watchdog runs,
// NOT a corrupted archive.
func postCompleteCallback(ctx context.Context, body workerCallbackPayload) {
	agentURL := strings.TrimRight(os.Getenv("BACKUP_AGENT_URL"), "/")
	token := os.Getenv("BACKUP_AGENT_TOKEN")
	runID := os.Getenv("BACKUP_RUN_ID")
	if agentURL == "" || token == "" || runID == "" {
		slog.Info(
			"worker callback skipped — missing env",
			"agent_url_set", agentURL != "",
			"token_set", token != "",
			"run_id_set", runID != "",
		)
		return
	}
	payload, err := json.Marshal(body)
	if err != nil {
		slog.Error("worker callback marshal failed", "err", err)
		return
	}
	cbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		cbCtx, http.MethodPost,
		agentURL+"/api/v1/internal/runs/"+runID+"/complete",
		bytes.NewReader(payload),
	)
	if err != nil {
		slog.Error("worker callback build failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("worker callback POST failed", "err", err, "url", agentURL)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		slog.Error("worker callback non-2xx", "status", resp.StatusCode, "body", string(respBody))
		return
	}
	slog.Info("worker callback ok", "status", resp.StatusCode, "run_id", runID, "payload_status", body.Status)
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
