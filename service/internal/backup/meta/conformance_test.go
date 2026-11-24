// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package meta

// Conformance tests — cross-implementation agreement on the Store
// interface. Each test seeds identical inputs into BOTH colocated and
// mongo backends, then asserts both return the same shape on reads.
//
// WHY THIS EXISTS. The service picks ONE metadata backend at startup
// (cmd/backup/main.go). Post-`USE_GOEMBEDB=1`, that choice flips from
// colocated to mongo. The two are independent implementations of the
// same interface (meta.Store), developed in parallel, each with its
// own per-backend test file that asserts internally-consistent
// hardcoded expectations. Nothing in the codebase asserts they agree
// with EACH OTHER on identical inputs — so sort order, nil-vs-empty
// semantics, time-precision round-trip, missing-field decode, and
// ErrNotFound shape can all drift silently between them. The failure
// mode is a plausible wrong answer rather than an error: a tenant that
// looks deleted on one side and visible on the other, a timestamp
// that disagrees between pre-flip and post-flip SPA renders, a list
// ordering that differs on a tie.
//
// This suite is the differential test that catches those. It is
// written to FAIL today on every axis it has reached — a conformance
// suite that passes on first write against two independent impls is
// weaker evidence of agreement than it looks, per the "a test that
// passes on first write is not yet a regression test" rule. The
// failures ARE the findings; each test's error message names the
// specific divergence and the file/line where it lives, so the flip
// PR can either resolve them in code (preferred) or document the
// acceptance with a specific mitigation. DO NOT "fix" these tests by
// relaxing their assertions — the divergence they surface is the
// signal they exist to produce.
//
// Divergences confirmed against colocated + mongo on 2026-11-24, in
// descending order of blast radius post-flip:
//
//  1. SOFT-DELETED VISIBILITY. `colocated.Tenants().List()` filters
//     out `SoftDeleted` rows (colocated_tenants.go:34). `mongo` does
//     not (tenant/store.go:81, "for operator SPA visibility"). Post-
//     flip the SPA's tenant list grows by every soft-deleted tenant
//     — visibly on a reload. Pick one shape and align.
//
//  2. TIME-PRECISION ROUND-TRIP. Colocated preserves ns precision
//     through JSON. Mongo truncates to ms via BSON datetime. Operator
//     reading `createdAt` pre- and post-flip on the same tenant sees
//     different ns tails. Low operational risk but will trip any
//     downstream consumer that expects exact-equality on timestamps.
//
//  3. NIL-VS-EMPTY SLICE. An empty-store `List()` returns `[]` (non-
//     nil) from colocated and `nil` from mongo. Leaks into
//     `/api/v1/overview`'s JSON on a tenantless deploy as
//     `"projects": []` vs `"projects": null`.
//
// `TestConformance_Tenants_GetMissingShape` passed on first run: both
// impls return `(Record{}, false, nil)` on a Get for an absent key.
// That is the ONE axis where the two impls agree today; the test
// stays as a regression guard against future drift.
//
// RUNNING. The mongo side needs a reachable Mongo instance. Set
// `BACKUP_CONFORMANCE_URI` to its connection string; unset skips the
// whole suite (not just the mongo path — the whole conformance is a
// no-op without it, since cross-impl agreement cannot be tested
// without two impls). Each test drops the mongo database in cleanup;
// concurrent runs against the same URI collide on `sc_component_backup`
// and are intentionally serial.
//
//   BACKUP_CONFORMANCE_URI=mongodb://127.0.0.1:27018 go test ./service/internal/backup/meta/ -run TestConformance_ -count=1 -v

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// confPair bundles the two live stores under test. Both are the SAME
// Store interface, constructed from the SAME seed in each test.
type confPair struct {
	colocated Store
	mongo     Store
}

// setupConformancePair returns a (colocated, mongo) pair. The mongo
// side is pointed at a freshly-dropped `sc_component_backup` database
// so cross-test state cannot leak; the colocated side is backed by an
// in-memory `storage.Store` so there is nothing to clean up explicitly.
//
// Skips the test (not fatal) if `BACKUP_CONFORMANCE_URI` is unset — a
// contributor without mongo on PATH still sees a green `go test ./...`.
func setupConformancePair(t *testing.T) confPair {
	t.Helper()
	uri := strings.TrimSpace(os.Getenv("BACKUP_CONFORMANCE_URI"))
	if uri == "" {
		t.Skip("BACKUP_CONFORMANCE_URI not set; skipping conformance suite (needs mongo for the cross-impl comparison)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Drop the shared mongo DB BEFORE constructing the mongo store.
	// `newMongoStore` opens its four narrow sub-clients, each of which
	// writes into the hardcoded `sc_component_backup` DB (see the
	// defaultDBName constants in tenant/project/scheduler). We cannot
	// parameterise that from here without touching their constructors,
	// so isolation is "drop first, construct second, drop in cleanup."
	// Tests are therefore implicitly serial against this URI.
	admin, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect for cleanup: %v", err)
	}
	if err := admin.Database("sc_component_backup").Drop(ctx); err != nil {
		t.Fatalf("drop sc_component_backup for isolation: %v", err)
	}
	_ = admin.Disconnect(ctx)

	colocated, err := newColocatedStore(storage.NewInMemoryStore())
	if err != nil {
		t.Fatalf("newColocatedStore: %v", err)
	}
	mongoStore, err := newMongoStore(ctx, uri)
	if err != nil {
		t.Fatalf("newMongoStore: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = mongoStore.Close(cctx)
		_ = colocated.Close(cctx)
		cadmin, cerr := mongo.Connect(cctx, options.Client().ApplyURI(uri))
		if cerr == nil {
			_ = cadmin.Database("sc_component_backup").Drop(cctx)
			_ = cadmin.Disconnect(cctx)
		}
	})
	return confPair{colocated: colocated, mongo: mongoStore}
}

// seedTenantToBoth inserts the same tenant record into both impls.
// Returns the error from each if they disagree on write-side success.
func seedTenantToBoth(t *testing.T, p confPair, rec tenant.Record) {
	t.Helper()
	ctx := context.Background()
	if err := p.colocated.Tenants().Upsert(ctx, rec); err != nil {
		t.Fatalf("colocated upsert %q: %v", rec.StackName, err)
	}
	if err := p.mongo.Tenants().Upsert(ctx, rec); err != nil {
		t.Fatalf("mongo upsert %q: %v", rec.StackName, err)
	}
}

// listTenantsSorted returns both impls' tenant lists, sorted by
// StackName so ordering differences are normalized out at the
// comparison level. Returns the raw slices too so a test that WANTS
// to compare order can.
type tenantListResult struct {
	rawColocated []tenant.Record
	rawMongo     []tenant.Record
	sortedC      []tenant.Record
	sortedM      []tenant.Record
}

func listTenantsBoth(t *testing.T, p confPair) tenantListResult {
	t.Helper()
	ctx := context.Background()
	rawC, err := p.colocated.Tenants().List(ctx)
	if err != nil {
		t.Fatalf("colocated tenants list: %v", err)
	}
	rawM, err := p.mongo.Tenants().List(ctx)
	if err != nil {
		t.Fatalf("mongo tenants list: %v", err)
	}
	// Copy then sort. Don't mutate the inputs; a future test that
	// compares raw ordering needs the originals.
	sortedC := append([]tenant.Record(nil), rawC...)
	sortedM := append([]tenant.Record(nil), rawM...)
	sort.Slice(sortedC, func(i, j int) bool { return sortedC[i].StackName < sortedC[j].StackName })
	sort.Slice(sortedM, func(i, j int) bool { return sortedM[i].StackName < sortedM[j].StackName })
	return tenantListResult{rawColocated: rawC, rawMongo: rawM, sortedC: sortedC, sortedM: sortedM}
}

// TestConformance_Tenants_EmptyStoreListShape asserts that both
// impls return the same shape on an empty store. Two different kinds
// of "empty" the SPA has to handle:
//
//   - length 0: both must agree.
//   - nil-vs-[]: `len == 0` is agnostic, but a caller that does
//     `rows == nil` sees a different result between impls.
//
// The service's overview handler uses a mix of patterns; a divergence
// here can leak into JSON (`null` vs `[]` on empty projects list on
// `/api/v1/overview`).
func TestConformance_Tenants_EmptyStoreListShape(t *testing.T) {
	p := setupConformancePair(t)
	res := listTenantsBoth(t, p)
	if len(res.rawColocated) != 0 || len(res.rawMongo) != 0 {
		t.Errorf("empty store but got non-empty: colocated=%d mongo=%d",
			len(res.rawColocated), len(res.rawMongo))
	}
	cNil := res.rawColocated == nil
	mNil := res.rawMongo == nil
	if cNil != mNil {
		t.Errorf("empty-store nil-vs-empty-slice disagreement: colocated nil=%t  mongo nil=%t", cNil, mNil)
	}
}

// TestConformance_Tenants_SoftDeletedVisibility is the first expected
// divergence. The SoftDelete() method marks a row rather than
// removing it. Colocated's List filters out SoftDeleted=true rows
// (colocated_tenants.go:34-36). Mongo's List returns every row
// including soft-deleted ones (tenant/store.go:81 comment: "for
// operator SPA visibility").
//
// Post-flip the SPA's tenant list changes shape — soft-deleted
// tenants that were hidden pre-flip reappear. That is a behavioural
// change the flip must either resolve (align the two impls) or
// document (SPA accepts both shapes).
//
// This test WILL FAIL on first run against both impls as currently
// written. The failure IS the finding; the test is committed as a
// regression guard once the divergence is resolved.
func TestConformance_Tenants_SoftDeletedVisibility(t *testing.T) {
	p := setupConformancePair(t)

	kept := tenant.Record{
		StackName:  "alive",
		Version:    "v1",
		CreatedAt:  time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
		LastSeenAt: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
	}
	gone := tenant.Record{
		StackName:  "soft-deleted",
		Version:    "v1",
		CreatedAt:  time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
		LastSeenAt: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
	}
	seedTenantToBoth(t, p, kept)
	seedTenantToBoth(t, p, gone)
	ctx := context.Background()
	if err := p.colocated.Tenants().SoftDelete(ctx, "soft-deleted"); err != nil {
		t.Fatalf("colocated softdelete: %v", err)
	}
	if err := p.mongo.Tenants().SoftDelete(ctx, "soft-deleted"); err != nil {
		t.Fatalf("mongo softdelete: %v", err)
	}
	res := listTenantsBoth(t, p)
	if len(res.rawColocated) != len(res.rawMongo) {
		t.Errorf("tenant.List shape disagrees on soft-deleted rows: colocated=%d (filters out), mongo=%d (includes). See colocated_tenants.go:34 vs tenant/store.go:81. Flip PR must align: either colocated stops filtering or mongo starts, with the SPA tested against whichever shape wins.",
			len(res.rawColocated), len(res.rawMongo))
	}
}

// TestConformance_Tenants_GetMissingShape asserts that a Get for a
// non-existent key returns the SAME three-tuple shape on both impls:
// zero Record, false, nil. The caller switches on `ok` without
// checking `err`, so a disagreement on the error shape (one returns
// (Record{}, false, nil), the other returns (Record{}, false,
// someErr)) would fire two different code paths in the handler.
func TestConformance_Tenants_GetMissingShape(t *testing.T) {
	p := setupConformancePair(t)
	ctx := context.Background()

	recC, okC, errC := p.colocated.Tenants().Get(ctx, "does-not-exist")
	recM, okM, errM := p.mongo.Tenants().Get(ctx, "does-not-exist")

	if okC != okM {
		t.Errorf("Get(missing) ok disagreement: colocated=%t mongo=%t", okC, okM)
	}
	if (errC == nil) != (errM == nil) {
		t.Errorf("Get(missing) error-nil-shape disagreement: colocated err=%v mongo err=%v", errC, errM)
	}
	if !reflect.DeepEqual(recC, tenant.Record{}) || !reflect.DeepEqual(recM, tenant.Record{}) {
		t.Errorf("Get(missing) returned non-zero record: colocated=%+v mongo=%+v", recC, recM)
	}
}

// TestConformance_Tenants_TimePrecisionRoundtrip asserts that a time
// value survives a write-read round-trip through both impls at the
// SAME precision. Mongo truncates `time.Time` to milliseconds (BSON
// datetime is int64 milliseconds). Colocated round-trips through
// JSON, which carries full nanosecond precision.
//
// The test seeds a record with a `time.Time` that has sub-millisecond
// precision and asserts both impls round-trip it to the SAME value
// after read. The expected first-write result: colocated returns the
// original ns-precision value; mongo returns the ms-truncated form.
// This is a known BSON property, so the handler-side fix is either
// to normalize on write (mongo pattern) or to tolerate the drift at
// comparison (handler pattern). Either way the test surfaces the
// divergence before the flip rather than after.
func TestConformance_Tenants_TimePrecisionRoundtrip(t *testing.T) {
	p := setupConformancePair(t)
	// A time with microsecond precision — Mongo will truncate to ms
	// but not whole-second. Chose one with a distinctive sub-ms
	// component (123,456 ns = 123 µs + 456 ns) so the truncation shape
	// is readable in the test output.
	t0 := time.Date(2026, 11, 24, 10, 15, 30, 123_456_789, time.UTC)
	rec := tenant.Record{
		StackName:  "time-probe",
		Version:    "v1",
		CreatedAt:  t0,
		LastSeenAt: t0,
	}
	seedTenantToBoth(t, p, rec)

	ctx := context.Background()
	gotC, okC, _ := p.colocated.Tenants().Get(ctx, "time-probe")
	gotM, okM, _ := p.mongo.Tenants().Get(ctx, "time-probe")
	if !okC || !okM {
		t.Fatalf("Get(time-probe) after seed: colocated ok=%t mongo ok=%t", okC, okM)
	}

	// Report both values at a precision operators can read (nanos
	// down to the last digit). A reviewer reading CI output sees
	// what the truncation shape actually is.
	cNanos := gotC.CreatedAt.UnixNano()
	mNanos := gotM.CreatedAt.UnixNano()
	origNanos := t0.UnixNano()

	if cNanos != mNanos {
		t.Errorf("time precision round-trip disagrees: original=%d (ns), colocated=%d (preserves ns through JSON round-trip), mongo=%d (truncates to ms per BSON datetime). Flip PR must align: either colocated normalizes on write (truncate to ms before JSON marshal) or callers tolerate the drift at comparison. An operator comparing pre-flip and post-flip `createdAt` on the same tenant sees different ns tails until this resolves.",
			origNanos, cNanos, mNanos)
	}
	_ = origNanos
}

// --- Runs ------------------------------------------------------------
//
// scheduler.RunStore writes backup.BackupRun rows keyed by `_id`. The
// colocated impl writes JSON files at
// `_meta/runs/<enc-tenant>/<ts>-<enc-id>.json` and client-side-sorts
// `ListBackupsByTenant` by StartedAt DESC (colocated_runs.go:65). The
// mongo impl sorts via `SetSort(startedAt DESC)` at Find time
// (store_mongo.go:152). An earlier prediction (mine and sc-components')
// was that lexical-vs-natural ordering would diverge here; it does
// not, because both impls apply the SAME startedAt DESC sort. The
// test below is a regression guard against future impl drift.

// TestConformance_Runs_ListBackupsByTenant_SortOrder seeds three runs
// with non-chronological IDs — IDs whose LEXICAL order differs from
// their StartedAt order — and asserts both impls return them in the
// SAME order. A fixture that chose IDs sorting the same way as their
// StartedAt (e.g. run-1 at t+0, run-2 at t+1) would not distinguish
// a lexical-sort impl from a startedAt-sort one; this fixture does.
func TestConformance_Runs_ListBackupsByTenant_SortOrder(t *testing.T) {
	p := setupConformancePair(t)
	ctx := context.Background()

	tenantStack := "sort-probe-tenant"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seed := []backup.BackupRun{
		{
			// Oldest by StartedAt, but ID sorts LAST lexically.
			ID: "run-z-oldest", TenantStack: tenantStack,
			StartedAt: base, Driver: "mongodb", Status: backup.StatusSucceeded,
		},
		{
			// Middle by StartedAt, ID sorts first lexically.
			ID: "run-a-middle", TenantStack: tenantStack,
			StartedAt: base.Add(30 * 24 * time.Hour), Driver: "mongodb", Status: backup.StatusSucceeded,
		},
		{
			// Newest by StartedAt, ID sorts MIDDLE lexically.
			ID: "run-m-newest", TenantStack: tenantStack,
			StartedAt: base.Add(60 * 24 * time.Hour), Driver: "mongodb", Status: backup.StatusSucceeded,
		},
	}
	// Lexical order:   [run-a-middle, run-m-newest, run-z-oldest]
	// StartedAt DESC:  [run-m-newest, run-a-middle, run-z-oldest]
	// Both impls are expected to return the StartedAt DESC order; the
	// lexical-order outcome is the one this test rejects.
	wantOrder := []string{"run-m-newest", "run-a-middle", "run-z-oldest"}

	for _, r := range seed {
		if err := p.colocated.Runs().PutBackup(ctx, r); err != nil {
			t.Fatalf("colocated PutBackup %q: %v", r.ID, err)
		}
		if err := p.mongo.Runs().PutBackup(ctx, r); err != nil {
			t.Fatalf("mongo PutBackup %q: %v", r.ID, err)
		}
	}

	gotC, err := p.colocated.Runs().ListBackupsByTenant(ctx, tenantStack)
	if err != nil {
		t.Fatalf("colocated ListBackupsByTenant: %v", err)
	}
	gotM, err := p.mongo.Runs().ListBackupsByTenant(ctx, tenantStack)
	if err != nil {
		t.Fatalf("mongo ListBackupsByTenant: %v", err)
	}

	idsC := make([]string, len(gotC))
	for i, r := range gotC {
		idsC[i] = r.ID
	}
	idsM := make([]string, len(gotM))
	for i, r := range gotM {
		idsM[i] = r.ID
	}

	if !reflect.DeepEqual(idsC, wantOrder) {
		t.Errorf("colocated ListBackupsByTenant order disagrees with StartedAt DESC: got %v, want %v. Impl may have changed from startedAt-DESC sort to something else.", idsC, wantOrder)
	}
	if !reflect.DeepEqual(idsM, wantOrder) {
		t.Errorf("mongo ListBackupsByTenant order disagrees with StartedAt DESC: got %v, want %v.", idsM, wantOrder)
	}
	// The cross-impl check is what the suite exists for, so state it
	// directly in addition to each impl's expected shape above:
	if !reflect.DeepEqual(idsC, idsM) {
		t.Errorf("ListBackupsByTenant order disagrees across impls: colocated=%v mongo=%v. Both should sort StartedAt DESC.", idsC, idsM)
	}
}

// TestConformance_Runs_GetMissingErrorShape asserts both impls return
// an error of the same shape (non-nil, formatted similarly) on a
// GetBackup for an absent `_id`. A caller doing substring-match or
// errors.Is on the returned error sees a different outcome between
// impls if the shapes drift.
//
// Current code (as of 2026-11-24):
//   - colocated: errors.New("backup run %q not found", id) via fmt.Errorf
//     (colocated_runs.go:42)
//   - mongo:     errors.Errorf("backup-run %q: not found", id)
//     (scheduler/store_mongo.go:134)
//
// Note the formatting divergence: "backup run" vs "backup-run", and
// the colon separator. Any caller doing an error-message substring
// check (not just errors.Is) would see one impl match and the other
// not. The test asserts BOTH impls return an error that CONTAINS
// "not found" so a loose caller does not break on the flip; strict
// callers using errors.Is or errors.As would need a sentinel error
// the interface does not currently expose, which is a separate
// refactor.
func TestConformance_Runs_GetMissingErrorShape(t *testing.T) {
	p := setupConformancePair(t)
	ctx := context.Background()

	_, errC := p.colocated.Runs().GetBackup(ctx, "not-a-real-id")
	_, errM := p.mongo.Runs().GetBackup(ctx, "not-a-real-id")

	if errC == nil {
		t.Errorf("colocated GetBackup(missing) returned nil error; impl must signal missing")
	}
	if errM == nil {
		t.Errorf("mongo GetBackup(missing) returned nil error; impl must signal missing")
	}
	if errC == nil || errM == nil {
		return
	}
	if !strings.Contains(errC.Error(), "not found") {
		t.Errorf("colocated error does not contain 'not found': %q", errC.Error())
	}
	if !strings.Contains(errM.Error(), "not found") {
		t.Errorf("mongo error does not contain 'not found': %q", errM.Error())
	}
	// Flag the formatting divergence as a WARNING via t.Log; it is
	// not caught by the assertions above but is a real drift that
	// strict callers would trip on. "backup run" vs "backup-run" is
	// the current shape.
	if errC.Error() != errM.Error() {
		t.Logf("NOTE: error message formatting differs across impls (not a failure here since both contain 'not found', but strict callers would break): colocated=%q mongo=%q",
			errC.Error(), errM.Error())
	}
}

// --- Projects --------------------------------------------------------
//
// project.Store filters SoftDeleted=true rows on List in BOTH impls
// (colocated_projects.go:114 and project/store_mongo.go:158). This is
// the OPPOSITE of tenant.Store, whose mongo impl deliberately
// includes soft-deleted "for operator SPA visibility" — a
// cross-interface inconsistency worth naming. The test below is a
// regression guard against future drift on the project side.

// TestConformance_Projects_SoftDeletedFiltered seeds a soft-deleted
// row + a live row in both impls, calls List, and asserts both impls
// return exactly the live row. A regression (one impl starts
// including soft-deleted) would fire this test.
func TestConformance_Projects_SoftDeletedFiltered(t *testing.T) {
	p := setupConformancePair(t)
	ctx := context.Background()

	alive := project.Config{
		Name: "project-alive", Engine: "mongodb",
		Interval: "1h", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	gone := project.Config{
		Name: "project-gone", Engine: "mongodb",
		Interval: "1h", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}

	createdCA, err := p.colocated.Projects().Create(ctx, alive)
	if err != nil {
		t.Fatalf("colocated Create(alive): %v", err)
	}
	createdCG, err := p.colocated.Projects().Create(ctx, gone)
	if err != nil {
		t.Fatalf("colocated Create(gone): %v", err)
	}
	createdMA, err := p.mongo.Projects().Create(ctx, alive)
	if err != nil {
		t.Fatalf("mongo Create(alive): %v", err)
	}
	createdMG, err := p.mongo.Projects().Create(ctx, gone)
	if err != nil {
		t.Fatalf("mongo Create(gone): %v", err)
	}
	_ = createdCA
	_ = createdMA
	// SoftDelete needs the current Revision — use the one Create
	// returned (impls are free to set it, and they may disagree on
	// shape; the Revision from Create IS what the subsequent
	// SoftDelete call must match).
	if err := p.colocated.Projects().SoftDelete(ctx, createdCG.Name, createdCG.Revision); err != nil {
		t.Fatalf("colocated SoftDelete: %v", err)
	}
	if err := p.mongo.Projects().SoftDelete(ctx, createdMG.Name, createdMG.Revision); err != nil {
		t.Fatalf("mongo SoftDelete: %v", err)
	}

	gotC, err := p.colocated.Projects().List(ctx)
	if err != nil {
		t.Fatalf("colocated List: %v", err)
	}
	gotM, err := p.mongo.Projects().List(ctx)
	if err != nil {
		t.Fatalf("mongo List: %v", err)
	}

	if len(gotC) != 1 || gotC[0].Name != "project-alive" {
		t.Errorf("colocated List after SoftDelete: expected [project-alive], got names=%v", projectNames(gotC))
	}
	if len(gotM) != 1 || gotM[0].Name != "project-alive" {
		t.Errorf("mongo List after SoftDelete: expected [project-alive], got names=%v", projectNames(gotM))
	}
	if len(gotC) != len(gotM) {
		t.Errorf("project.List disagrees across impls on soft-delete filter: colocated len=%d, mongo len=%d. Expected BOTH to filter (opposite of tenant.Store, which is a known cross-interface inconsistency).",
			len(gotC), len(gotM))
	}
}

func projectNames(cfgs []project.Config) []string {
	names := make([]string, len(cfgs))
	for i, c := range cfgs {
		names[i] = c.Name
	}
	return names
}

// To the next contributor adding to this suite: the seven tests above
// cover tenants (four), runs (two), and projects (one). The same
// shape (seed-both, read-both, compare) generalises to snapshots
// (Snapshots().SnapshotsForProject across a since-cutoff,
// Snapshots().ObjectSamplesForProject with database partition). Keep
// each test focused on ONE divergence axis; a test that fails on
// three things at once is a test nobody reads.
//
// Also note: a test that PASSES on first write against two
// independent impls is weaker evidence of agreement than it looks.
// If you write one and it's immediately green, strengthen the
// fixture — pick a time the mongo-driver won't round the way you
// expect, pick a key that encodes differently from its stackName,
// pick a value in a field with no bson tag but a json tag. The
// point is to surface where the two impls disagree today; silent
// agreement is surprising, not reassuring.
func init() {
	// No-op — placeholder so a future reviewer looking for a package
	// init() finds this comment and does not add one that fights
	// the per-test setupConformancePair cleanup contract.
	_ = fmt.Sprint
}
