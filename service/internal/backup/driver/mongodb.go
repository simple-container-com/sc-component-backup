// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

// MongoDBDriver shells `mongodump` / `mongorestore` with --archive
// (streaming, single-file output) + --gzip (compressed on the wire).
// Restore uses --drop so a partial-restore-then-retry converges to
// the archive's exact state rather than piling rows on top.
type MongoDBDriver struct{}

func (MongoDBDriver) Name() string { return "mongodb" }

// Dump streams a mongodump archive to out. The caller is responsible
// for closing out (if it's an s3 upload writer etc.). URI-authentication
// is passed as a command-line arg — mongodump does not support
// reading the URI from stdin. Operators who want to avoid URI-in-argv
// visibility should mint a connection file via `mongodump --config
// <file>`; not in v1.
func (m *MongoDBDriver) Dump(ctx context.Context, conn ConnectionSpec, out io.Writer) error {
	uri := conn.URI
	if uri == "" {
		return fmt.Errorf("mongodb driver: ConnectionSpec.URI is empty; cannot dump")
	}
	if err := requireCmd("mongodump"); err != nil {
		return err
	}
	// Scope the dump to the specified database when the tenant declares
	// one. Without --db mongodump enumerates every DB on the server
	// and tries to read admin.system.roles — that requires clusterAdmin,
	// which tenant-scoped users (readWriteAnyDatabase or lower) don't
	// have. A tenant that wants to back up the whole cluster explicitly
	// leaves conn.Database blank AND provisions a cluster-admin creds.
	args := []string{"--uri=" + uri, "--archive", "--gzip", "--quiet"}
	if db := conn.Database; db != "" {
		args = append(args, "--db="+db)
	}
	cmd := exec.CommandContext(ctx, "mongodump", args...)
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mongodump failed: %w; stderr tail: %s", err, tailStderr(stderr.Bytes()))
	}
	return nil
}

// Restore streams a mongorestore from in. The archive it reads must be
// the shape mongodump --archive produced — bson, gzipped.
func (m *MongoDBDriver) Restore(ctx context.Context, conn ConnectionSpec, in io.Reader) error {
	uri := conn.URI
	if uri == "" {
		return fmt.Errorf("mongodb driver: ConnectionSpec.URI is empty; cannot restore")
	}
	if err := requireCmd("mongorestore"); err != nil {
		return err
	}
	// Scope restore to --db + --nsFrom/--nsTo when the tenant declares
	// one; see Dump above for why.
	rargs := []string{"--uri=" + uri, "--archive", "--gzip", "--drop", "--quiet"}
	if db := conn.Database; db != "" {
		rargs = append(rargs, "--nsInclude="+db+".*")
	}
	cmd := exec.CommandContext(ctx, "mongorestore", rargs...)
	var stderr bytes.Buffer
	cmd.Stdin = in
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mongorestore failed: %w; stderr tail: %s", err, tailStderr(stderr.Bytes()))
	}
	return nil
}
