// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package goembedbadmin

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/meta"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
)

// ColocatedMigrationConfig wires the `/admin/migrate-colocated-to-goembedb`
// endpoint. Sibling to MigrationConfig above — same auth gate, same sink
// shape — but the SOURCE is the colocated S3 metadata keyspace the backup
// service already runs against in production (`metadata: colocated`)
// rather than an external Mongo.
//
// Separating the two shapes keeps the operator path honest: a staging
// deployment with `metadata: colocated` would be ORPHANED by the
// mongo-source endpoint (walks an empty external DB, verifies against
// zero → "0 >= 0" is true → every tenant disappears on the next
// scheduler tick). This endpoint walks the S3 _meta/ keyspace via the
// SAME live `meta.colocatedStore` accessors the service uses, so a
// drift between the writer's key layout and the migration's reader is
// unreachable by construction.
//
// Serialization choice (LIVE-CAUGHT 2026-11-24): the sink is read back
// by `mongo-driver` into the same Go structs the service uses at
// runtime (`tenant.Record`, `project.Config`, `backup.BackupRun`,
// `backup.RestoreRun`, scheduler's private `snapshotDoc`), so the
// migration writes those structs directly through the driver — the
// driver uses their **bson** tags. The tempting alternative
// (`json.Marshal → UnmarshalExtJSON → bson.M`) produces documents
// keyed by json tags; wherever bson and json disagree (and they do on
// `startedAt`/`started_at` style fields, and object-sample layout
// differs entirely) the service's reader would silently decode zero
// values — a tenant with an empty schedule reads to the scheduler
// exactly like one that was never backed up.
//
// The service-side read-back test (`TestColocatedMigration_ReadBackThroughService`)
// opens a `meta.Store` with `METADATA_TYPE=mongo` pointed at the sink
// and compares field-by-field against the colocated source. A rename
// of one bson tag on a fixture struct breaks the read-back —
// `TestColocatedMigration_BSONTagAgreement_Inversion` is that proof.
type ColocatedMigrationConfig struct {
	// Source is the live colocated metadata store (built by
	// `meta.NewFromConfig(ctx, {Type: TypeColocated}, archive)` on the
	// paired archive Store). Non-nil during cutover; nil disables
	// the endpoint with a 500 that names the gap.
	Source meta.Store

	// Sink is the goembedb-backed Mongo database (the embedded
	// `*mongo.Client`'s database). Writes land here, same shape as
	// the mongo-source endpoint.
	Sink *mongo.Database

	// AdminToken, if non-empty, is required as the value of the
	// `X-Admin-Token` header. Empty disables the endpoint entirely
	// (same safe default as the mongo-source endpoint).
	AdminToken string
}

// colocatedMigrationResponse is the shape returned on success.
//
// `Rows` carries one entry per collection in Collections so an
// operator diffing against the closed set sees which classes moved,
// which were skipped (source="none" with a reason), and which were
// empty. SourceEmpty + Verified lead the body so a shell can `jq`
// them without descending into Rows.
//
// Status code semantics (deliberate, 2026-11-24): a wet run that
// finds ZERO across every colocated-owned class answers 422 so a
// runbook `curl -f` catches it without parsing JSON. A dry run on
// the same source answers 200 — counting is the point of a dry run.
type colocatedMigrationResponse struct {
	DryRun        bool                    `json:"dry_run"`
	SourceEmpty   bool                    `json:"source_empty"`
	SourceCount   int64                   `json:"source_count"`
	MigratedCount int64                   `json:"migrated_count"`
	Verified      bool                    `json:"verified"`
	ElapsedMs     int64                   `json:"elapsed_ms"`
	Rows          []colocatedMigrationRow `json:"rows"`
	Errors        []migrationError        `json:"errors,omitempty"`
}

// colocatedMigrationRow is the per-collection breakdown operators read
// to decide "is this gap intentional?" `Source` is one of
// "colocated" (walked from S3 and copied), "none" (no colocated
// accessor — stored elsewhere by the service), or "skipped" (list
// error for the class, preserved in Errors).
type colocatedMigrationRow struct {
	Collection    string `json:"collection"`
	Source        string `json:"source"`
	SourceCount   int64  `json:"source_count"`
	MigratedCount int64  `json:"migrated_count"`
	Reason        string `json:"reason,omitempty"`
}

// Migrate serves `POST /admin/migrate-colocated-to-goembedb`.
//
// Behaviour:
//
//  1. For each of the four colocated-owned classes
//     (tenants, projects, backup_runs, restore_runs, db_snapshots)
//     the Source's accessor lists live rows; each is bson-upserted
//     into the same-named sink collection by its reader-side key
//     (stackName for tenants, _id for the other four).
//  2. For settings, alerts_dismissals, object_samples — classes with
//     NO colocated accessor — a row lands in the response with
//     source="none" and a reason. The operator sees the gap without
//     being told everything is zero.
//  3. `SourceEmpty=true` when every walked class returned zero rows.
//     On a wet run that answers 422 and the sink is untouched; on a
//     dry run that answers 200.
//  4. `Verified=true` ONLY when SourceCount > 0 AND every migrated
//     row was accounted for AND no per-document error fired. An
//     empty source cannot verify, by construction — that is the
//     specific failure mode this endpoint exists to prevent.
//
// Query parameter `dry_run=1` lists but does not copy.
func (c *ColocatedMigrationConfig) Migrate(w http.ResponseWriter, r *http.Request) {
	if c.AdminToken == "" {
		writeErr(w, http.StatusNotFound, "migration endpoint disabled (ADMIN_MIGRATION_TOKEN unset)")
		return
	}
	got := r.Header.Get("X-Admin-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(c.AdminToken)) != 1 {
		writeErr(w, http.StatusUnauthorized, "missing or invalid X-Admin-Token")
		return
	}
	if c.Source == nil {
		writeErr(w, http.StatusInternalServerError, "migration source (colocated meta.Store) is not configured")
		return
	}
	if c.Sink == nil {
		writeErr(w, http.StatusInternalServerError, "migration sink (goembedb) is not configured")
		return
	}

	dryRun := r.URL.Query().Get("dry_run") == "1"
	start := time.Now()
	ctx := r.Context()

	resp := colocatedMigrationResponse{DryRun: dryRun}

	// Walk the four colocated-owned classes. Each step appends its
	// row to resp.Rows and tallies SourceCount / MigratedCount.
	c.migrateTenants(ctx, dryRun, &resp)
	projs := c.migrateProjects(ctx, dryRun, &resp)
	c.migrateBackupRuns(ctx, dryRun, &resp)
	c.migrateRestoreRuns(ctx, dryRun, &resp)
	c.migrateSnapshots(ctx, projs, dryRun, &resp)

	// Explicit gaps: these three classes have NO colocated accessor
	// at all — settings is in notify.MongoStore, alerts_dismissals in
	// alerts.MongoStore, object_samples has no list-all accessor on
	// the colocated SnapshotStore. An operator diffing against
	// Collections can see the gap, not be told everything is zero.
	resp.Rows = append(
		resp.Rows,
		colocatedMigrationRow{
			Collection: CollectionSettings,
			Source:     "none",
			Reason:     "no colocated accessor — stored elsewhere (notify.MongoStore); re-create via SPA after cutover",
		},
		colocatedMigrationRow{
			Collection: CollectionAlertsDismissals,
			Source:     "none",
			Reason:     "no colocated accessor — stored elsewhere (alerts.MongoStore); dismissals will reset after cutover",
		},
		colocatedMigrationRow{
			Collection: CollectionObjectSamples,
			Source:     "none",
			Reason:     "no list-all accessor on colocated SnapshotStore; samples regenerate on the next scheduler tick",
		},
	)

	// Stable row order for diff-friendly responses.
	sort.Slice(resp.Rows, func(i, j int) bool {
		return resp.Rows[i].Collection < resp.Rows[j].Collection
	})

	// source_empty: zero across every class that was walked.
	resp.SourceEmpty = resp.SourceCount == 0

	// verified: at least one source row AND every row accounted for
	// AND no per-document errors. `SourceCount > 0` is the specific
	// guard the `verified: true on 0 >= 0` trap requires.
	if !dryRun && len(resp.Errors) == 0 {
		resp.Verified = resp.SourceCount > 0 && resp.MigratedCount >= resp.SourceCount
	}
	resp.ElapsedMs = time.Since(start).Milliseconds()

	slog.Info("colocated migration",
		"dry_run", resp.DryRun,
		"source_empty", resp.SourceEmpty,
		"source_count", resp.SourceCount,
		"migrated_count", resp.MigratedCount,
		"verified", resp.Verified,
		"errors", len(resp.Errors),
		"elapsed_ms", resp.ElapsedMs)

	// A wet run that found zero across every class is refused with
	// 422 so an operator's `curl -f` catches it. A dry run reports the
	// same shape at 200 — counting an empty source is a valid dry run.
	status := http.StatusOK
	if resp.SourceEmpty && !dryRun {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, resp)
}

// migrateTenants walks the colocated tenants/ prefix and upserts each
// row by stackName. The Mongo-side reader uses `stackName` as the
// match key (tenant.Record carries no `_id` field — the collection's
// unique index is on stackName), so a filter on `_id` would create
// duplicates on re-run. See `service/internal/tenant/store.go#MongoStore.Upsert`.
func (c *ColocatedMigrationConfig) migrateTenants(ctx context.Context, dryRun bool, resp *colocatedMigrationResponse) {
	rows, err := c.Source.Tenants().List(ctx)
	if err != nil {
		resp.Errors = append(resp.Errors, migrationError{Collection: CollectionTenants, Err: "list: " + err.Error()})
		resp.Rows = append(resp.Rows, colocatedMigrationRow{Collection: CollectionTenants, Source: "colocated", Reason: "list failed"})
		return
	}
	row := colocatedMigrationRow{Collection: CollectionTenants, Source: "colocated", SourceCount: int64(len(rows))}
	resp.SourceCount += row.SourceCount
	if !dryRun {
		coll := c.Sink.Collection(CollectionTenants)
		for _, rec := range rows {
			filter := bson.M{"stackName": rec.StackName}
			opts := options.Replace().SetUpsert(true)
			if _, err := coll.ReplaceOne(ctx, filter, rec, opts); err != nil {
				resp.Errors = append(resp.Errors, migrationError{Collection: CollectionTenants, ID: rec.StackName, Err: "upsert: " + err.Error()})
				continue
			}
			row.MigratedCount++
			resp.MigratedCount++
		}
	}
	resp.Rows = append(resp.Rows, row)
}

// migrateProjects walks the colocated projects/ prefix and upserts
// each row by `_id` (project.Config's bson:"_id" is Name). Returns
// the project names so the snapshot walker has a seed — the colocated
// SnapshotStore has no list-all accessor and its only reader is
// scoped by project.
func (c *ColocatedMigrationConfig) migrateProjects(ctx context.Context, dryRun bool, resp *colocatedMigrationResponse) []project.Config {
	rows, err := c.Source.Projects().List(ctx)
	if err != nil {
		resp.Errors = append(resp.Errors, migrationError{Collection: CollectionProjects, Err: "list: " + err.Error()})
		resp.Rows = append(resp.Rows, colocatedMigrationRow{Collection: CollectionProjects, Source: "colocated", Reason: "list failed"})
		return nil
	}
	row := colocatedMigrationRow{Collection: CollectionProjects, Source: "colocated", SourceCount: int64(len(rows))}
	resp.SourceCount += row.SourceCount
	if !dryRun {
		coll := c.Sink.Collection(CollectionProjects)
		for _, cfg := range rows {
			filter := bson.M{"_id": cfg.Name}
			opts := options.Replace().SetUpsert(true)
			if _, err := coll.ReplaceOne(ctx, filter, cfg, opts); err != nil {
				resp.Errors = append(resp.Errors, migrationError{Collection: CollectionProjects, ID: cfg.Name, Err: "upsert: " + err.Error()})
				continue
			}
			row.MigratedCount++
			resp.MigratedCount++
		}
	}
	resp.Rows = append(resp.Rows, row)
	return rows
}

// migrateBackupRuns walks every tenant's runs/ prefix via
// ListAllBackups and upserts each row by `_id` (BackupRun.ID). The
// limit=0 argument means "no cap"; the migration is a one-off and
// operator memory limits have never been the binding constraint here.
func (c *ColocatedMigrationConfig) migrateBackupRuns(ctx context.Context, dryRun bool, resp *colocatedMigrationResponse) {
	rows, err := c.Source.Runs().ListAllBackups(ctx, 0)
	if err != nil {
		resp.Errors = append(resp.Errors, migrationError{Collection: CollectionBackupRuns, Err: "list: " + err.Error()})
		resp.Rows = append(resp.Rows, colocatedMigrationRow{Collection: CollectionBackupRuns, Source: "colocated", Reason: "list failed"})
		return
	}
	row := colocatedMigrationRow{Collection: CollectionBackupRuns, Source: "colocated", SourceCount: int64(len(rows))}
	resp.SourceCount += row.SourceCount
	if !dryRun {
		coll := c.Sink.Collection(CollectionBackupRuns)
		for _, r := range rows {
			filter := bson.M{"_id": r.ID}
			opts := options.Replace().SetUpsert(true)
			if _, err := coll.ReplaceOne(ctx, filter, r, opts); err != nil {
				resp.Errors = append(resp.Errors, migrationError{Collection: CollectionBackupRuns, ID: r.ID, Err: "upsert: " + err.Error()})
				continue
			}
			row.MigratedCount++
			resp.MigratedCount++
		}
	}
	resp.Rows = append(resp.Rows, row)
}

// restoreLister is the narrow interface the colocated RunStore
// satisfies for cross-tenant restore enumeration. The scheduler.
// RunStore interface itself does not expose this — restore list-all
// is a migration-only read path, and widening the interface would
// force every backend (Mongo, Postgres, MySQL, SQLite, in-memory)
// to carry a method they never need. Keeping it here as a narrow
// type assertion means the interface stays minimal and a colocated-
// source migration is the only caller.
type restoreLister interface {
	ListAllRestores(ctx context.Context, limit int) ([]backup.RestoreRun, error)
}

// migrateRestoreRuns walks every tenant's restores/ prefix via the
// colocated RunStore's ListAllRestores extension (added in the same
// commit as this endpoint — see colocated_runs.go). The sink filter
// key is `_id` (RestoreRun.ID). Dedupe is unnecessary because each
// restore key is already unique on ID.
func (c *ColocatedMigrationConfig) migrateRestoreRuns(ctx context.Context, dryRun bool, resp *colocatedMigrationResponse) {
	row := colocatedMigrationRow{Collection: CollectionRestoreRuns, Source: "colocated"}
	rs, ok := c.Source.Runs().(restoreLister)
	if !ok {
		// A non-colocated RunStore reached this handler. Report the
		// gap without failing the whole migration — the operator
		// learns which backend was wired and can rewire deliberately.
		row.Source = "none"
		row.Reason = "underlying RunStore does not implement ListAllRestores"
		resp.Rows = append(resp.Rows, row)
		return
	}
	rows, err := rs.ListAllRestores(ctx, 0)
	if err != nil {
		resp.Errors = append(resp.Errors, migrationError{Collection: CollectionRestoreRuns, Err: "list: " + err.Error()})
		row.Reason = "list failed"
		resp.Rows = append(resp.Rows, row)
		return
	}
	row.SourceCount = int64(len(rows))
	resp.SourceCount += row.SourceCount
	if !dryRun {
		coll := c.Sink.Collection(CollectionRestoreRuns)
		for _, rr := range rows {
			filter := bson.M{"_id": rr.ID}
			opts := options.Replace().SetUpsert(true)
			if _, err := coll.ReplaceOne(ctx, filter, rr, opts); err != nil {
				resp.Errors = append(resp.Errors, migrationError{Collection: CollectionRestoreRuns, ID: rr.ID, Err: "upsert: " + err.Error()})
				continue
			}
			row.MigratedCount++
			resp.MigratedCount++
		}
	}
	resp.Rows = append(resp.Rows, row)
}

// migrateSnapshots walks each project's snapshot prefix and upserts
// each row via scheduler.SnapshotToBSONDoc — the same wire shape the
// MongoSnapshotStore writes (snapshotDoc, bson tags). stats.DbSnapshot
// itself carries only json tags because it is the stats package's
// wire shape; the Mongo-level mapping lives in scheduler and is
// re-used here so a drift between the writer and the migrator is one
// edit instead of two.
//
// The colocated SnapshotStore reader is scoped per-project; projs is
// the seed. Snapshots under projects not yet migrated will not land
// — but by this point in Migrate() projects has already been walked,
// so the seed is the full set.
func (c *ColocatedMigrationConfig) migrateSnapshots(ctx context.Context, projs []project.Config, dryRun bool, resp *colocatedMigrationResponse) {
	row := colocatedMigrationRow{Collection: CollectionSnapshots, Source: "colocated"}
	coll := c.Sink.Collection(CollectionSnapshots)
	seen := map[int64]bool{}
	for _, p := range projs {
		snaps, err := c.Source.Snapshots().SnapshotsForProject(ctx, p.Name, time.Time{})
		if err != nil {
			resp.Errors = append(resp.Errors, migrationError{Collection: CollectionSnapshots, ID: p.Name, Err: "list: " + err.Error()})
			continue
		}
		for _, s := range snaps {
			if seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			row.SourceCount++
			resp.SourceCount++
			if !dryRun {
				doc := scheduler.SnapshotToBSONDoc(s)
				filter := bson.M{"_id": s.ID}
				opts := options.Replace().SetUpsert(true)
				if _, err := coll.ReplaceOne(ctx, filter, doc, opts); err != nil {
					resp.Errors = append(resp.Errors, migrationError{Collection: CollectionSnapshots, ID: fmt.Sprintf("%d", s.ID), Err: "upsert: " + err.Error()})
					continue
				}
				row.MigratedCount++
				resp.MigratedCount++
			}
		}
	}
	resp.Rows = append(resp.Rows, row)
}
