// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package project

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pkg/errors"
)

// Collection name constants — kept exported so operator-side tooling can
// grep for them. Database name is shared with the tenant.Store
// (`sc_component_backup`) — the two collections live in the same Mongo
// DB so backup smokes can scope a single BACKUP_DB_NAME env.
const (
	defaultDBName         = "sc_component_backup"
	defaultCollectionName = "projects"
)

// MongoStore persists Config rows in a dedicated collection. Lifecycle
// mirrors tenant.MongoStore — connect, Ping, create indexes, Close on
// shutdown.
type MongoStore struct {
	client     *mongo.Client
	collection *mongo.Collection
}

// NewMongoStore opens a Mongo client, pings the server, and creates the
// _id-is-Name index implicitly plus an auxiliary softDeleted index to
// keep List queries cheap. Blocks until the ping returns (bounded by
// ctx deadline).
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
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "softDeleted", Value: 1}, {Key: "_id", Value: 1}},
	}); err != nil {
		_ = client.Disconnect(ctx)
		return nil, errors.Wrap(err, "mongo index create")
	}
	return &MongoStore{client: client, collection: coll}, nil
}

func (s *MongoStore) Create(ctx context.Context, cfg Config) (Config, error) {
	now := time.Now().UTC()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now
	cfg.SoftDeleted = false
	if cfg.Revision == "" {
		cfg.Revision = NewRevision()
	}
	// Insert atomically; if a non-soft-deleted row already exists,
	// upgrade to ErrConflict. Insert fails only on duplicate _id.
	if _, err := s.collection.InsertOne(ctx, cfg); err != nil {
		// Duplicate _id could also mean a soft-deleted row is squatting
		// on the name — a second attempt with ReplaceOne under a
		// softDeleted:true filter resurrects it with a fresh revision.
		if mongo.IsDuplicateKeyError(err) {
			resurrected, resErr := s.resurrect(ctx, cfg)
			if resErr == nil {
				return resurrected, nil
			}
			// Fall through to conflict if resurrect found a live row.
			return Config{}, ErrConflict
		}
		return Config{}, errors.Wrap(err, "mongo insert")
	}
	return cfg, nil
}

// resurrect replaces a soft-deleted row at `cfg.Name` with the new
// config. Returns ErrConflict if the existing row is NOT soft-deleted.
func (s *MongoStore) resurrect(ctx context.Context, cfg Config) (Config, error) {
	filter := bson.M{"_id": cfg.Name, "softDeleted": true}
	opts := options.Replace().SetUpsert(false)
	res, err := s.collection.ReplaceOne(ctx, filter, cfg, opts)
	if err != nil {
		return Config{}, errors.Wrap(err, "mongo replace")
	}
	if res.MatchedCount == 0 {
		return Config{}, ErrConflict
	}
	return cfg, nil
}

func (s *MongoStore) Get(ctx context.Context, name string) (Config, bool, error) {
	var r Config
	err := s.collection.FindOne(ctx, bson.M{"_id": name}).Decode(&r)
	if err == mongo.ErrNoDocuments {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, errors.Wrap(err, "mongo find one")
	}
	if r.SoftDeleted {
		return Config{}, false, nil
	}
	return r, true, nil
}

func (s *MongoStore) Update(ctx context.Context, cfg Config) (Config, error) {
	// Optimistic concurrency: filter on current revision; if no match,
	// either the row is gone or the revision is stale. A follow-up
	// Get-by-_id disambiguates.
	newRev := NewRevision()
	filter := bson.M{"_id": cfg.Name, "revision": cfg.Revision, "softDeleted": bson.M{"$ne": true}}
	update := bson.M{"$set": bson.M{
		"engine":            cfg.Engine,
		"databases":         cfg.Databases,
		"interval":          cfg.Interval,
		"timeout":           cfg.Timeout,
		"retention":         cfg.Retention,
		"retries":           cfg.Retries,
		"shrinkWarnPercent": cfg.ShrinkWarnPercent,
		"dumpArgs":          cfg.DumpArgs,
		"globals":           cfg.Globals,
		"startAt":           cfg.StartAt,
		"updatedAt":         time.Now().UTC(),
		"revision":          newRev,
	}}
	res, err := s.collection.UpdateOne(ctx, filter, update)
	if err != nil {
		return Config{}, errors.Wrap(err, "mongo update one")
	}
	if res.MatchedCount == 0 {
		// Disambiguate between "no such row" and "revision mismatch".
		_, found, _ := s.Get(ctx, cfg.Name)
		if !found {
			return Config{}, ErrNotFound
		}
		return Config{}, ErrRevisionMismatch
	}
	cfg.Revision = newRev
	return s.getFresh(ctx, cfg.Name)
}

// getFresh re-reads a row by _id — used after an Update to return the
// post-write value with server-assigned timestamps.
func (s *MongoStore) getFresh(ctx context.Context, name string) (Config, error) {
	r, _, err := s.Get(ctx, name)
	return r, err
}

func (s *MongoStore) SetSecret(ctx context.Context, name, secretURI string) error {
	filter := bson.M{"_id": name, "softDeleted": bson.M{"$ne": true}}
	update := bson.M{"$set": bson.M{"secretUri": secretURI, "updatedAt": time.Now().UTC()}}
	res, err := s.collection.UpdateOne(ctx, filter, update)
	if err != nil {
		return errors.Wrap(err, "mongo update one")
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) SoftDelete(ctx context.Context, name, revision string) error {
	filter := bson.M{"_id": name, "revision": revision, "softDeleted": bson.M{"$ne": true}}
	update := bson.M{"$set": bson.M{"softDeleted": true, "updatedAt": time.Now().UTC()}}
	res, err := s.collection.UpdateOne(ctx, filter, update)
	if err != nil {
		return errors.Wrap(err, "mongo update one")
	}
	if res.MatchedCount == 0 {
		_, found, _ := s.Get(ctx, name)
		if !found {
			return ErrNotFound
		}
		return ErrRevisionMismatch
	}
	return nil
}

func (s *MongoStore) List(ctx context.Context) ([]Config, error) {
	cursor, err := s.collection.Find(ctx, bson.M{"softDeleted": bson.M{"$ne": true}})
	if err != nil {
		return nil, errors.Wrap(err, "mongo find")
	}
	defer cursor.Close(ctx)
	var rows []Config
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, errors.Wrap(err, "mongo cursor all")
	}
	return rows, nil
}

func (s *MongoStore) Close(ctx context.Context) error { return s.client.Disconnect(ctx) }
