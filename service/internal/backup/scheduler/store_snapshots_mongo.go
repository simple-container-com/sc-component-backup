// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pkg/errors"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
)

// Default collection names for the snapshot store. DB name matches the
// run store's `sc_component_backup` so one deployment holds every
// backup component collection under one Mongo database.
const (
	defaultSnapshotsCollectionName = "db_snapshots"
	defaultSamplesCollectionName   = "object_samples"
)

// MongoSnapshotStore persists stats.DbSnapshot + stats.ObjectSample rows
// under two collections of a single Mongo database, plus delegates
// RunResultsForProject to an in-process RunStore (ProjectDbResults
// helper). Satisfies SnapshotStore.
//
// Shape follows store_mongo.go#MongoRunStore — one client, per-collection
// pointer, errors wrapped via pkg/errors, index creation gated by the
// constructor.
type MongoSnapshotStore struct {
	client    *mongo.Client
	db        *mongo.Database
	snapshots *mongo.Collection
	samples   *mongo.Collection
	runs      RunStore
}

// NewMongoSnapshotStore connects to `uri`, pings, and creates per-
// collection indexes. The context bounds both connect and index creates.
// `runs` is the RunStore the store projects DbResults off — pass the
// same one the scheduler writes to; the DbResult projection is
// single-sourced through it.
func NewMongoSnapshotStore(ctx context.Context, uri, dbName string, runs RunStore) (*MongoSnapshotStore, error) {
	if dbName == "" {
		dbName = defaultRunStoreDBName
	}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, errors.Wrap(err, "mongo connect")
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, errors.Wrap(err, "mongo ping")
	}
	db := client.Database(dbName)
	s := &MongoSnapshotStore{
		client:    client,
		db:        db,
		snapshots: db.Collection(defaultSnapshotsCollectionName),
		samples:   db.Collection(defaultSamplesCollectionName),
		runs:      runs,
	}
	if err := s.ensureIndexes(ctx); err != nil {
		_ = client.Disconnect(ctx)
		return nil, errors.Wrap(err, "mongo ensure indexes")
	}
	return s, nil
}

// ensureIndexes creates the per-collection indexes the stats
// aggregators + the SPA charts need. Idempotent.
//
// Chosen shapes:
//   - db_snapshots {project:1, ts:-1}             — per-project newest-first scan
//   - db_snapshots {project:1, database:1, ts:-1} — per-(project,database) scan
//   - object_samples {project:1, database:1, snapshotId:-1} — per-(project,database) sample pull
//
// Deliberately NOT created: a TTL index. Snapshots are kept forever
// per the Python reference — the `data_bytes/disk_bytes/records` history
// a chart renders depends on it, and shedding rows would silently
// truncate the "all-time" period.
func (s *MongoSnapshotStore) ensureIndexes(ctx context.Context) error {
	snapIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "project", Value: 1}, {Key: "ts", Value: -1}}},
		{Keys: bson.D{{Key: "project", Value: 1}, {Key: "database", Value: 1}, {Key: "ts", Value: -1}}},
	}
	if _, err := s.snapshots.Indexes().CreateMany(ctx, snapIdx); err != nil {
		return errors.Wrap(err, "db_snapshots indexes")
	}
	sampleIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "project", Value: 1}, {Key: "database", Value: 1}, {Key: "snapshotId", Value: -1}}},
	}
	if _, err := s.samples.Indexes().CreateMany(ctx, sampleIdx); err != nil {
		return errors.Wrap(err, "object_samples indexes")
	}
	return nil
}

// snapshotDoc decorates stats.DbSnapshot with bson tags Mongo understands.
// stats.DbSnapshot carries only json tags by design (it's the stats
// package's wire shape); the storage layer maps to bson here so the
// stats package never learns about Mongo.
type snapshotDoc struct {
	ID            int64     `bson:"_id"`
	Project       string    `bson:"project"`
	Engine        string    `bson:"engine,omitempty"`
	Database      string    `bson:"database"`
	RunID         *int64    `bson:"runId,omitempty"`
	Key           *string   `bson:"key,omitempty"`
	Ts            time.Time `bson:"ts"`
	Status        string    `bson:"status"`
	ArchiveBytes  int64     `bson:"archiveBytes"`
	DataBytes     *int64    `bson:"dataBytes,omitempty"`
	DiskBytes     *int64    `bson:"diskBytes,omitempty"`
	IndexBytes    *int64    `bson:"indexBytes,omitempty"`
	Records       *int64    `bson:"records,omitempty"`
	Approx        bool      `bson:"approx"`
	Partial       bool      `bson:"partial"`
	DumpSeconds   *float64  `bson:"dumpSeconds,omitempty"`
	UploadSeconds *float64  `bson:"uploadSeconds,omitempty"`
	Source        string    `bson:"source,omitempty"`
}

func toSnapshotDoc(s stats.DbSnapshot) snapshotDoc {
	return snapshotDoc{
		ID: s.ID, Project: s.Project, Engine: s.Engine, Database: s.Database,
		RunID: s.RunID, Key: s.Key, Ts: s.Ts, Status: s.Status,
		ArchiveBytes: s.ArchiveBytes,
		DataBytes:    s.DataBytes, DiskBytes: s.DiskBytes, IndexBytes: s.IndexBytes,
		Records: s.Records, Approx: s.Approx, Partial: s.Partial,
		DumpSeconds: s.DumpSeconds, UploadSeconds: s.UploadSeconds, Source: s.Source,
	}
}

func fromSnapshotDoc(d snapshotDoc) stats.DbSnapshot {
	return stats.DbSnapshot{
		ID: d.ID, Project: d.Project, Engine: d.Engine, Database: d.Database,
		RunID: d.RunID, Key: d.Key, Ts: d.Ts, Status: d.Status,
		ArchiveBytes: d.ArchiveBytes,
		DataBytes:    d.DataBytes, DiskBytes: d.DiskBytes, IndexBytes: d.IndexBytes,
		Records: d.Records, Approx: d.Approx, Partial: d.Partial,
		DumpSeconds: d.DumpSeconds, UploadSeconds: d.UploadSeconds, Source: d.Source,
	}
}

// sampleDoc is the on-wire object_samples row. Carries Project +
// Database alongside SnapshotID so the per-(project,database) read is
// a covered-index scan without a snapshot-join.
type sampleDoc struct {
	Project    string `bson:"project"`
	Database   string `bson:"database"`
	SnapshotID int64  `bson:"snapshotId"`
	Name       string `bson:"name"`
	Kind       string `bson:"kind"`
	Records    *int64 `bson:"records,omitempty"`
	DataBytes  *int64 `bson:"dataBytes,omitempty"`
	DiskBytes  *int64 `bson:"diskBytes,omitempty"`
	IndexBytes *int64 `bson:"indexBytes,omitempty"`
}

func (s *MongoSnapshotStore) PutDbSnapshot(ctx context.Context, snap stats.DbSnapshot) error {
	doc := toSnapshotDoc(snap)
	filter := bson.M{"_id": doc.ID}
	opts := options.Replace().SetUpsert(true)
	if _, err := s.snapshots.ReplaceOne(ctx, filter, doc, opts); err != nil {
		return errors.Wrap(err, "mongo replace db_snapshots")
	}
	return nil
}

func (s *MongoSnapshotStore) PutObjectSamples(ctx context.Context, project, database, key string, snapshotID int64, samples []stats.ObjectSample) error {
	// Drop any prior samples for this (project, database, snapshotId)
	// pair so a replay replaces instead of duplicating. key is not
	// indexed — snapshotId is the grouping handle.
	if _, err := s.samples.DeleteMany(ctx, bson.M{
		"project":    project,
		"database":   database,
		"snapshotId": snapshotID,
	}); err != nil {
		return errors.Wrap(err, "mongo delete prior samples")
	}
	if len(samples) == 0 {
		return nil
	}
	docs := make([]any, 0, len(samples))
	for _, so := range samples {
		docs = append(docs, sampleDoc{
			Project: project, Database: database, SnapshotID: snapshotID,
			Name: so.Name, Kind: so.Kind, Records: so.Records,
			DataBytes: so.DataBytes, DiskBytes: so.DiskBytes, IndexBytes: so.IndexBytes,
		})
	}
	if _, err := s.samples.InsertMany(ctx, docs); err != nil {
		return errors.Wrap(err, "mongo insert object_samples")
	}
	return nil
}

func (s *MongoSnapshotStore) SnapshotsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbSnapshot, error) {
	if project == "" {
		return []stats.DbSnapshot{}, nil
	}
	filter := bson.M{"project": project}
	if !since.IsZero() {
		filter["ts"] = bson.M{"$gte": since}
	}
	opts := options.Find().SetSort(bson.D{{Key: "ts", Value: -1}})
	cursor, err := s.snapshots.Find(ctx, filter, opts)
	if err != nil {
		return nil, errors.Wrap(err, "mongo find db_snapshots")
	}
	defer cursor.Close(ctx)
	var docs []snapshotDoc
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, errors.Wrap(err, "mongo cursor all db_snapshots")
	}
	out := make([]stats.DbSnapshot, 0, len(docs))
	for _, d := range docs {
		out = append(out, fromSnapshotDoc(d))
	}
	return out, nil
}

func (s *MongoSnapshotStore) ObjectSamplesForProject(ctx context.Context, project, database string, since time.Time) ([]stats.ObjectSample, error) {
	if project == "" || database == "" {
		return []stats.ObjectSample{}, nil
	}
	// Scope by parent-snapshot Ts via a small two-step: fetch the
	// qualifying snapshot IDs, then pull samples with snapshotId $in.
	// The compound index on (project, database, snapshotId) covers
	// the second step; the first is a covered scan on the snapshots
	// index.
	snapFilter := bson.M{"project": project, "database": database}
	if !since.IsZero() {
		snapFilter["ts"] = bson.M{"$gte": since}
	}
	snapOpts := options.Find().
		SetSort(bson.D{{Key: "ts", Value: -1}}).
		SetProjection(bson.M{"_id": 1})
	cursor, err := s.snapshots.Find(ctx, snapFilter, snapOpts)
	if err != nil {
		return nil, errors.Wrap(err, "mongo find snapshot ids")
	}
	defer cursor.Close(ctx)
	var ids []struct {
		ID int64 `bson:"_id"`
	}
	if err := cursor.All(ctx, &ids); err != nil {
		return nil, errors.Wrap(err, "mongo cursor all snapshot ids")
	}
	if len(ids) == 0 {
		return []stats.ObjectSample{}, nil
	}
	snapIDs := make([]int64, 0, len(ids))
	order := make(map[int64]int, len(ids))
	for i, row := range ids {
		snapIDs = append(snapIDs, row.ID)
		order[row.ID] = i // newest-first position
	}
	samplesCursor, err := s.samples.Find(ctx, bson.M{
		"project":    project,
		"database":   database,
		"snapshotId": bson.M{"$in": snapIDs},
	})
	if err != nil {
		return nil, errors.Wrap(err, "mongo find object_samples")
	}
	defer samplesCursor.Close(ctx)
	var sdocs []sampleDoc
	if err := samplesCursor.All(ctx, &sdocs); err != nil {
		return nil, errors.Wrap(err, "mongo cursor all object_samples")
	}
	// Preserve newest-snapshot-first order.
	out := make([]stats.ObjectSample, len(sdocs))
	// Sort via the snapshot-order map.
	type ord struct {
		s stats.ObjectSample
		o int
	}
	tmp := make([]ord, 0, len(sdocs))
	for _, d := range sdocs {
		tmp = append(tmp, ord{
			s: stats.ObjectSample{
				SnapshotID: d.SnapshotID, Name: d.Name, Kind: d.Kind,
				Records: d.Records, DataBytes: d.DataBytes,
				DiskBytes: d.DiskBytes, IndexBytes: d.IndexBytes,
			},
			o: order[d.SnapshotID],
		})
	}
	// Stable sort by snapshot order (newest-first).
	for i := 1; i < len(tmp); i++ {
		for j := i; j > 0 && tmp[j].o < tmp[j-1].o; j-- {
			tmp[j], tmp[j-1] = tmp[j-1], tmp[j]
		}
	}
	for i, t := range tmp {
		out[i] = t.s
	}
	return out, nil
}

func (s *MongoSnapshotStore) RunResultsForProject(ctx context.Context, project string, since time.Time) ([]stats.DbResult, error) {
	if s.runs == nil {
		return []stats.DbResult{}, nil
	}
	runs, err := s.runs.ListBackupsByTenant(ctx, project)
	if err != nil {
		return nil, err
	}
	return ProjectDbResults(runs, since), nil
}

func (s *MongoSnapshotStore) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}
