// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// redact replaces every non-empty secret substring in text with a
// generic placeholder. Secrets are tried longest-first so a password
// that is a substring of the full URI is substituted before the URI
// itself, leaving the URI's host/path legible in the redacted output.
//
// Ported from local-db-backup/db_backup/engines/base.py:redact — same
// shape, same placeholders.
func redact(text string, secrets []string) string {
	cleaned := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			cleaned = append(cleaned, s)
		}
	}
	sort.Slice(cleaned, func(i, j int) bool { return len(cleaned[i]) > len(cleaned[j]) })
	for _, s := range cleaned {
		placeholder := "<password>"
		if strings.Contains(s, "://") {
			placeholder = "<uri>"
		}
		text = strings.ReplaceAll(text, s, placeholder)
	}
	return text
}

// toolVersion runs `<cmd> <args...>` (typically `cmd --version`) with
// a short deadline and extracts the first N.N or N.N.N triple from
// combined stdout + stderr. Empty string means the tool wasn't found
// or didn't print a parseable version — the caller surfaces that as
// a TestResult warning rather than an error, matching the Python ref.
func toolVersion(ctx context.Context, cmd string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	c.Env = cleanEnv()
	out, _ := c.CombinedOutput()
	m := versionRE.FindStringSubmatch(string(out))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

var versionRE = regexp.MustCompile(`(\d+\.\d+(?:\.\d+)?)`)

// cleanEnv returns a copy of the current environment with variables
// that the DB CLIs would silently consume stripped out — a PGHOST or
// MONGODB_URI inherited from the host shouldn't quietly redirect a
// shelled-out dump away from the configured target.
//
// The scrub prefixes match local-db-backup/db_backup/engines/base.py.
func cleanEnv(extra ...string) []string {
	scrub := []string{"PG", "MYSQL", "MARIADB", "MONGO"}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		name := kv[:eq]
		upper := strings.ToUpper(name)
		skip := false
		for _, p := range scrub {
			if strings.HasPrefix(upper, p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// privateDir creates a 0700 temp directory and returns a cleanup
// func the caller must defer. Any file written inside gets whatever
// mode the writer chose, but the directory itself is not readable by
// other processes — belt-and-braces against a 0644-default umask.
func privateDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "sc-backup-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create private tempdir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, fmt.Errorf("chmod 0700 %s: %w", dir, err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// writePrivateFile writes content to <dir>/<name> with mode 0600,
// refusing to overwrite an existing file. Used for the mongodump
// `--config` yaml that holds the connection URI — keeping credentials
// out of argv/env.
func writePrivateFile(dir, name, content string) (string, error) {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// secretsFromConn builds a redaction list from a ConnectionSpec: URI,
// password, user — whichever are set. Order doesn't matter (redact
// sorts internally); duplicates are fine.
func secretsFromConn(conn ConnectionSpec) []string {
	out := make([]string, 0, 3)
	if conn.URI != "" {
		out = append(out, conn.URI)
	}
	if conn.Password != "" {
		out = append(out, conn.Password)
	}
	return out
}
