// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// TestBootGoEmbedOnce_LambdaWithoutBucketHardFails pins the invariant that
// a Lambda deploy with GOEMBEDB_S3_BUCKET unset refuses to boot rather
// than warning and falling back to /tmp/goembedb. Lambda's /tmp is per-
// container and dies with every container recycle; silent fallback means
// each warm container reads an empty sink on cold start, concludes a
// backup it already ran never happened, and re-runs it. Permanent and
// recurring, not a one-time empty-store window.
//
// Inversion to confirm the test can fail: unset AWS_LAMBDA_FUNCTION_NAME
// (the sibling test TestBootGoEmbedOnce_NonLambdaWithoutBucketWarns);
// bootGoEmbedOnce must NOT return the GOEMBEDB_S3_BUCKET-named error in
// that mode. Without the inversion, this test would also pass against
// the pre-fix code that unconditionally warned.
func TestBootGoEmbedOnce_LambdaWithoutBucketHardFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOEMBEDB_SQLITE_DIR", tmp)
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "backup-staging-test")
	t.Setenv("GOEMBEDB_S3_BUCKET", "")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, err := bootGoEmbedOnce(logger)
	if err == nil {
		t.Fatal("bootGoEmbedOnce must refuse to boot on Lambda with no bucket; got nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "GOEMBEDB_S3_BUCKET") {
		t.Errorf("error must name GOEMBEDB_S3_BUCKET so the operator can find the fix; got: %s", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "lambda") {
		t.Errorf("error must name Lambda so the Lambda-specific reason is visible; got: %s", msg)
	}
}

// TestBootGoEmbedOnce_NonLambdaWithoutBucketWarns inverts the assertion:
// outside Lambda (ECS / VM / local smoke) a single-node sqlite embed is
// a valid "just put it in sqlite" deploy, so no-bucket must proceed with
// a warning rather than return the Lambda error.
//
// Without this inversion, a pre-fix implementation that unconditionally
// returned the GOEMBEDB_S3_BUCKET error would pass the Lambda-hard-fail
// test and silently break every non-Lambda deploy.
//
// We only assert that bootGoEmbedOnce does NOT return the specific
// GOEMBEDB_S3_BUCKET + Lambda error; later-stage I/O errors (sqlite
// path, ferretdb Listen, etc.) are fine — they fail for unrelated
// reasons and are not this test's subject.
func TestBootGoEmbedOnce_NonLambdaWithoutBucketWarns(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOEMBEDB_SQLITE_DIR", tmp)
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "")
	t.Setenv("GOEMBEDB_S3_BUCKET", "")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, err := bootGoEmbedOnce(logger)
	if err != nil && strings.Contains(err.Error(), "GOEMBEDB_S3_BUCKET") &&
		strings.Contains(strings.ToLower(err.Error()), "lambda") {
		t.Errorf("non-Lambda no-bucket must NOT return the Lambda error; got: %s", err)
	}
}
