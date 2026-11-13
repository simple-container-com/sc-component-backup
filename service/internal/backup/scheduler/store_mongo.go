// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pkg/errors"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// Default collection + DB names for the run store. The DB name matches
// the tenant store's (`sc_component_backup`), so one Mongo deployment
// holds both backup component collections under one namespace.
const (
	defaultRunStoreDBName        = "sc_component_backup"
	defaultBackupCollectionName  = "backup_runs"
	defaultRestoreCollectionName = "restore_runs"
)

// MongoRunStore persists BackupRun + RestoreRun rows under two
// collections of a single Mongo database. Satisfies RunStore.
//
// Shape follows tenant/store.go#MongoStore — one client, one
// per-collection pointer, errors wrapped via pkg/errors, index creation
// gated by the constructor so a handler can assume the collections are
// ready at first write.
type MongoRunStore struct {
	client   *mongo.Client
	db       *mongo.Database
	backups  *mongo.Collection
	restores *mongo.Collection
}

// NewMongoRunStore connects to the given Mongo URI, pings, and creates
// the per-collection indexes needed by the handler + scheduler query
// patterns. The context bounds both the connect and the index creates.
func NewMongoRunStore(ctx context.Context, uri, dbName string) (*MongoRunStore, error) {
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
	s := &MongoRunStore{
		client:   client,
		db:       db,
		backups:  db.Collection(defaultBackupCollectionName),
		restores: db.Collection(defaultRestoreCollectionName),
	}
	if err := s.ensureIndexes(ctx); err != nil {
		_ = client.Disconnect(ctx)
		return nil, errors.Wrap(err, "mongo ensure indexes")
	}
	return s, nil
}

// ensureIndexes creates the per-collection indexes the handler + the
// scheduler need. Idempotent — repeated calls are no-ops, so a service
// restart against an existing database is safe.
//
// Chosen shapes:
//   - backups {tenantStack:1, startedAt:-1} — the per-tenant "newest
//     first" listing the SPA Project page drives (api.runs).
//   - backups {status:1}                   — filter running/pending
//     rows the scheduler tick needs to resume on restart.
//   - restores {tenantStack:1, startedAt:-1} — same shape, restore
//     history on the Project page.
//
// Deliberately NOT created:
//   - A TTL index on finishedAt. Retention is driven by the GFS
//     rotation (`retention/` package 943218b) over the S3 archive
//     keys, NOT over the Mongo run rows. A TTL on finishedAt would
//     drop the audit trail of what used to exist, leaving an orphaned
//     S3 object with no DB row — the exact state the two-phase
//     delete comment in model.go warns against. A future slice can
//     add a grooming job that reads the retention output and prunes
//     Mongo rows whose `S3Path` no longer exists.
func (s *MongoRunStore) ensureIndexes(ctx context.Context) error {
	backupIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenantStack", Value: 1}, {Key: "startedAt", Value: -1}}},
		{Keys: bson.D{{Key: "status", Value: 1}}},
	}
	if _, err := s.backups.Indexes().CreateMany(ctx, backupIdx); err != nil {
		return errors.Wrap(err, "backup_runs indexes")
	}
	restoreIdx := []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenantStack", Value: 1}, {Key: "startedAt", Value: -1}}},
	}
	if _, err := s.restores.Indexes().CreateMany(ctx, restoreIdx); err != nil {
		return errors.Wrap(err, "restore_runs indexes")
	}
	return nil
}

// PutBackup inserts a fresh BackupRun row or replaces an existing one
// by `_id`. Scheduler calls this at run start; UpdateBackup is a
// synonym the scheduler uses on status transitions, kept separate on
// the interface to document intent.
func (s *MongoRunStore) PutBackup(ctx context.Context, run backup.BackupRun) error {
	filter := bson.M{"_id": run.ID}
	opts := options.Replace().SetUpsert(true)
	if _, err := s.backups.ReplaceOne(ctx, filter, run, opts); err != nil {
		return errors.Wrap(err, "mongo replace backup_runs")
	}
	return nil
}

// UpdateBackup is the status-transition form. Same wire as PutBackup
// in this impl (upsert-by-id); separate on the interface so callers
// can write intent-expressive code.
func (s *MongoRunStore) UpdateBackup(ctx context.Context, run backup.BackupRun) error {
	return s.PutBackup(ctx, run)
}

// GetBackup fetches one BackupRun by its `_id`. Returns a wrapped
// not-found error when the row is absent — matches InMemoryRunStore's
// shape so handlers can `%v`-compare consistently.
func (s *MongoRunStore) GetBackup(ctx context.Context, id string) (backup.BackupRun, error) {
	var r backup.BackupRun
	err := s.backups.FindOne(ctx, bson.M{"_id": id}).Decode(&r)
	if err == mongo.ErrNoDocuments {
		return backup.BackupRun{}, errors.Errorf("backup-run %q: not found", id)
	}
	if err != nil {
		return backup.BackupRun{}, errors.Wrap(err, "mongo find backup_runs")
	}
	return r, nil
}

// ListBackupsByTenant returns every BackupRun row for `tenantStack`,
// newest first by StartedAt. The index on
// {tenantStack:1, startedAt:-1} makes this a covered-index range scan
// — no in-memory sort. Empty tenantStack returns an empty slice (not
// every row, for safety).
func (s *MongoRunStore) ListBackupsByTenant(ctx context.Context, tenantStack string) ([]backup.BackupRun, error) {
	if tenantStack == "" {
		return []backup.BackupRun{}, nil
	}
	opts := options.Find().SetSort(bson.D{{Key: "startedAt", Value: -1}})
	cursor, err := s.backups.Find(ctx, bson.M{"tenantStack": tenantStack}, opts)
	if err != nil {
		return nil, errors.Wrap(err, "mongo find backup_runs by tenant")
	}
	defer cursor.Close(ctx)
	var rows []backup.BackupRun
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, errors.Wrap(err, "mongo cursor all backup_runs")
	}
	return rows, nil
}

// PutRestore inserts a fresh RestoreRun row or replaces an existing
// one by `_id`. Mirrors PutBackup.
func (s *MongoRunStore) PutRestore(ctx context.Context, run backup.RestoreRun) error {
	filter := bson.M{"_id": run.ID}
	opts := options.Replace().SetUpsert(true)
	if _, err := s.restores.ReplaceOne(ctx, filter, run, opts); err != nil {
		return errors.Wrap(err, "mongo replace restore_runs")
	}
	return nil
}

// UpdateRestore is the status-transition form — same wire as
// PutRestore. Kept separate on the interface to document intent.
func (s *MongoRunStore) UpdateRestore(ctx context.Context, run backup.RestoreRun) error {
	return s.PutRestore(ctx, run)
}

// GetRestore fetches one RestoreRun by `_id`.
func (s *MongoRunStore) GetRestore(ctx context.Context, id string) (backup.RestoreRun, error) {
	var r backup.RestoreRun
	err := s.restores.FindOne(ctx, bson.M{"_id": id}).Decode(&r)
	if err == mongo.ErrNoDocuments {
		return backup.RestoreRun{}, errors.Errorf("restore-run %q: not found", id)
	}
	if err != nil {
		return backup.RestoreRun{}, errors.Wrap(err, "mongo find restore_runs")
	}
	return r, nil
}

// Close disconnects the underlying Mongo client. Service's main calls
// this during graceful shutdown; a crashed process is covered by the
// driver's heartbeat teardown.
func (s *MongoRunStore) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}
