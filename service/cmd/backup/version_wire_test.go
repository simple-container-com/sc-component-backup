// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// The build stamp, end to end: ldflags -> main.version -> the JSON on
// both /healthz (what a smoke reads with no credential) and on
// /api/v1/session (what the SPA header renders).
//
// Why a binary test and not a handler test. The handler tests in
// internal/api build their own SessionConfig, so every one of them
// passes with main.go never setting Version at all. That is the exact
// shape of the bug this fleet has shipped twice — a value that is
// present, correct and READ BY NOTHING, logging identically to a
// working deploy (`signingKey` as a bare ARN on backup-smoke-1d
// 2026-11-14, then BACKUP_OPERATOR_PASSWORD on the first
// backup.simple-forge.com login 2026-11-18). The only honest
// assertion is to ask the real binary what build it is.
//
// Mirrors sc-component-notifier#6 verbatim and sc-component-observatory
// #30. Both surfaces are asserted because they are wired SEPARATELY in
// main.go (api.Healthz(version) and sessionCfg.Version = version) and
// the /healthz one is the one a smoke reaches with no credentials.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A value no default could produce: `version` falls back to "dev" and a
// real build stamps CalVer, so neither can pass this by accident.
const wantVersion = "v2026.11.24-bkupwiretest"

func TestBinaryReportsItsBuildOnHealthzAndSession(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary; skipped under -short")
	}

	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "backup")

	// Build the real binary with the sentinel version via ldflags.
	// `-buildvcs=false` is a go build flag (not a linker flag), kept
	// outside the -ldflags argument.
	cmd := exec.Command("go", "build",
		"-buildvcs=false",
		"-ldflags", "-X main.version="+wantVersion,
		"-o", binPath, ".")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Pick a free port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	storeDir := t.TempDir()
	proc := exec.Command(binPath)
	proc.Env = append(
		os.Environ(),
		"PORT="+fmt.Sprintf("%d", port),
		"SERVICE_JWT_SECRET=0123456789abcdef0123456789abcdef",
		"BACKUP_OPERATOR_PASSWORD=wiretest-password",
		"STORAGE_TYPE=filesystem",
		"STORAGE_PATH="+storeDir,
		"METADATA_TYPE=colocated",
		"BACKUP_COOKIE_SECURE=false",
		"USE_GOEMBEDB=0",
		"LOCAL_DEBUG=true",
	)
	proc.Stdout = os.Stderr
	proc.Stderr = os.Stderr
	if err := proc.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	t.Cleanup(func() { _ = proc.Process.Kill(); _, _ = proc.Process.Wait() })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Poll /healthz until the binary is up.
	deadline := time.Now().Add(20 * time.Second)
	var healthzBody []byte
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil && resp.StatusCode == 200 {
			ct := resp.Header.Get("Content-Type")
			buf, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(ct, "json") {
				t.Fatalf("/healthz returned content-type %q (not JSON) — plain-ok regression", ct)
			}
			healthzBody = buf
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(250 * time.Millisecond)
	}
	if healthzBody == nil {
		t.Fatal("binary did not start serving /healthz within 20s")
	}

	// Surface 1: /healthz carries the build.
	var hz map[string]any
	if err := json.Unmarshal(healthzBody, &hz); err != nil {
		t.Fatalf("/healthz body is not JSON: %v (body=%q)", err, healthzBody)
	}
	if got := hz["version"]; got != wantVersion {
		t.Errorf("/healthz version = %v, want %q — ldflag landed on nothing or main.version never wired into api.Healthz(version)", got, wantVersion)
	}
	if got := hz["status"]; got != "ok" {
		t.Errorf("/healthz status = %v, want %q — probe renamed its field", got, "ok")
	}

	// Surface 2: /api/v1/session carries the build (ungated).
	resp, err := http.Get(base + "/api/v1/session")
	if err != nil {
		t.Fatalf("get /api/v1/session: %v", err)
	}
	defer resp.Body.Close()
	var sess map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("/api/v1/session body: %v", err)
	}
	if got := sess["version"]; got != wantVersion {
		t.Errorf("/api/v1/session version = %v, want %q — SessionConfig.Version never wired from main.go", got, wantVersion)
	}
}
