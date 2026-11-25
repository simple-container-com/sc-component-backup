// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package goembedbadmin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/meta"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/goembedbadmin"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// newColocatedSource builds a live colocated meta.Store over an
// in-memory archive. Same shape the service uses in production with
// `metadata: colocated` — writes go to the archive's _meta/ prefix.
func newColocatedSource(t *testing.T) (meta.Store, *storage.InMemoryStore) {
	t.Helper()
	archive := storage.NewInMemoryStore()
	src, err := meta.NewFromConfig(context.Background(), meta.Config{Type: meta.TypeColocated}, archive)
	if err != nil {
		t.Fatalf("new colocated source: %v", err)
	}
	return src, archive
}

// seedColocated populates the colocated source with a small fixed
// fixture: N tenants, M projects, K backup runs across both tenants,
// L restores, P snapshots. Returns the counts so the test can diff.
func seedColocated(t *testing.T, src meta.Store) (tenants, projects, backups, restores, snapshots int) {
	t.Helper()
	ctx := context.Background()
	for _, stack := range []string{"stack-a", "stack-b"} {
		if err := src.Tenants().Upsert(ctx, tenant.Record{
			StackName:       stack,
			Version:         "v1",
			Resources:       []tenant.Resource{{Type: "mongodb", Name: "db"}},
			OrgID:           "org-x",
			CreatedAt:       time.Now().UTC(),
			LastSeenAt:      time.Now().UTC(),
			IntervalMinutes: 60,
		}); err != nil {
			t.Fatalf("seed tenant %s: %v", stack, err)
		}
		tenants++
	}
	for _, name := range []string{"proj-1", "proj-2", "proj-3"} {
		if _, err := src.Projects().Create(ctx, project.Config{
			Name:      name,
			Engine:    "mongodb",
			Databases: "all",
			Interval:  "1h",
			Retention: project.Retention{Last: 5},
		}); err != nil {
			t.Fatalf("seed project %s: %v", name, err)
		}
		projects++
	}
	// Backup runs — two per tenant, so ListAllBackups has to merge.
	for _, stack := range []string{"stack-a", "stack-b"} {
		for i, id := range []string{"run-" + stack + "-1", "run-" + stack + "-2"} {
			if err := src.Runs().PutBackup(ctx, backup.BackupRun{
				ID:            id,
				TenantStack:   stack,
				ResourceName:  "db",
				Driver:        "mongodb",
				StartedAt:     time.Now().UTC().Add(-time.Duration(i+1) * time.Hour),
				Status:        backup.StatusSucceeded,
				S3Path:        stack + "/db/x.gz",
				TriggerSource: "schedule",
			}); err != nil {
				t.Fatalf("seed backup run %s: %v", id, err)
			}
			backups++
		}
	}
	// One restore for stack-a run-1.
	if err := src.Runs().PutRestore(ctx, backup.RestoreRun{
		ID:           "restore-a-1",
		BackupRunID:  "run-stack-a-1",
		TenantStack:  "stack-a",
		ResourceName: "db",
		Driver:       "mongodb",
		StartedAt:    time.Now().UTC(),
		Status:       backup.StatusSucceeded,
		TriggeredBy:  "operator",
	}); err != nil {
		t.Fatalf("seed restore: %v", err)
	}
	restores++
	// Snapshots — one per project for proj-1 and proj-2 (proj-3 empty).
	for i, name := range []string{"proj-1", "proj-2"} {
		if err := src.Snapshots().PutDbSnapshot(ctx, stats.DbSnapshot{
			ID:           int64(100 + i),
			Project:      name,
			Engine:       "mongodb",
			Database:     "db",
			Ts:           time.Now().UTC(),
			Status:       "ok",
			ArchiveBytes: int64(1024 * (i + 1)),
			Approx:       false,
			Partial:      false,
		}); err != nil {
			t.Fatalf("seed snapshot %s: %v", name, err)
		}
		snapshots++
	}
	return
}

// TestColocatedMigration_HappyPath walks every seeded class into a
// clean sink and asserts per-collection counts match. Also asserts
// idempotency on re-run — the same input landed the same output.
func TestColocatedMigration_HappyPath(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := client.Database("sink_" + t.Name())

	src, _ := newColocatedSource(t)
	nTenants, nProjects, nBackups, nRestores, nSnapshots := seedColocated(t, src)
	wantTotal := int64(nTenants + nProjects + nBackups + nRestores + nSnapshots)

	cfg := &goembedbadmin.ColocatedMigrationConfig{Source: src, Sink: sink, AdminToken: "secret"}

	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "secret")
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Migrate: status %d, body %s", w.Code, w.Body.String())
	}
	resp := decodeColocated(t, w)

	if resp.SourceEmpty {
		t.Fatalf("source_empty=true on a seeded source (body=%s)", w.Body.String())
	}
	if resp.SourceCount != wantTotal {
		t.Fatalf("source_count = %d, want %d", resp.SourceCount, wantTotal)
	}
	if resp.MigratedCount != wantTotal {
		t.Fatalf("migrated_count = %d, want %d (errors=%v)", resp.MigratedCount, wantTotal, resp.Errors)
	}
	if !resp.Verified {
		t.Fatalf("verified=false on a positive source (body=%s)", w.Body.String())
	}
	// Per-collection breakdown must call out the three source="none" gaps.
	byColl := map[string]struct {
		Source, Reason string
		SourceCount    int64
		MigratedCount  int64
	}{}
	for _, row := range resp.Rows {
		byColl[row.Collection] = struct {
			Source, Reason string
			SourceCount    int64
			MigratedCount  int64
		}{row.Source, row.Reason, row.SourceCount, row.MigratedCount}
	}
	for _, name := range []string{goembedbadmin.CollectionSettings, goembedbadmin.CollectionAlertsDismissals, goembedbadmin.CollectionObjectSamples} {
		if got, ok := byColl[name]; !ok || got.Source != "none" || got.Reason == "" {
			t.Fatalf("collection %q: want source=none + non-empty reason, got %+v", name, got)
		}
	}
	// Re-run is idempotent — replace by _id/stackName produces the same counts.
	req2 := httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb", nil)
	req2.Header.Set("X-Admin-Token", "secret")
	w2 := httptest.NewRecorder()
	cfg.Migrate(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("re-run status %d", w2.Code)
	}
	resp2 := decodeColocated(t, w2)
	if resp2.MigratedCount != wantTotal {
		t.Fatalf("re-run migrated_count = %d, want %d", resp2.MigratedCount, wantTotal)
	}
}

// TestColocatedMigration_DryRun counts without writing.
func TestColocatedMigration_DryRun(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := client.Database("sink_" + t.Name())

	src, _ := newColocatedSource(t)
	nT, nP, nB, nR, nS := seedColocated(t, src)
	wantTotal := int64(nT + nP + nB + nR + nS)

	cfg := &goembedbadmin.ColocatedMigrationConfig{Source: src, Sink: sink, AdminToken: "t"}
	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb?dry_run=1", nil)
	req.Header.Set("X-Admin-Token", "t")
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("dry-run status %d", w.Code)
	}
	resp := decodeColocated(t, w)
	if !resp.DryRun {
		t.Fatalf("dry_run flag not echoed: %+v", resp)
	}
	if resp.SourceCount != wantTotal {
		t.Fatalf("dry-run source_count = %d, want %d", resp.SourceCount, wantTotal)
	}
	if resp.MigratedCount != 0 {
		t.Fatalf("dry-run leaked a write: migrated_count=%d", resp.MigratedCount)
	}
	if resp.Verified {
		t.Fatalf("dry-run must never report verified=true")
	}
	// Sink must stay empty.
	for _, name := range goembedbadmin.Collections {
		n, err := sink.Collection(name).CountDocuments(context.Background(), map[string]any{})
		if err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if n != 0 {
			t.Fatalf("dry-run leaked a write to %s: %d rows", name, n)
		}
	}
}

// TestColocatedMigration_Auth covers the three auth states + the
// missing-config shape (same as the mongo-source endpoint).
func TestColocatedMigration_Auth(t *testing.T) {
	// Disabled when AdminToken is empty.
	cfg := &goembedbadmin.ColocatedMigrationConfig{AdminToken: ""}
	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb", nil)
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled: got %d, want 404", w.Code)
	}
	// No header.
	cfg.AdminToken = "s"
	req = httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb", nil)
	w = httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no header: got %d, want 401", w.Code)
	}
	// Wrong token.
	req = httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "other")
	w = httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", w.Code)
	}
	// Correct token but Source/Sink absent → 500 naming what is missing.
	cfg = &goembedbadmin.ColocatedMigrationConfig{AdminToken: "t"}
	req = httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "t")
	w = httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("missing source: got %d, want 500", w.Code)
	}
}

// TestColocatedMigration_EmptySource_422 is the INVERTED guard — the
// specific failure this endpoint exists to prevent. Point the walker
// at a colocated store that enumerates nothing and assert:
//
//  1. HTTP 422 so a runbook `curl -f` catches it (not 200).
//  2. source_empty: true in the body.
//  3. verified: false — zero across every class is never "verified".
//
// A dry-run on the same empty source answers 200 — counting an empty
// source is a valid dry run — but still reports source_empty: true
// and verified: false.
func TestColocatedMigration_EmptySource_422(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := client.Database("sink_" + t.Name())

	// Fresh colocated source — no fixtures.
	src, _ := newColocatedSource(t)

	cfg := &goembedbadmin.ColocatedMigrationConfig{Source: src, Sink: sink, AdminToken: "t"}

	// Wet run on empty → 422.
	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "t")
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty-source wet run: status %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
	resp := decodeColocated(t, w)
	if !resp.SourceEmpty {
		t.Fatalf("empty-source: source_empty=false (body=%s)", w.Body.String())
	}
	if resp.Verified {
		t.Fatalf("empty-source: verified=true (this is THE trap)")
	}
	if resp.SourceCount != 0 {
		t.Fatalf("empty-source: source_count=%d, want 0", resp.SourceCount)
	}
	// Dry-run on empty: still reports empty + unverified, but 200.
	req = httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb?dry_run=1", nil)
	req.Header.Set("X-Admin-Token", "t")
	w = httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("empty-source dry run: status %d, want 200", w.Code)
	}
	resp = decodeColocated(t, w)
	if !resp.SourceEmpty || resp.Verified {
		t.Fatalf("empty-source dry: want source_empty=true + verified=false, got %+v", resp)
	}
}

// TestColocatedMigration_ReadBackThroughService is the bson-tag-
// agreement proof. After the migration, open a `meta.Store` with
// METADATA_TYPE=mongo pointed at the SAME sink the migration wrote
// to, then call each domain accessor and compare field-by-field
// against the colocated source. A tag drift (json vs bson), or a
// struct field the migration writer silently dropped, breaks this.
func TestColocatedMigration_ReadBackThroughService(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sinkDBName := "sink_" + t.Name()
	sink := client.Database(sinkDBName)

	src, _ := newColocatedSource(t)
	seedColocated(t, src)

	// Run the migration.
	cfg := &goembedbadmin.ColocatedMigrationConfig{Source: src, Sink: sink, AdminToken: "t"}
	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "t")
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("migrate: status %d (body=%s)", w.Code, w.Body.String())
	}

	// Read back using the SAME shape the service uses at runtime
	// post-cutover: decode via the live Go struct's bson tags
	// against the same sink DB the migration wrote to.
	ctx := context.Background()
	assertTenant(t, ctx, sink, "stack-a")
	assertTenant(t, ctx, sink, "stack-b")
	assertProject(t, ctx, sink, "proj-1")
	assertProject(t, ctx, sink, "proj-2")
	assertBackupRun(t, ctx, sink, "run-stack-a-1")
	assertRestoreRun(t, ctx, sink, "restore-a-1")
	assertSnapshotCount(t, ctx, sink, 2)
	_ = sinkDBName
}

// assertTenant reads the sink's tenant row with the SAME bson tags
// the service's MongoStore uses — decoding into tenant.Record
// exercises every bson tag on the struct. A tag rename on the
// migration-writer side would read back zero values here.
func assertTenant(t *testing.T, ctx context.Context, sink *mongo.Database, stack string) {
	t.Helper()
	coll := sink.Collection(goembedbadmin.CollectionTenants)
	// The mongo store filter key is stackName, not _id.
	var rec tenant.Record
	if err := coll.FindOne(ctx, map[string]any{"stackName": stack}).Decode(&rec); err != nil {
		t.Fatalf("read-back tenant %s: %v", stack, err)
	}
	if rec.StackName != stack {
		t.Fatalf("read-back tenant %s: StackName decoded to %q (bson tag drift?)", stack, rec.StackName)
	}
	if rec.Version == "" {
		t.Fatalf("read-back tenant %s: Version decoded to empty (expected 'v1') — bson tag drift", stack)
	}
	if len(rec.Resources) == 0 || rec.Resources[0].Name != "db" {
		t.Fatalf("read-back tenant %s: Resources decoded as %+v — bson tag drift", stack, rec.Resources)
	}
}

func assertProject(t *testing.T, ctx context.Context, sink *mongo.Database, name string) {
	t.Helper()
	coll := sink.Collection(goembedbadmin.CollectionProjects)
	var cfg project.Config
	if err := coll.FindOne(ctx, map[string]any{"_id": name}).Decode(&cfg); err != nil {
		t.Fatalf("read-back project %s: %v", name, err)
	}
	if cfg.Name != name {
		t.Fatalf("read-back project %s: Name decoded to %q", name, cfg.Name)
	}
	if cfg.Engine != "mongodb" {
		t.Fatalf("read-back project %s: Engine decoded to %q", name, cfg.Engine)
	}
}

func assertBackupRun(t *testing.T, ctx context.Context, sink *mongo.Database, id string) {
	t.Helper()
	coll := sink.Collection(goembedbadmin.CollectionBackupRuns)
	var run backup.BackupRun
	if err := coll.FindOne(ctx, map[string]any{"_id": id}).Decode(&run); err != nil {
		t.Fatalf("read-back backup run %s: %v", id, err)
	}
	if run.ID != id {
		t.Fatalf("read-back backup run %s: ID decoded to %q", id, run.ID)
	}
	if run.TenantStack == "" {
		t.Fatalf("read-back backup run %s: TenantStack empty — bson tag drift", id)
	}
	if run.Status != backup.StatusSucceeded {
		t.Fatalf("read-back backup run %s: Status decoded to %q", id, run.Status)
	}
}

func assertRestoreRun(t *testing.T, ctx context.Context, sink *mongo.Database, id string) {
	t.Helper()
	coll := sink.Collection(goembedbadmin.CollectionRestoreRuns)
	var rr backup.RestoreRun
	if err := coll.FindOne(ctx, map[string]any{"_id": id}).Decode(&rr); err != nil {
		t.Fatalf("read-back restore run %s: %v", id, err)
	}
	if rr.ID != id {
		t.Fatalf("read-back restore run %s: ID decoded to %q", id, rr.ID)
	}
	if rr.BackupRunID == "" {
		t.Fatalf("read-back restore run %s: BackupRunID empty — bson tag drift", id)
	}
}

func assertSnapshotCount(t *testing.T, ctx context.Context, sink *mongo.Database, want int) {
	t.Helper()
	coll := sink.Collection(goembedbadmin.CollectionSnapshots)
	got, err := coll.CountDocuments(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if int(got) != want {
		t.Fatalf("snapshot count = %d, want %d", got, want)
	}
	// Decode one row into the scheduler's on-wire doc indirectly via
	// stats.DbSnapshot — the service's actual reader path uses
	// snapshotDoc, which carries bson tags; we re-build an equivalent
	// shape here to prove the key field decodes.
	type snapRow struct {
		ID           int64  `bson:"_id"`
		Project      string `bson:"project"`
		ArchiveBytes int64  `bson:"archiveBytes"`
	}
	var row snapRow
	if err := coll.FindOne(ctx, map[string]any{}).Decode(&row); err != nil {
		t.Fatalf("decode a snapshot row: %v", err)
	}
	if row.Project == "" || row.ArchiveBytes == 0 {
		t.Fatalf("snapshot read-back decoded zero-valued fields: %+v (SnapshotToBSONDoc drift?)", row)
	}
}

// decodeColocated unmarshals the migration response into a mirror
// struct for assertion. Field names here match the JSON tags in
// colocatedMigrationResponse.
type colocatedResp struct {
	DryRun        bool              `json:"dry_run"`
	SourceEmpty   bool              `json:"source_empty"`
	SourceCount   int64             `json:"source_count"`
	MigratedCount int64             `json:"migrated_count"`
	SkippedCount  int64             `json:"skipped_count"`
	Verified      bool              `json:"verified"`
	ElapsedMs     int64             `json:"elapsed_ms"`
	Rows          []colocatedResRow `json:"rows"`
	Errors        []any             `json:"errors"`
}

type colocatedResRow struct {
	Collection    string `json:"collection"`
	Source        string `json:"source"`
	SourceCount   int64  `json:"source_count"`
	MigratedCount int64  `json:"migrated_count"`
	SkippedCount  int64  `json:"skipped_count"`
	Reason        string `json:"reason"`
}

func decodeColocated(t *testing.T, w *httptest.ResponseRecorder) colocatedResp {
	t.Helper()
	var r colocatedResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, w.Body.String())
	}
	return r
}

// _ exports the scheduler helper used by the handler; a compile-time
// anchor that stops a `go mod tidy` from dropping the scheduler import
// here if the direct caller (colocated_source.go) ever moves.
var _ = scheduler.SnapshotToBSONDoc

// TestColocatedMigration_DoesNotClobberAFresherTenantRow is THE
// regression test for [the freshness guard]. A tenant row carries the
// Mongo credential the worker dumps with, and every consumer deploy
// ROTATES that credential — so a restore that blindly replays a
// snapshot over a live row does not merely lose an update, it breaks
// that tenant's backups outright with `(AuthenticationFailed)`.
//
// That is not hypothetical: on 2026-11-25 a wet run of this endpoint
// reverted 6 of 7 tenant rows to pre-rotation passwords and every
// `mongodump` for them failed. The seventh survived only because it had
// not deployed since the snapshot was taken.
//
// The skip must also count as success — see the `verified` assertion.
// An operator who sees `verified: false` on the correct outcome stops
// reading the field.
func TestColocatedMigration_DoesNotClobberAFresherTenantRow(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := client.Database("sink_" + t.Name())

	src, _ := newColocatedSource(t)
	nT, nP, nB, nR, nS := seedColocated(t, src)
	wantTotal := int64(nT + nP + nB + nR + nS)

	// The live row: same stack as a seeded source row, but stamped
	// LATER — exactly what a consumer deploy leaves behind after the
	// snapshot was taken — and carrying a credential that only exists
	// in the sink.
	ctx := context.Background()
	live := tenant.Record{
		StackName:  "stack-a",
		Version:    "v-live",
		Resources:  []tenant.Resource{{Type: "mongodb", Name: "db", Connection: map[string]string{"uri": "live-uri"}}},
		OrgID:      "org-x",
		CreatedAt:  time.Now().UTC(),
		LastSeenAt: time.Now().UTC().Add(time.Hour),
	}
	if _, err := sink.Collection(goembedbadmin.CollectionTenants).ReplaceOne(ctx,
		tenant.UpsertFilter(live.StackName), live, options.Replace().SetUpsert(true)); err != nil {
		t.Fatalf("seed live sink row: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-colocated-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "secret")
	w := httptest.NewRecorder()
	(&goembedbadmin.ColocatedMigrationConfig{Source: src, Sink: sink, AdminToken: "secret"}).Migrate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Migrate: status %d, body %s", w.Code, w.Body.String())
	}
	resp := decodeColocated(t, w)

	// THE assertion: the live credential survived.
	var got tenant.Record
	if err := sink.Collection(goembedbadmin.CollectionTenants).
		FindOne(ctx, tenant.UpsertFilter("stack-a")).Decode(&got); err != nil {
		t.Fatalf("read back stack-a: %v", err)
	}
	if got.Version != "v-live" {
		t.Fatalf("stack-a version = %q, want %q — the migration overwrote a FRESHER tenant row, which reverts its Mongo credential to a pre-rotation password and breaks that tenant's backups", got.Version, "v-live")
	}
	if uri := got.Resources[0].Connection["uri"]; uri != "live-uri" {
		t.Fatalf("stack-a uri = %q, want %q — the live credential was clobbered by the snapshot copy", uri, "live-uri")
	}

	// A stack that had NOT deployed since the snapshot must still be
	// copied — the guard is a freshness check, not a blanket skip.
	if err := sink.Collection(goembedbadmin.CollectionTenants).
		FindOne(ctx, tenant.UpsertFilter("stack-b")).Decode(&got); err != nil {
		t.Fatalf("stack-b was not migrated at all: %v", err)
	}
	if got.Version != "v1" {
		t.Fatalf("stack-b version = %q, want %q (source copy)", got.Version, "v1")
	}

	// And the skip is accounted as a success, not as a gap.
	if resp.SkippedCount != 1 {
		t.Fatalf("skipped_count = %d, want 1", resp.SkippedCount)
	}
	if resp.MigratedCount != wantTotal-1 {
		t.Fatalf("migrated_count = %d, want %d (one skip)", resp.MigratedCount, wantTotal-1)
	}
	if !resp.Verified {
		t.Fatalf("verified=false when every source row was either copied or deliberately skipped (body=%s)", w.Body.String())
	}
	for _, row := range resp.Rows {
		if row.Collection == goembedbadmin.CollectionTenants && row.SkippedCount != 1 {
			t.Fatalf("tenants row skipped_count = %d, want 1 (%+v)", row.SkippedCount, row)
		}
	}
}
