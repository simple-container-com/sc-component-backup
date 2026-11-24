// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// The ARN-exit calibration, end to end: boot with a SecretsManager ARN
// the service CANNOT reach (bogus account + non-existent secret), and
// assert the binary exits 2 rather than serving.
//
// This is a CALIBRATION test, aimed at the 2026-11-18 production
// failure mode. On that date, `BACKUP_OPERATOR_PASSWORD` resolved into
// the Lambda env as an `arn:aws:secretsmanager:…` string and was read
// by nothing — the service compared operator logins against the raw
// 115-char ARN and every attempt returned "wrong password." The fix
// added `resolveAWSSecretARN` + `os.Exit(2)` on resolve failure.
//
// The absence a handler test cannot see: a service that silently
// serves when a secret fails to resolve. Every handler test boots with
// non-ARN literals (via NewSessionConfigFromEnv or inline construction),
// so the resolve path is never exercised end-to-end. A mutation that
// deletes `os.Exit(2)` on resolve failure is caught here and ONLY
// here — the `mutation-check` harness depends on this test.
//
// Why a bogus-ARN input, not a mocked SDK: the SDK's own behaviour on
// "I can't reach AWS and have no credentials" is the exact shape the
// Lambda hit live when the operator supplied a mistyped ARN. The test
// asserts that failure stays loud (exit 2) rather than falling through.
//
// Notes carried forward from sibling repos:
//   - `GOTOOLCHAIN=go1.27.3` (or auto) required — service/go.mod
//     declares 1.27.3 and local default is 1.26.11; the resulting
//     wave of INVALID reads as entry breakage, not environment.
//   - `AWS_EC2_METADATA_DISABLED=true` kills the IMDSv2 timeout path
//     so the SDK fails on no-credentials in ~hundreds of ms, not the
//     full 15s resolve-ctx timeout. Keeps the test under 5s locally.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A bogus ARN: syntactically valid, routed to a non-existent account +
// secret so the SDK call cannot succeed by accident in any environment.
const bogusARN = "arn:aws:secretsmanager:eu-west-1:000000000000:secret:nope-AAAAAA"

// Each test names the specific slog message the resolve-error path
// emits, and asserts (a) exit code 2 AND (b) that message appears in
// stderr AND (c) the fallthrough-guard message does NOT appear. The
// third assertion is load-bearing: without it, the test cannot
// distinguish the resolve-error Exit(2) from a SECONDARY guard's
// Exit(2) that fires on the broken value when the first is bypassed
// (live-caught 2026-11-24 writing this test: removing the resolve
// Exit(2) from SERVICE_JWT_SECRET's block leaves `jwtSecret = ""`,
// which falls into the `len(jwtSecret) < 32` guard four lines down —
// also an Exit(2), different slog message, same test outcome. The
// bare exit-code assertion would have been vacuous).

func TestBinaryExitsTwoOnServiceJWTSecretArnResolveFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary; skipped under -short")
	}
	binPath := buildBinary(t)
	assertExitsTwoOnResolveFailure(
		t, binPath, map[string]string{
			"SERVICE_JWT_SECRET": bogusARN,
			// BACKUP_OPERATOR_PASSWORD left non-ARN so this test is strictly
			// about the SERVICE_JWT_SECRET resolve path.
			"BACKUP_OPERATOR_PASSWORD": "placeholder-not-an-arn",
		},
		"failed to resolve SERVICE_JWT_SECRET",
		// The secondary guard four lines later. If the resolve-error
		// Exit(2) is bypassed, jwtSecret becomes "" and the len-check
		// also exits 2 — the bare exit-code assertion cannot tell the
		// two apart. The absence-assertion closes that gap.
		"SERVICE_JWT_SECRET must be at least 32 bytes",
	)
}

func TestBinaryExitsTwoOnBackupOperatorPasswordArnResolveFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary; skipped under -short")
	}
	binPath := buildBinary(t)
	assertExitsTwoOnResolveFailure(
		t, binPath, map[string]string{
			"SERVICE_JWT_SECRET":       "0123456789abcdef0123456789abcdef",
			"BACKUP_OPERATOR_PASSWORD": bogusARN,
		},
		"failed to resolve BACKUP_OPERATOR_PASSWORD",
		// There is no SECONDARY guard on BACKUP_OPERATOR_PASSWORD after
		// its resolve block — bypassing the Exit(2) leaves the empty
		// resolved value in the env and the service proceeds to boot.
		// The anti-marker is therefore the "starting sc-component-backup"
		// slog line, which only prints after all three resolve blocks
		// succeed. Its presence under a bogus ARN proves the loud-exit
		// failed and the service fell through.
		"starting sc-component-backup",
	)
}

// buildBinary compiles cmd/backup into a temp dir. Shared between the
// two tests because the only thing that varies is the env.
func buildBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "backup")
	cmd := exec.Command("go", "build",
		"-buildvcs=false",
		"-o", binPath, ".")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binPath
}

// assertExitsTwoOnResolveFailure runs the binary with `extraEnv` on top
// of the minimum self-contained boot env (filesystem storage in a temp
// dir, plain HTTP, no IMDSv2 chase), waits up to 30s for exit, and
// asserts THREE things: exit code 2, the resolve-error slog `wantMsg`
// is present in stderr, and the fallthrough-guard slog `antiMsg` is
// absent.
//
// The anti-marker is load-bearing: a resolve-error Exit(2) that gets
// bypassed often falls into a secondary guard's Exit(2) with a
// different slog message, and the bare exit-code assertion cannot
// tell the two apart. Asserting the anti-marker's absence is what
// makes the test specific to the resolve-error path.
func assertExitsTwoOnResolveFailure(t *testing.T, binPath string, extraEnv map[string]string, wantMsg, antiMsg string) {
	t.Helper()
	storeDir := t.TempDir()
	var out bytes.Buffer
	proc := exec.Command(binPath)
	env := append(
		os.Environ(),
		"PORT=0",
		"STORAGE_TYPE=filesystem",
		"STORAGE_PATH="+storeDir,
		"METADATA_TYPE=colocated",
		"BACKUP_COOKIE_SECURE=false",
		"USE_GOEMBEDB=0",
		"LOCAL_DEBUG=true",
		// Kill the EC2 metadata discovery path so the SDK fails on
		// no-credentials in ms, not the 15s resolve-ctx timeout.
		"AWS_EC2_METADATA_DISABLED=true",
		// No AWS region hints — nothing should succeed by accident on
		// an inherited AWS_REGION.
		"AWS_REGION=",
		"AWS_DEFAULT_REGION=",
	)
	for k, v := range extraEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	proc.Env = env
	proc.Stdout = &out
	proc.Stderr = &out
	if err := proc.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()

	var exitCode int
	select {
	case err := <-done:
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("binary exited without an ExitError: %v (want exit code 2)\nstderr:\n%s", err, out.String())
		}
		exitCode = exit.ExitCode()
	case <-time.After(30 * time.Second):
		_ = proc.Process.Kill()
		t.Fatalf("binary did not exit within 30s on bogus ARN — the resolve-failure path is swallowing the error OR the resolver's context budget exceeded 30s\nstderr:\n%s", out.String())
	}

	stderr := out.String()
	if exitCode != 2 {
		t.Errorf("binary exit code = %d, want 2 — the resolve-failure path either didn't fire or was renamed\nstderr:\n%s", exitCode, stderr)
	}
	if !strings.Contains(stderr, wantMsg) {
		t.Errorf("stderr does not contain %q — the resolve-error slog message was renamed or the resolve path was bypassed before it could log\nstderr:\n%s", wantMsg, stderr)
	}
	if strings.Contains(stderr, antiMsg) {
		t.Errorf("stderr contains %q — a SECONDARY guard fired, meaning the resolve-error Exit(2) was BYPASSED and the broken value fell through to the next check. The bare exit-code assertion would have been vacuous.\nstderr:\n%s", antiMsg, stderr)
	}
}
