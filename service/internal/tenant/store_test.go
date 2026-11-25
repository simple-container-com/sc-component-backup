// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package tenant

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pkg/errors"
)

// The match key must pin `_id`. A `stackName` filter picks the same row
// and reads just as naturally, which is exactly why this is a test and
// not a comment: on go-embedb the difference is whether the write is
// classified read-modify-write, and an RMW upsert is refused by every
// replica but the tenant's lease owner. Two warm Lambda execution
// environments made that ~50% of all `POST /api/v1/tenants/register`
// calls, failing the deploy of every service using this component.
func TestUpsertFilter_PinsID(t *testing.T) {
	got := UpsertFilter("forge-baas--staging")
	want := bson.M{"_id": "forge-baas--staging"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UpsertFilter = %v, want %v (an upsert not pinned to _id is RMW on go-embedb)", got, want)
	}
}

// fakeColl records every op in order. The ORDER is the point: a rekey
// that upserts before deleting trips the unique stackName index.
type fakeColl struct {
	docs []any // what Find returns
	err  error // what Find fails with

	ops        []string // "find" / "delete:<id>" / "replace:<id>"
	replaced   []Record
	deleteErr  error
	replaceErr error
}

func (f *fakeColl) Find(_ context.Context, _ any, _ ...*options.FindOptions) (*mongo.Cursor, error) {
	f.ops = append(f.ops, "find")
	if f.err != nil {
		return nil, f.err
	}

	return mongo.NewCursorFromDocuments(f.docs, nil, nil)
}

func (f *fakeColl) DeleteOne(_ context.Context, filter any, _ ...*options.DeleteOptions) (*mongo.DeleteResult, error) {
	f.ops = append(f.ops, "delete:"+idOf(filter))
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}

	return &mongo.DeleteResult{DeletedCount: 1}, nil
}

func (f *fakeColl) ReplaceOne(_ context.Context, filter, replacement any, _ ...*options.ReplaceOptions) (*mongo.UpdateResult, error) {
	f.ops = append(f.ops, "replace:"+idOf(filter))
	if r, ok := replacement.(Record); ok {
		f.replaced = append(f.replaced, r)
	}
	if f.replaceErr != nil {
		return nil, f.replaceErr
	}

	return &mongo.UpdateResult{UpsertedCount: 1}, nil
}

// idOf renders a filter's `_id` for the op log, ObjectIDs as hex.
func idOf(filter any) string {
	m, ok := filter.(bson.M)
	if !ok {
		return "?"
	}
	switch id := m["_id"].(type) {
	case string:
		return id
	case primitive.ObjectID:
		return id.Hex()
	default:
		return "?"
	}
}

// legacyDoc is a row as it exists in the collection TODAY: `_id` is a
// generated ObjectID and the stack name lives only in `stackName`.
func legacyDoc(oid primitive.ObjectID, stackName string) bson.D {
	return bson.D{
		{Key: "_id", Value: oid},
		{Key: "stackName", Value: stackName},
		{Key: "version", Value: "v1"},
		{Key: "orgId", Value: "simple-container-com"},
		{Key: "createdAt", Value: time.Now().UTC().Truncate(time.Second)},
	}
}

func checkOps(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ops = %v, want %v", got, want)
	}
}

// THE regression test. A legacy row must be deleted by its ObjectID
// BEFORE the `_id`-keyed row is inserted — the unique index on
// stackName rejects the insert otherwise, which would turn the ~50%
// upsert failure into a 100% one for exactly the tenants that already
// exist.
func TestRekeyLegacyRows_DeletesBeforeItInserts(t *testing.T) {
	oid := primitive.NewObjectID()
	f := &fakeColl{docs: []any{legacyDoc(oid, "forge-baas--staging")}}

	if err := rekeyLegacyRows(context.Background(), f); err != nil {
		t.Fatalf("rekeyLegacyRows: %v", err)
	}

	checkOps(t, f.ops, []string{"find", "delete:" + oid.Hex(), "replace:forge-baas--staging"})

	if len(f.replaced) != 1 {
		t.Fatalf("replaced %d rows, want 1", len(f.replaced))
	}
	if f.replaced[0].StackName != "forge-baas--staging" {
		t.Fatalf("replaced stackName = %q", f.replaced[0].StackName)
	}
	// The rekey must carry the row's fields across, not just its key —
	// a tenant that loses its version or interval stops being backed up
	// on the schedule the operator declared.
	if f.replaced[0].Version != "v1" || f.replaced[0].OrgID != "simple-container-com" {
		t.Fatalf("replaced row lost fields: %+v", f.replaced[0])
	}
}

// Idempotent: a row already keyed by its stack name is left alone. Every
// boot runs this and every replica runs it concurrently, so a rekey that
// rewrote already-keyed rows would be a permanent write storm.
func TestRekeyLegacyRows_SkipsRowsAlreadyKeyedByStackName(t *testing.T) {
	f := &fakeColl{docs: []any{bson.D{
		{Key: "_id", Value: "forge-baas--staging"},
		{Key: "stackName", Value: "forge-baas--staging"},
	}}}

	if err := rekeyLegacyRows(context.Background(), f); err != nil {
		t.Fatalf("rekeyLegacyRows: %v", err)
	}

	checkOps(t, f.ops, []string{"find"}) // no write at all
}

// A string `_id` that does NOT match stackName is still legacy — it was
// keyed by something else — and must be rekeyed. Guards the equality in
// the skip condition, which a bare `_, ok := row.ID.(string)` would
// wrongly treat as done.
func TestRekeyLegacyRows_RekeysAStringIDThatIsNotTheStackName(t *testing.T) {
	f := &fakeColl{docs: []any{bson.D{
		{Key: "_id", Value: "some-other-key"},
		{Key: "stackName", Value: "forge-baas--staging"},
	}}}

	if err := rekeyLegacyRows(context.Background(), f); err != nil {
		t.Fatalf("rekeyLegacyRows: %v", err)
	}

	checkOps(t, f.ops, []string{"find", "delete:some-other-key", "replace:forge-baas--staging"})
}

// A row with no stackName has no natural key to rekey onto. Leave it for
// an operator: deleting it would silently drop a tenant, and upserting
// it under `_id: ""` would collide with the next such row.
func TestRekeyLegacyRows_LeavesARowWithNoStackName(t *testing.T) {
	oid := primitive.NewObjectID()
	f := &fakeColl{docs: []any{bson.D{{Key: "_id", Value: oid}}}}

	if err := rekeyLegacyRows(context.Background(), f); err != nil {
		t.Fatalf("rekeyLegacyRows: %v", err)
	}

	checkOps(t, f.ops, []string{"find"}) // neither deleted nor rewritten
}

// Every row gets rekeyed — the loop must not stop after the first.
func TestRekeyLegacyRows_HandlesEveryRow(t *testing.T) {
	a, b := primitive.NewObjectID(), primitive.NewObjectID()
	f := &fakeColl{docs: []any{
		legacyDoc(a, "forge-baas--staging"),
		legacyDoc(b, "forge-sessions--staging"),
	}}

	if err := rekeyLegacyRows(context.Background(), f); err != nil {
		t.Fatalf("rekeyLegacyRows: %v", err)
	}

	checkOps(t, f.ops, []string{
		"find",
		"delete:" + a.Hex(), "replace:forge-baas--staging",
		"delete:" + b.Hex(), "replace:forge-sessions--staging",
	})
}

// An error on any leg is reported, not swallowed. The CALLER decides
// this is non-fatal (see NewMongoStore); swallowing it here would hide a
// collection that never rekeys from the warning that says so.
func TestRekeyLegacyRows_ReportsErrors(t *testing.T) {
	oid := primitive.NewObjectID()
	boom := errors.New("boom")

	cases := map[string]*fakeColl{
		"find":    {err: boom},
		"delete":  {docs: []any{legacyDoc(oid, "s")}, deleteErr: boom},
		"replace": {docs: []any{legacyDoc(oid, "s")}, replaceErr: boom},
	}

	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			err := rekeyLegacyRows(context.Background(), f)
			if err == nil {
				t.Fatalf("rekeyLegacyRows: want an error when %s fails", name)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("rekeyLegacyRows: err = %v, want it to wrap %v", err, boom)
			}
		})
	}
}
