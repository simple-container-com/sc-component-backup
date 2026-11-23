// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// go-embedb boot helper for sc-component-backup. Kept in its own file
// so the existing metadata-store selection path (the default,
// external Mongo / Postgres / MySQL / SQLite / colocated) stays
// readable without the ferretdb / embedded-listener machinery
// showing in main.go or config.go.
//
// Pattern mirrors forge-notifier/cmd/notifier/boot_goembedb.go, which
// in turn mirrors sc-component-observatory/service/cmd/observatory/
// boot_goembedb.go and go-embedb's own cmd/smoke-lambda/main.go
// bootEmbed(): one cold-start boot, a package-scoped FerretDB
// instance plus a connected `*mongo.Client`, every subsequent handler
// call reuses both. The Lambda-correctness knobs (AckWaitLiveOnly,
// large HeartbeatTTL, auto replicaId from AWS_LAMBDA_LOG_STREAM_NAME)
// are hard-wired on when AWS_LAMBDA_FUNCTION_NAME is set, so an
// operator who enables USE_GOEMBEDB on the backup Lambda does not
// have to remember go-embedb Bug 13.
//
// Blob carve-out: the archive store (`.tar.gz`, `.sql.gz`, etc.
// produced by the dump drivers) is NOT touched by this helper. It
// keeps running on S3 / GCS / filesystem via buildArchiveStore in
// config.go. Only the metadata rows (tenants, projects, backup +
// restore runs, snapshots + samples, alerts dismissals, notify
// settings) ride through the embedded Mongo wire protocol here.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/FerretDB/FerretDB/ferretdb"
)

// goEmbed is the booted state. Accessed through bootGoEmbed below;
// guards cold-start re-entry with a sync.Once, same shape smoke-lambda
// uses — a Lambda container may enter the handler many times, and
// re-opening the SQLite listener on each call would turn a sub-ms
// read into an expensive connect.
type goEmbed struct {
	FDB     *ferretdb.FerretDB
	Client  *mongo.Client
	URI     string
	ReplSet string
	// cancel stops the FerretDB.Run goroutine. Only reached from a
	// test / graceful-shutdown path; a Lambda container simply freezes.
	cancel context.CancelFunc
	// runDone is closed when FerretDB.Run returns, so a test can wait
	// for the listener to actually be down before tearing a temp dir.
	runDone chan struct{}
}

var (
	goEmbedOnce sync.Once
	goEmbedVal  *goEmbed
	goEmbedErr  error
)

// bootGoEmbed brings up go-embedb + the Mongo client once per process.
// Re-entry after a successful boot is a no-op; a failed boot is
// cached so every subsequent call fails fast rather than retrying a
// listener on an already-bound socket (which would print a cryptic
// "address already in use" on every request).
func bootGoEmbed(logger *slog.Logger) (*goEmbed, error) {
	goEmbedOnce.Do(func() {
		goEmbedVal, goEmbedErr = bootGoEmbedOnce(logger)
	})
	return goEmbedVal, goEmbedErr
}

func bootGoEmbedOnce(logger *slog.Logger) (*goEmbed, error) {
	dir := envOrDefault("GOEMBEDB_SQLITE_DIR", "/tmp/goembedb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("goembedb: mkdir sqlite dir %q: %w", dir, err)
	}
	replSet := envOrDefault("GOEMBEDB_REPLSET_NAME", "backup-rs")

	cfg := &ferretdb.Config{
		Logger:      logger.With("subsystem", "ferretdb"),
		Handler:     "sqlite",
		SQLiteURL:   "file:" + dir + "/",
		Listener:    ferretdb.ListenerConfig{TCP: "127.0.0.1:0"},
		ReplSetName: replSet,
	}

	// Replication substrate. OPTIONAL — the backup service has one
	// writer today (the single Lambda), so a single-node embed (no
	// GOEMBEDB_S3_BUCKET) is a valid "just put it in sqlite" deploy
	// for a smoke. The S3 active-active layer fires only when the
	// bucket is configured.
	if bucket := os.Getenv("GOEMBEDB_S3_BUCKET"); bucket != "" {
		region := envOrDefault("GOEMBEDB_S3_REGION", os.Getenv("AWS_REGION"))
		rep := &ferretdb.ReplicationStorageConfig{
			Backend:  "s3",
			S3Bucket: bucket,
			S3Prefix: envOrDefault("GOEMBEDB_S3_PREFIX", "sc-component-backup--staging/"),
			S3Region: region,
		}
		if maxReplicas := envIntOrZero("GOEMBEDB_MAX_REPLICAS"); maxReplicas > 0 {
			rep.MaxReplicas = maxReplicas
		}
		if ttl := envDurationOrZero("GOEMBEDB_HEARTBEAT_TTL"); ttl > 0 {
			rep.HeartbeatTTL = ttl
		}
		// Lambda-correctness defaults. Explicit env overrides win so
		// a long-lived deploy (ECS / VM) can turn them off, but the
		// common case — sc-component-backup IS a Lambda — gets
		// Bug 13 right without the operator remembering it.
		if isLambda() {
			if rep.HeartbeatTTL == 0 {
				rep.HeartbeatTTL = 3 * time.Minute
			}
			if rep.MaxReplicas == 0 {
				rep.MaxReplicas = 10
			}
			rep.AckWaitLiveOnly = true
		}
		// Explicit override still wins (useful for a non-Lambda
		// deploy that nonetheless wants live-only — e.g. a smoke).
		if envBool("GOEMBEDB_ACK_WAIT_LIVE_ONLY") {
			rep.AckWaitLiveOnly = true
		}
		cfg.ReplicationStorage = rep
		logger.Info("goembedb replication configured",
			"bucket", rep.S3Bucket, "prefix", rep.S3Prefix, "region", rep.S3Region,
			"ackWaitLiveOnly", rep.AckWaitLiveOnly, "heartbeatTTL", rep.HeartbeatTTL)
	} else {
		logger.Warn("GOEMBEDB_S3_BUCKET unset — running single-node, no replication")
	}

	fdb, err := ferretdb.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("goembedb: ferretdb.New: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if runErr := fdb.Run(ctx); runErr != nil && !errors.Is(runErr, context.Canceled) {
			logger.Error("ferretdb.Run exited", "err", runErr)
		}
	}()

	uri := fdb.MongoDBURI()
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	// SetDirect: the embed advertises a single-host rs which a
	// non-direct mongo-driver waits to elect a primary against;
	// direct connection skips SDAM and pins to the loopback socket.
	// Same reasoning as smoke-lambda/main.go.
	client, err := mongo.Connect(cctx, options.Client().ApplyURI(uri).SetDirect(true))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("goembedb: mongo.Connect: %w", err)
	}
	if err := client.Ping(cctx, readpref.Primary()); err != nil {
		cancel()
		return nil, fmt.Errorf("goembedb: mongo ping: %w", err)
	}

	logger.Info("goembedb booted",
		"uri", uri, "replSet", replSet, "sqliteDir", dir,
		"replicationEnabled", cfg.ReplicationStorage != nil)

	return &goEmbed{
		FDB:     fdb,
		Client:  client,
		URI:     uri,
		ReplSet: replSet,
		cancel:  cancel,
		runDone: runDone,
	}, nil
}

// metaURIForGoEmbed returns a Mongo URI that the existing meta.Store
// mongo backend wiring can `ApplyURI` cleanly: direct-connection is
// forced via a query parameter so the four per-store `mongo.Connect`
// calls (tenant / run / snapshot / project) all skip SDAM against
// the single-node embedded replica set.
//
// A query parameter is used (rather than modifying meta/mongo.go to
// thread an `options.ClientOptions`) specifically to keep the meta
// backends untouched — one knob (`METADATA_URI`) still selects the
// server, which keeps every alternate backend (postgres / mysql /
// sqlite / colocated) identical to pre-goembedb.
func metaURIForGoEmbed(baseURI string) string {
	if baseURI == "" {
		return baseURI
	}
	sep := "?"
	for _, c := range baseURI {
		if c == '?' {
			sep = "&"
			break
		}
	}
	return baseURI + sep + "directConnection=true"
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envIntOrZero(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func envDurationOrZero(name string) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}

func envBool(name string) bool {
	switch os.Getenv(name) {
	case "1", "true", "TRUE", "yes", "on":
		return true
	}
	return false
}

func isLambda() bool {
	return os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != ""
}
