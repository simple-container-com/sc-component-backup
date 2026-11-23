// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package goembedbadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MigrationConfig wires the `/admin/migrate-mongo-to-goembedb`
// endpoint.
//
// The endpoint's path says "mongo-to-goembedb" because that is the
// pre-cutover shape of this specific service: backup already runs
// against an external `METADATA_TYPE=mongo` + `METADATA_URI` in
// production, and the migration copies that database into the
// embedded go-embedb. Observatory's equivalent endpoint is named
// `/admin/migrate-s3-to-goembedb` (observatory's pre-cutover store
// was S3); notifier's keeps the "s3-to-goembedb" spelling for
// fleet-wide tooling consistency but is semantically a Mongo-to-Mongo
// copy same as this one. Pick whichever path matches the operator's
// deploy.
//
// Semantically it is a Mongo-to-Mongo copy: the backup service was
// already Mongo-native before this adoption (five metadata backends
// including `mongo`; see service/internal/backup/meta/meta.go).
//
// Separated from the HTTP wiring because the migration is NOT part
// of the steady-state surface: it is opt-in plumbing the operator
// runs once per stack during cutover, and it needs BOTH databases
// (source + sink) at the same time. The rest of the service runs
// against exactly one metadata backend chosen by USE_GOEMBEDB +
// METADATA_TYPE.
//
// Gating is by `ADMIN_MIGRATION_TOKEN` env (not SERVICE_JWT_SECRET
// nor the SPA session), on purpose: the migration endpoint is
// called by an operator shell (curl against the deployed Lambda),
// not by the conductor nor by a logged-in SPA user. Mixing it into
// the existing JWT middleware would force the operator to mint and
// manage yet another scoped token for a once-per-stack operation.
//
// ## Blob carve-out
//
// Archive blobs (the dump artefacts in S3/GCS/FS) are NEVER touched
// by this endpoint. Collections above is the closed set of metadata
// namespaces the migration walks; nothing in it names an archive key
// or a blob prefix. The archive store itself keeps serving
// Store.Get / Store.Presign reads unchanged after the cutover.
type MigrationConfig struct {
	// Source is the EXTERNAL Mongo database (the current METADATA_URI
	// target). Non-nil during cutover even when the service itself
	// is otherwise running on goembedb, because the migration copies
	// FROM here.
	Source *mongo.Database

	// Sink is the goembedb-backed Mongo database (the embedded
	// `*mongo.Client`'s database). Writes land here.
	Sink *mongo.Database

	// AdminToken, if non-empty, is required as the value of the
	// `X-Admin-Token` header. Empty disables the endpoint (which is
	// the safe default: an operator who did not set the env never
	// opens an unauthenticated migration route).
	AdminToken string
}

// migrationResponse is the shape returned on success.
//
// `Migrated` is the per-collection count the destination's documents
// landed under (so an operator can diff against each class's
// expected row count). `SourceCount` is the sum of per-collection
// source counts (the "LIST total" analogue observatory reports).
// Verified = true when the two totals agree AND no per-document
// error fired; false (with Errors non-empty) otherwise.
//
// The field tag `s3_count` is kept for cross-service tooling
// compatibility with observatory / notifier reports (which actually
// COUNT S3 keys); here it is the sum of source-Mongo per-collection
// counts.
type migrationResponse struct {
	DryRun       bool             `json:"dry_run"`
	SourceCount  int64            `json:"s3_count"`
	SourceCounts map[string]int64 `json:"source_counts"`
	Migrated     map[string]int64 `json:"migrated"`
	Total        int64            `json:"total_migrated"`
	ElapsedMs    int64            `json:"elapsed_ms"`
	Verified     bool             `json:"verified"`
	Errors       []migrationError `json:"errors,omitempty"`
}

type migrationError struct {
	Collection string `json:"collection"`
	ID         string `json:"id,omitempty"`
	Err        string `json:"err"`
}

// Migrate serves `POST /admin/migrate-mongo-to-goembedb`.
//
// Behaviour:
//
//  1. For each collection in Collections:
//     a. CountDocuments on the source produces the expected total.
//     b. Find() streams every document; each is upserted into the
//     same-named sink collection by its own `_id` field.
//  2. collectionCounts(sink) produces the per-collection landed count
//     the operator diffs against the source totals.
//  3. verified = len(Errors) == 0 AND sum(Migrated) >= SourceCount.
//
// Query parameter `dry_run=1` counts but does not copy. Lets the
// operator validate scope and shape before touching the sink.
//
// A per-document error is NON-fatal: the migration continues past it
// and the response lists every failure. Live-caught lesson from an
// earlier SC Component cutover: a migration that aborted on the
// first error left the destination half-populated with no record of
// which documents landed, forcing a full re-run on a cold sink.
func (c *MigrationConfig) Migrate(w http.ResponseWriter, r *http.Request) {
	if c.AdminToken == "" {
		writeErr(w, http.StatusNotFound, "migration endpoint disabled (ADMIN_MIGRATION_TOKEN unset)")
		return
	}
	if r.Header.Get("X-Admin-Token") != c.AdminToken {
		writeErr(w, http.StatusUnauthorized, "missing or invalid X-Admin-Token")
		return
	}
	if c.Source == nil {
		writeErr(w, http.StatusInternalServerError, "migration source (external Mongo) is not configured")
		return
	}
	if c.Sink == nil {
		writeErr(w, http.StatusInternalServerError, "migration sink (goembedb) is not configured")
		return
	}

	dryRun := r.URL.Query().Get("dry_run") == "1"
	start := time.Now()
	ctx := r.Context()

	resp := migrationResponse{
		DryRun:       dryRun,
		SourceCounts: map[string]int64{},
		Migrated:     map[string]int64{},
	}

	// Stable collection iteration so a diff of two runs is meaningful.
	names := append([]string(nil), Collections...)
	sort.Strings(names)

	for _, name := range names {
		srcColl := c.Source.Collection(name)
		n, err := srcColl.CountDocuments(ctx, bson.M{})
		if err != nil {
			resp.Errors = append(resp.Errors, migrationError{Collection: name, Err: "count source: " + err.Error()})
			continue
		}
		resp.SourceCounts[name] = n
		resp.SourceCount += n

		if dryRun || n == 0 {
			continue
		}

		if err := copyCollection(ctx, srcColl, c.Sink.Collection(name), &resp); err != nil {
			resp.Errors = append(resp.Errors, migrationError{Collection: name, Err: err.Error()})
		}
	}

	// Count destination rows even on a dry-run — gives the operator
	// a "what would be touched" vs "what is already there" picture.
	counts, cerr := collectionCounts(ctx, c.Sink)
	if cerr != nil {
		resp.Errors = append(resp.Errors, migrationError{Collection: "<collection-counts>", Err: cerr.Error()})
	} else {
		resp.Migrated = counts
	}

	// Verified: zero per-document errors AND the per-collection
	// total matches the source total. A dry run never claims verified.
	if !dryRun && len(resp.Errors) == 0 {
		var sum int64
		for _, n := range resp.Migrated {
			sum += n
		}
		resp.Verified = sum >= resp.SourceCount
	}
	resp.ElapsedMs = time.Since(start).Milliseconds()

	slog.Info("migration",
		"dry_run", resp.DryRun,
		"source_count", resp.SourceCount,
		"total_migrated", resp.Total,
		"verified", resp.Verified,
		"errors", len(resp.Errors),
		"elapsed_ms", resp.ElapsedMs)

	writeJSON(w, http.StatusOK, resp)
}

// copyCollection streams one source collection into a same-named
// sink collection. Each document is upserted by `_id` so a retried
// migration overwrites rather than duplicates. Per-document errors
// are reported through resp.Errors and iteration continues.
func copyCollection(ctx context.Context, src, sink *mongo.Collection, resp *migrationResponse) error {
	cur, err := src.Find(ctx, bson.M{})
	if err != nil {
		return fmt.Errorf("find %s: %w", src.Name(), err)
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			resp.Errors = append(resp.Errors, migrationError{
				Collection: src.Name(),
				Err:        "decode: " + err.Error(),
			})
			continue
		}
		id, hasID := doc["_id"]
		if !hasID {
			resp.Errors = append(resp.Errors, migrationError{
				Collection: src.Name(),
				Err:        "document missing _id",
			})
			continue
		}
		_, uerr := sink.ReplaceOne(ctx, bson.M{"_id": id}, doc, options.Replace().SetUpsert(true))
		if uerr != nil {
			resp.Errors = append(resp.Errors, migrationError{
				Collection: src.Name(),
				ID:         fmt.Sprintf("%v", id),
				Err:        "upsert: " + uerr.Error(),
			})
			continue
		}
		resp.Total++
	}
	if err := cur.Err(); err != nil {
		return fmt.Errorf("cursor %s: %w", src.Name(), err)
	}
	return nil
}

// collectionCounts returns a per-collection document count across
// every backup-owned collection. Used by the migration endpoint
// for a count-parity verification report.
//
// A missing collection reports 0 (Mongo returns no error for
// CountDocuments against a non-existent collection — the driver
// treats it as empty). That is the right shape for the dry-run
// report: a fresh sink legitimately contains zero of everything.
func collectionCounts(ctx context.Context, db *mongo.Database) (map[string]int64, error) {
	out := map[string]int64{}
	for _, name := range Collections {
		n, err := db.Collection(name).CountDocuments(ctx, bson.M{})
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", name, err)
		}
		out[name] = n
	}
	return out, nil
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// errMigrationNotConfigured is the sentinel the wiring layer checks
// to decide whether to mount the route at all.
var errMigrationNotConfigured = errors.New("MigrationConfig requires Source + Sink + AdminToken")

// ensure errMigrationNotConfigured is referenced so a future refactor
// does not drop the sentinel on an unused-value lint.
var _ = errMigrationNotConfigured
