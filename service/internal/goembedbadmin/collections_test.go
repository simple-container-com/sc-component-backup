// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package goembedbadmin_test

import (
	"testing"

	"github.com/simple-container-com/sc-component-backup/service/internal/goembedbadmin"
)

// TestCollectionNames_StableSet pins the Collections set so a future
// refactor cannot silently drop a class from the migration without
// a test failure. Values are duplicated by design — a drift in one
// side is caught here without the test reaching into each package's
// private `defaultCollectionName` constants (which are unexported).
//
// Cross-check the per-package constants when touching this list:
//
//   - internal/tenant/store.go                       defaultCollectionName = "tenants"
//   - internal/backup/project/store_mongo.go         defaultCollectionName = "projects"
//   - internal/backup/scheduler/store_mongo.go       defaultBackupCollectionName  = "backup_runs"
//   - internal/backup/scheduler/store_mongo.go       defaultRestoreCollectionName = "restore_runs"
//   - internal/backup/scheduler/store_snapshots_mongo.go  defaultSnapshotsCollectionName = "db_snapshots"
//   - internal/backup/scheduler/store_snapshots_mongo.go  defaultSamplesCollectionName   = "object_samples"
//   - internal/backup/notify/store.go                settingsCollectionName = "settings"
//   - internal/backup/alerts/store_mongo.go          alertsCollectionName   = "alerts_dismissals"
func TestCollectionNames_StableSet(t *testing.T) {
	want := map[string]bool{
		"tenants":           true,
		"projects":          true,
		"backup_runs":       true,
		"restore_runs":      true,
		"db_snapshots":      true,
		"object_samples":    true,
		"settings":          true,
		"alerts_dismissals": true,
	}
	got := map[string]bool{}
	for _, name := range goembedbadmin.Collections {
		if got[name] {
			t.Fatalf("Collections lists %q twice", name)
		}
		got[name] = true
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("Collections missing %q", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Fatalf("Collections lists unexpected %q (did a domain package rename?)", name)
		}
	}

	if goembedbadmin.Database != "sc_component_backup" {
		t.Fatalf("Database = %q, want sc_component_backup", goembedbadmin.Database)
	}
}
