// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package goembedbadmin holds the admin-only HTTP surface the
// go-embedb cutover needs — the migration endpoint, the
// backend-selection Prometheus gauge, and the shared metrics
// handler mount.
//
// Kept separate from the backup service's domain packages so a
// deploy without USE_GOEMBEDB or ADMIN_MIGRATION_TOKEN compiles
// identically and no manager picks up a dependency on ferretdb /
// prometheus.
//
// ## Blob carve-out
//
// This package migrates METADATA only. The archive blobs (the
// `.tar.gz`, `.sql.gz`, `.gz` files produced by mongodump /
// pg_dump / mariadb-dump drivers) live under the operator's chosen
// archive store (S3 / GCS / filesystem) and are NEVER touched by
// the migration endpoint. See collections.go#Collections for the
// closed set of metadata namespaces that ARE copied.
package goembedbadmin

// Database is the single Mongo database the backup service persists
// all metadata to. All six domain packages use the same name — kept
// in a single constant here so the migration endpoint knows where to
// read the source and write the sink.
//
// Cross-pinned in migration_test.go against the per-package
// `defaultDBName` / `settingsDBName` / `alertsDBName` constants to
// catch a drift in either place.
const Database = "sc_component_backup"

// Collections enumerates every Mongo collection the backup service
// writes metadata rows to. Centralised here so the migration
// endpoint has one source of truth for what to copy and the
// per-collection count report is a closed set the operator can
// diff against a source cluster.
//
// Order is mostly dispatch-friendly (tenants first because every
// other row references a tenant key; alerts last because they are
// synthetic) but nothing functional depends on it — the report is
// sorted alphabetically before rendering.
var Collections = []string{
	CollectionTenants,
	CollectionProjects,
	CollectionBackupRuns,
	CollectionRestoreRuns,
	CollectionSnapshots,
	CollectionObjectSamples,
	CollectionSettings,
	CollectionAlertsDismissals,
}

// Collection names. Must match the per-package default*CollectionName
// / settingsCollectionName / alertsCollectionName constants in the
// domain packages. A drift here silently drops that class from the
// migration + the metrics gauge — migration_test.go cross-pins each
// constant to its domain package.
const (
	// CollectionTenants — tenant.MongoStore. One row per
	// `uses: [my-backup]` consumer; registered by the plugin.
	CollectionTenants = "tenants"

	// CollectionProjects — project.MongoStore. One row per SPA-defined
	// backup project (connection URI + schedule + retention).
	CollectionProjects = "projects"

	// CollectionBackupRuns — scheduler.MongoRunStore.backups. One row
	// per backup attempt (queued / running / succeeded / failed /
	// cancelled). Archive-path points back to the S3 / GCS / FS object.
	CollectionBackupRuns = "backup_runs"

	// CollectionRestoreRuns — scheduler.MongoRunStore.restores. One row
	// per restore attempt. Same statuses as backup_runs.
	CollectionRestoreRuns = "restore_runs"

	// CollectionSnapshots — scheduler.MongoSnapshotStore.snapshots. One
	// row per project per captured snapshot pointing at a
	// dump-metadata record (sizes, counts).
	CollectionSnapshots = "db_snapshots"

	// CollectionObjectSamples — scheduler.MongoSnapshotStore.samples.
	// Per-object size / count samples feeding the SPA's stats charts.
	CollectionObjectSamples = "object_samples"

	// CollectionSettings — notify.MongoStore (telegram bot token +
	// chat id). Encrypted at rest once the Phase-7 shape ships; stays
	// a single row today.
	CollectionSettings = "settings"

	// CollectionAlertsDismissals — alerts.MongoStore. Per-id
	// dismissals + the "clear all" high-water mark.
	CollectionAlertsDismissals = "alerts_dismissals"
)
