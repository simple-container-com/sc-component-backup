// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// mongoSystemDBs are the server-owned databases a user-scoped
// `--listDatabases` should hide. Matches local-db-backup
// db_backup/config.py:SYSTEM_DBS["mongodb"].
var mongoSystemDBs = map[string]struct{}{
	"admin":  {},
	"local":  {},
	"config": {},
}

// MongoDBDriver shells `mongodump` / `mongorestore` with --archive=-
// (stream to stdout/stdin) + --gzip (compressed on the wire). Restore
// uses --drop + --nsInclude so a partial-restore-then-retry converges
// to the archive's exact state rather than piling rows on top.
//
// The URI is passed to the CLI via a 0600 `--config` yaml rather
// than argv, so `ps` / `/proc/*/cmdline` can't see it. Ported from
// local-db-backup/db_backup/engines/mongodb.py.
type MongoDBDriver struct{}

func (MongoDBDriver) Name() string { return "mongodb" }

// RestoreCommand is the human-readable one-liner the SPA renders on
// the archive's restore tab. `{cat}` is a placeholder the frontend
// swaps for the concrete download step (aws s3 cp / curl / etc.).
func (MongoDBDriver) RestoreCommand() string {
	return `{cat} | mongorestore --uri "<target>" --archive --gzip`
}

// Dump streams a mongodump archive to out. The caller is responsible
// for closing out (if it's an S3 upload writer etc.). The URI goes
// through a 0600 `--config` file so credentials stay out of argv.
func (m *MongoDBDriver) Dump(ctx context.Context, conn ConnectionSpec, out io.Writer) error {
	if conn.URI == "" {
		return fmt.Errorf("mongodb driver: ConnectionSpec.URI is empty; cannot dump")
	}
	if err := requireCmd("mongodump"); err != nil {
		return err
	}
	dir, cleanup, err := privateDir()
	if err != nil {
		return err
	}
	defer cleanup()
	cfgPath, err := writePrivateFile(dir, "mongodump.yaml", fmt.Sprintf("uri: %q\n", conn.URI))
	if err != nil {
		return err
	}
	// --archive=- streams to stdout; --gzip compresses on the wire.
	// Scope to a database when the tenant declares one (see note in
	// the pre-port stub about tenant-scoped users lacking clusterAdmin).
	args := []string{
		"--config=" + cfgPath,
		"--archive=-",
		"--gzip",
		"--quiet",
	}
	if conn.Database != "" {
		args = append(args, "--db="+conn.Database)
	}
	cmd := exec.CommandContext(ctx, "mongodump", args...)
	cmd.Env = cleanEnv()
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mongodump failed: %w; stderr tail: %s", err,
			redact(tailStderr(stderr.Bytes()), secretsFromConn(conn)))
	}
	return nil
}

// Restore streams a mongorestore from in. The archive must be the
// shape mongodump --archive produced — bson, gzipped.
func (m *MongoDBDriver) Restore(ctx context.Context, conn ConnectionSpec, in io.Reader) error {
	if conn.URI == "" {
		return fmt.Errorf("mongodb driver: ConnectionSpec.URI is empty; cannot restore")
	}
	if err := requireCmd("mongorestore"); err != nil {
		return err
	}
	dir, cleanup, err := privateDir()
	if err != nil {
		return err
	}
	defer cleanup()
	cfgPath, err := writePrivateFile(dir, "mongorestore.yaml", fmt.Sprintf("uri: %q\n", conn.URI))
	if err != nil {
		return err
	}
	args := []string{
		"--config=" + cfgPath,
		"--archive=-",
		"--gzip",
		"--drop",
		"--quiet",
	}
	if conn.Database != "" {
		args = append(args, "--nsInclude="+conn.Database+".*")
	}
	cmd := exec.CommandContext(ctx, "mongorestore", args...)
	cmd.Env = cleanEnv()
	var stderr bytes.Buffer
	cmd.Stdin = in
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mongorestore failed: %w; stderr tail: %s", err,
			redact(tailStderr(stderr.Bytes()), secretsFromConn(conn)))
	}
	return nil
}

// ListDatabases returns the user-visible databases on the server.
// System DBs (admin/local/config) are filtered out so the SPA shows
// only what the operator cares about.
func (m *MongoDBDriver) ListDatabases(ctx context.Context, conn ConnectionSpec) ([]string, error) {
	cli, err := mongoConnect(ctx, conn, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Disconnect(context.Background()) }()
	names, err := cli.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("list databases: %s",
			redact(err.Error(), secretsFromConn(conn)))
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, sys := mongoSystemDBs[n]; sys {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// Inventory enumerates the collections of `db` with per-collection
// size breakdown (docs count, uncompressed bytes, storage bytes,
// index bytes), sorted largest-first. Uses `collStats` per collection
// with a per-command context — a single stuck collStats can't block
// the whole call.
func (m *MongoDBDriver) Inventory(ctx context.Context, conn ConnectionSpec, db string) ([]CollectionStat, error) {
	if db == "" {
		return nil, fmt.Errorf("mongodb driver: Inventory requires a database name")
	}
	cli, err := mongoConnect(ctx, conn, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Disconnect(context.Background()) }()

	d := cli.Database(db)
	// Filter to type:collection so views + timeseries-metaN are not
	// listed as top-level collections; the dump wouldn't include them
	// as their own archives either.
	names, err := d.ListCollectionNames(ctx, bson.M{"type": "collection"})
	if err != nil {
		return nil, fmt.Errorf("list collection names: %s",
			redact(err.Error(), secretsFromConn(conn)))
	}
	out := make([]CollectionStat, 0, len(names))
	for _, name := range names {
		if strings.HasPrefix(name, "system.") {
			continue
		}
		statsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var st struct {
			Count          int64 `bson:"count"`
			Size           int64 `bson:"size"`
			StorageSize    int64 `bson:"storageSize"`
			TotalIndexSize int64 `bson:"totalIndexSize"`
		}
		err := d.RunCommand(statsCtx, bson.D{{Key: "collStats", Value: name}}).Decode(&st)
		cancel()
		if err != nil {
			// Skip collections we can't read stats for — a missing stat
			// should not fail the whole inventory.
			continue
		}
		out = append(out, CollectionStat{
			Name:         name,
			Kind:         "collection",
			Docs:         st.Count,
			Bytes:        st.Size,
			StorageBytes: st.StorageSize,
			IndexBytes:   st.TotalIndexSize,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Test probes connectivity + collects version info + warns about
// common misconfigurations (no `readPreference` on the URI, missing
// mongodump binary in the image).
func (m *MongoDBDriver) Test(ctx context.Context, conn ConnectionSpec) (TestResult, error) {
	res := TestResult{
		ClientVersion: toolVersion(ctx, "mongodump", "--version"),
	}
	cli, err := mongoConnect(ctx, conn, 15*time.Second)
	if err != nil {
		res.Error = redact(err.Error(), secretsFromConn(conn))
		if res.ClientVersion == "" {
			res.Warnings = append(res.Warnings, "mongodump not found in the container")
		}
		return res, nil
	}
	defer func() { _ = cli.Disconnect(context.Background()) }()

	// Server version via buildInfo on admin.
	var info struct {
		Version string `bson:"version"`
	}
	if err := cli.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err == nil {
		res.ServerVersion = info.Version
	}

	names, lerr := cli.ListDatabaseNames(ctx, bson.D{})
	if lerr == nil {
		dbs := make([]string, 0, len(names))
		for _, n := range names {
			if _, sys := mongoSystemDBs[n]; sys {
				continue
			}
			dbs = append(dbs, n)
		}
		sort.Strings(dbs)
		res.Databases = dbs
	} else {
		res.Error = redact(lerr.Error(), secretsFromConn(conn))
	}

	// readPreference warning — matches Python reference. A dump against
	// the primary load-affects live traffic on that node.
	rp := strings.ToLower(conn.URI)
	if !strings.Contains(rp, "readpreference=secondary") && !strings.Contains(rp, "readpreference=secondarypreferred") {
		res.Warnings = append(res.Warnings,
			"add readPreference=secondary to the connection string so dumps do not load the primary")
	}
	if res.ClientVersion == "" {
		res.Warnings = append(res.Warnings, "mongodump not found in the container")
	}
	res.OK = res.Error == ""
	return res, nil
}

// mongoConnect dials a MongoClient with a shared timeout applied to
// server-selection, connect and socket timeouts. The caller must
// Disconnect when done.
func mongoConnect(ctx context.Context, conn ConnectionSpec, timeout time.Duration) (*mongo.Client, error) {
	if conn.URI == "" {
		return nil, fmt.Errorf("mongodb driver: ConnectionSpec.URI is empty")
	}
	opts := options.Client().ApplyURI(conn.URI).
		SetAppName("sc-component-backup").
		SetServerSelectionTimeout(timeout).
		SetConnectTimeout(timeout).
		SetSocketTimeout(timeout).
		SetReadPreference(readpref.SecondaryPreferred())
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cli, err := mongo.Connect(dialCtx, opts)
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	// Ping to catch bad credentials / network early rather than deferring
	// to the first operation.
	if err := cli.Ping(dialCtx, readpref.Nearest()); err != nil {
		_ = cli.Disconnect(context.Background())
		return nil, fmt.Errorf("mongo ping: %w", err)
	}
	return cli, nil
}
