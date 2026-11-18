// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package alerts

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pkg/errors"
)

// MongoDismissals persists dismissals in one Mongo collection with
// two kinds of rows:
//   - Per-ID row:  {_id: <int64>, kind: "id"}
//   - Before-row:  {_id: "@before", kind: "before", at: <time>}
//
// Two rows in one collection (not two collections) because they're
// read together — one State() call reads every row in the collection
// via Find({}). The `@before` id shape is deliberately not a legal
// FNV hash value (the `@` prefix makes it a Mongo string, not an
// int), so there's no collision risk with per-id rows.
type MongoDismissals struct {
	client     *mongo.Client
	collection *mongo.Collection
}

const (
	alertsDBName         = "sc_component_backup"
	alertsCollectionName = "alerts_dismissals"
	beforeDocID          = "@before"
)

// NewMongoDismissals opens a Mongo client, pings, and returns a
// ready-to-use store. Shares the DB name with tenant + project stores.
func NewMongoDismissals(ctx context.Context, uri string) (*MongoDismissals, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, errors.Wrap(err, "mongo connect")
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, errors.Wrap(err, "mongo ping")
	}
	coll := client.Database(alertsDBName).Collection(alertsCollectionName)
	return &MongoDismissals{client: client, collection: coll}, nil
}

// Close releases the Mongo client.
func (s *MongoDismissals) Close(ctx context.Context) error {
	if s.client == nil {
		return nil
	}
	return s.client.Disconnect(ctx)
}

// Dismiss upserts a per-id row. Idempotent — $setOnInsert guards
// against overwriting a prior row.
func (s *MongoDismissals) Dismiss(ctx context.Context, id int64) error {
	_, err := s.collection.UpdateOne(
		ctx,
		bson.M{"_id": id},
		bson.M{"$setOnInsert": bson.M{"_id": id, "kind": "id", "at": time.Now().UTC()}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return errors.Wrap(err, "mongo dismiss upsert")
	}
	return nil
}

// DismissAll advances the before-cutoff, high-watermark style.
// Uses $max so a concurrent writer with an older timestamp cannot
// roll it back.
func (s *MongoDismissals) DismissAll(ctx context.Context, before time.Time) error {
	_, err := s.collection.UpdateOne(
		ctx,
		bson.M{"_id": beforeDocID},
		bson.M{
			"$setOnInsert": bson.M{"_id": beforeDocID, "kind": "before"},
			"$max":         bson.M{"at": before.UTC()},
		},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return errors.Wrap(err, "mongo dismiss-all upsert")
	}
	return nil
}

// State reads every row in one Find + decode pass. The collection
// stays small (one row per dismissed alert + one before-row), so a
// per-request full scan is cheap — each Overview request runs this
// once and filters in-memory.
func (s *MongoDismissals) State(ctx context.Context) (State, error) {
	cur, err := s.collection.Find(ctx, bson.M{})
	if err != nil {
		return State{}, errors.Wrap(err, "mongo state find")
	}
	defer cur.Close(ctx)
	st := State{IDs: map[int64]struct{}{}}
	for cur.Next(ctx) {
		var row struct {
			ID   any       `bson:"_id"`
			Kind string    `bson:"kind"`
			At   time.Time `bson:"at"`
		}
		if err := cur.Decode(&row); err != nil {
			return State{}, errors.Wrap(err, "mongo state decode")
		}
		switch row.Kind {
		case "before":
			st.Before = row.At.UTC()
		case "id":
			// _id may deserialise as int32 or int64 depending on
			// the driver / value magnitude. Accept both so a row
			// written on a fresh install doesn't trip the decode
			// on an older one.
			switch v := row.ID.(type) {
			case int64:
				st.IDs[v] = struct{}{}
			case int32:
				st.IDs[int64(v)] = struct{}{}
			}
		}
	}
	if err := cur.Err(); err != nil {
		return State{}, errors.Wrap(err, "mongo state cursor")
	}
	return st, nil
}
