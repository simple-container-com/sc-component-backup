// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package tenant

import (
	"context"
	"log/slog"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pkg/errors"

	"github.com/simple-container-com/sc-component-backup/service/internal/mongox"
)

// Store is the tenant repository interface. Two implementations: MongoStore
// (production) and InMemoryStore (tests).
type Store interface {
	Upsert(ctx context.Context, record Record) error
	List(ctx context.Context) ([]Record, error)
	Get(ctx context.Context, stackName string) (Record, bool, error)
	SoftDelete(ctx context.Context, stackName string) error
	// HardDelete physically drops the row. Unlike SoftDelete, the row
	// is unrecoverable afterwards. Intended for orphan cleanup — a
	// consumer stack destroyed but its tenant row + archives left behind
	// because the plugin bus has no "consumer destroyed" event. Missing
	// row is not an error (idempotent).
	HardDelete(ctx context.Context, stackName string) error
	Close(ctx context.Context) error
}

// -------------------- Mongo implementation --------------------

const (
	defaultDBName         = "sc_component_backup"
	defaultCollectionName = "tenants"
)

// MongoStore is the production Store, backed by a single collection.
type MongoStore struct {
	client     *mongo.Client
	collection *mongo.Collection
}

// NewMongoStore opens a Mongo client, pings the server, creates an index
// on stackName. Blocks until the ping returns (bounded by ctx deadline).
func NewMongoStore(ctx context.Context, uri string) (*MongoStore, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, errors.Wrap(err, "mongo connect")
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, errors.Wrap(err, "mongo ping")
	}
	coll := client.Database(defaultDBName).Collection(defaultCollectionName)

	// Unique index on stackName — enforces (stackName, version) idempotency
	// at the DB level. The upsert match key is `_id`, not this index; see
	// [UpsertFilter] and [MongoStore.rekeyLegacyRows] for why, and for why
	// this index is the reason the rekey must delete before it inserts.
	_, err = coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "stackName", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err = mongox.TolerateNotWritablePrimary("tenants.stackName", err); err != nil {
		_ = client.Disconnect(ctx)
		return nil, errors.Wrap(err, "mongo index create")
	}
	s := &MongoStore{client: client, collection: coll}

	// Best-effort, and deliberately not fatal: a service that cannot
	// rekey still serves reads, and the next boot retries. Fatal here
	// would turn a migration hiccup into an outage.
	if err := rekeyLegacyRows(ctx, coll); err != nil {
		slog.WarnContext(ctx, "tenants: rekeying legacy ObjectID rows failed — upserts for pre-existing tenants may hit the stackName unique index until this succeeds",
			slog.String("err", err.Error()))
	}

	return s, nil
}

// UpsertFilter is the match key every writer of the tenants collection
// must use: `_id` pinned to the stack name.
//
// It is pinned to `_id` and NOT to `stackName` for a reason that is not
// about Mongo semantics at all — either filter picks the same row. On
// go-embedb, an upsert whose `_id` is not pinned by the filter or the
// replacement document is classified read-modify-write, because two
// replicas racing it would insert two documents with two generated
// ObjectIDs rather than converge. An RMW op is served only by the
// replica holding the tenant's lease; every other replica answers
// NotWritablePrimary(10107), and on Lambda — where each execution
// environment is its own replica and none is addressable — the client
// has nowhere to retry. With two warm environments, roughly half of all
// `POST /api/v1/tenants/register` calls answered `http 500: upsert
// failed`, which fails the deploy of every service that names this
// component in `uses:`. Live-caught 2026-11-25 on forge-baas.
//
// Pinning `_id` makes the write blind, so it converges under
// last-write-wins and never takes the lease. Keep every writer of this
// collection — including the colocated→go-embedb migration endpoint —
// on this function.
func UpsertFilter(stackName string) bson.M {
	return bson.M{"_id": stackName}
}

// Upsert inserts a new tenant row or replaces an existing one, keyed by
// stack name. See [UpsertFilter] for why the key is `_id`.
func (s *MongoStore) Upsert(ctx context.Context, record Record) error {
	opts := options.Replace().SetUpsert(true)
	if _, err := s.collection.ReplaceOne(ctx, UpsertFilter(record.StackName), record, opts); err != nil {
		return errors.Wrap(err, "mongo replace")
	}
	return nil
}

// rekeyCollection is the three-method slice of [mongo.Collection] that
// [rekeyLegacyRows] needs. It exists so the rekey can be tested against
// a fake — the repo has no live-Mongo harness, and the ordering this
// function has to get right (delete before insert) is invisible to a
// test that can only inspect the end state.
type rekeyCollection interface {
	Find(ctx context.Context, filter any, opts ...*options.FindOptions) (*mongo.Cursor, error)
	DeleteOne(ctx context.Context, filter any, opts ...*options.DeleteOptions) (*mongo.DeleteResult, error)
	ReplaceOne(ctx context.Context, filter, replacement any, opts ...*options.ReplaceOptions) (*mongo.UpdateResult, error)
}

// legacyRow is a tenant row plus whatever its `_id` happens to be. The
// `any` is load-bearing: pre-rekey rows carry a generated ObjectID and a
// `string` field here would fail to decode them.
type legacyRow struct {
	ID     any `bson:"_id"`
	Record `bson:",inline"`
}

// rekeyLegacyRows moves rows written before the `_id` key — their `_id`
// is a generated ObjectID — onto `_id: <stackName>`.
//
// Without this the first [MongoStore.Upsert] for an existing tenant
// matches nothing, inserts, and trips the unique index on `stackName`
// with a duplicate-key error: the fix would turn a 50%-failure into a
// 100%-failure for exactly the tenants that already exist.
//
// Idempotent and safe to run concurrently on several replicas: the
// delete is by the legacy `_id` (a no-op once another replica has done
// it) and the upsert is blind. Both ops are classified non-RMW by
// go-embedb, so no replica refuses them. Best-effort by design — a
// failure here must not stop the service from booting, and the next
// boot tries again.
func rekeyLegacyRows(ctx context.Context, coll rekeyCollection) error {
	// An unfiltered scan, not `{_id: {$type: "objectId"}}`: the tenants
	// collection holds one row per deployed stack (single digits), and a
	// query operator the engine does not implement would fail the scan,
	// leave every pre-existing tenant unrekeyed, and so turn the 50%
	// upsert failure into a 100% one. Cheap beats clever here.
	cursor, err := coll.Find(ctx, bson.M{})
	if err != nil {
		return errors.Wrap(err, "mongo find legacy rows")
	}
	defer cursor.Close(ctx)

	var rows []legacyRow
	if err := cursor.All(ctx, &rows); err != nil {
		return errors.Wrap(err, "mongo cursor all legacy rows")
	}

	for _, row := range rows {
		if id, ok := row.ID.(string); ok && id == row.StackName {
			continue // already keyed by stack name
		}
		if row.StackName == "" {
			// No natural key to rekey onto; leave it for an operator
			// rather than silently dropping a row.
			continue
		}
		// Delete FIRST. The unique index on stackName would otherwise
		// reject the insert branch of the upsert below, which is the
		// whole reason this function exists. The record is already in
		// memory, and the archive bucket's `_meta/` sidecars remain the
		// authoritative copy either way.
		if _, err := coll.DeleteOne(ctx, bson.M{"_id": row.ID}); err != nil {
			return errors.Wrapf(err, "mongo delete legacy row %q", row.StackName)
		}
		if _, err := coll.ReplaceOne(ctx, UpsertFilter(row.StackName), row.Record,
			options.Replace().SetUpsert(true)); err != nil {
			return errors.Wrapf(err, "mongo rekey row %q", row.StackName)
		}
	}

	return nil
}

// List returns every tenant row, soft-deleted included (for operator SPA
// visibility).
func (s *MongoStore) List(ctx context.Context) ([]Record, error) {
	cursor, err := s.collection.Find(ctx, bson.M{})
	if err != nil {
		return nil, errors.Wrap(err, "mongo find")
	}
	defer cursor.Close(ctx)
	var rows []Record
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, errors.Wrap(err, "mongo cursor all")
	}
	return rows, nil
}

// Get fetches a single tenant by stackName.
func (s *MongoStore) Get(ctx context.Context, stackName string) (Record, bool, error) {
	var r Record
	err := s.collection.FindOne(ctx, bson.M{"stackName": stackName}).Decode(&r)
	if err == mongo.ErrNoDocuments {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, errors.Wrap(err, "mongo find one")
	}
	return r, true, nil
}

// SoftDelete marks a tenant as removed without physically dropping the
// row — an operator can still see it in the SPA with a "removed" badge.
func (s *MongoStore) SoftDelete(ctx context.Context, stackName string) error {
	_, err := s.collection.UpdateOne(ctx,
		bson.M{"stackName": stackName},
		bson.M{"$set": bson.M{"softDeleted": true}})
	if err != nil {
		return errors.Wrap(err, "mongo update one")
	}
	return nil
}

// HardDelete physically drops the row. Idempotent — a missing row is
// not an error; the operator's intent (be rid of it) is satisfied.
func (s *MongoStore) HardDelete(ctx context.Context, stackName string) error {
	_, err := s.collection.DeleteOne(ctx, bson.M{"stackName": stackName})
	if err != nil {
		return errors.Wrap(err, "mongo delete one")
	}
	return nil
}

// Close disconnects the Mongo client.
func (s *MongoStore) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

// -------------------- In-memory implementation (tests) --------------------

// InMemoryStore is for tests — a map behind a mutex. No persistence.
type InMemoryStore struct {
	mu   sync.RWMutex
	rows map[string]Record
}

// NewInMemoryStore returns an empty store. Useful in handler tests that
// don't want a real Mongo.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{rows: map[string]Record{}}
}

func (s *InMemoryStore) Upsert(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[record.StackName] = record
	return nil
}

func (s *InMemoryStore) List(_ context.Context) ([]Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *InMemoryStore) Get(_ context.Context, stackName string) (Record, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rows[stackName]
	return r, ok, nil
}

func (s *InMemoryStore) SoftDelete(_ context.Context, stackName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[stackName]; ok {
		r.SoftDeleted = true
		s.rows[stackName] = r
	}
	return nil
}

func (s *InMemoryStore) HardDelete(_ context.Context, stackName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, stackName)
	return nil
}

func (s *InMemoryStore) Close(_ context.Context) error { return nil }
