// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package driver — DB-flavoured adapters that can `Dump` to a stream
// and `Restore` from one. One Driver per `connection.driver` the
// canonical credential-output spec defines (mongodb / postgres /
// mysql / redis — redis stubbed in v1).
//
// Implementations shell out to the native CLI (mongodump, pg_dump,
// mysqldump) rather than reimplementing the wire protocol — each tool
// knows its own back-compat matrix better than we do, and operators
// can diagnose failures against familiar error messages.
package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ConnectionSpec is the DB-plugin-agnostic credential bundle the
// canonical `connection.*` output family carries. The service builds
// this from a Resource's Connection map at tenant-register time.
type ConnectionSpec struct {
	URI      string
	Host     string
	Port     int
	User     string
	Password string
	Database string
	// TLSCAPem is optional — mongodb atlas exposes a cluster CA bundle,
	// AWS RDS region-bundles, CloudSQL via ServerCaCert. Empty when the
	// consumer isn't configured for TLS verify.
	TLSCAPem string
}

// Driver streams a full logical dump of a DB to an io.Writer and
// restores from an io.Reader. Both calls honour the context for
// cancellation — a SIGTERM during an in-flight dump kills the shelled
// subprocess cleanly.
type Driver interface {
	Name() string
	Dump(ctx context.Context, conn ConnectionSpec, out io.Writer) error
	Restore(ctx context.Context, conn ConnectionSpec, in io.Reader) error
}

// ErrDriverUnsupported is returned by stub drivers that declare a
// name but have no implementation yet (postgres + mysql in v1).
var ErrDriverUnsupported = errors.New("driver not yet implemented")

// ByName returns the registered driver for a `connection.driver`
// string, or an error if no driver matches. Drivers are intentionally
// constructed lazily — a service deployed to a Lambda image that
// carries all three CLIs still works on a dev workstation that only
// has one of them, since ByName doesn't verify CLI availability (the
// Dump/Restore calls do).
func ByName(name string) (Driver, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "mongodb", "mongo":
		return &MongoDBDriver{}, nil
	case "postgres", "postgresql":
		return &PostgresDriver{}, nil
	case "mysql":
		return &MySQLDriver{}, nil
	default:
		return nil, fmt.Errorf("no backup driver registered for %q", name)
	}
}

// commandNotFoundError wraps a missing-CLI error into something an
// HTTP handler can surface clearly to the operator (so the SPA shows
// "mongodump is not installed in the Backup service image" rather
// than a bare `exec: "mongodump": executable file not found`).
type commandNotFoundError struct {
	Cmd string
}

func (e *commandNotFoundError) Error() string {
	return fmt.Sprintf("%s CLI not found in PATH; a Backup service image must carry it", e.Cmd)
}

// requireCmd checks that `name` resolves in PATH. Returns a typed
// not-found error for operator clarity on dev workstations.
func requireCmd(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return &commandNotFoundError{Cmd: name}
	}
	return nil
}

// tailStderr returns the last 256 bytes of a child process's stderr,
// used to attach a diagnosable tail to a dump/restore error without
// bloating the DB-logged BackupRun.Error field.
func tailStderr(stderr []byte) string {
	const max = 256
	s := strings.TrimSpace(string(stderr))
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max:]
}

var _ = io.EOF // keep `io` referenced even if subpackages are stubs
