// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package tenant

import (
	"context"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pkg/errors"
)

// Store is the tenant repository interface. Two implementations: MongoStore
// (production) and InMemoryStore (tests).
type Store interface {
	Upsert(ctx context.Context, record Record) error
	List(ctx context.Context) ([]Record, error)
	Get(ctx context.Context, stackName string) (Record, bool, error)
	SoftDelete(ctx context.Context, stackName string) error
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
	// at the DB level. Upserts use stackName as the match key.
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "stackName", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		_ = client.Disconnect(ctx)
		return nil, errors.Wrap(err, "mongo index create")
	}
	return &MongoStore{client: client, collection: coll}, nil
}

// Upsert inserts a new tenant row or replaces an existing one by stackName.
func (s *MongoStore) Upsert(ctx context.Context, record Record) error {
	filter := bson.M{"stackName": record.StackName}
	opts := options.Replace().SetUpsert(true)
	if _, err := s.collection.ReplaceOne(ctx, filter, record, opts); err != nil {
		return errors.Wrap(err, "mongo replace")
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

func (s *InMemoryStore) Close(_ context.Context) error { return nil }
