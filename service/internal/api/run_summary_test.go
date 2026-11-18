// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// TestBackupRunToSummary_SucceededHasDatabases — a succeeded run must
// emit a non-nil Databases slice with exactly one synthesized row;
// without it the SPA's DbTable spreads undefined and crashes the
// history tab with "e is not iterable".
func TestBackupRunToSummary_SucceededHasDatabases(t *testing.T) {
	started := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	finished := started.Add(4 * time.Second)
	rs := backupRunToSummary(backup.BackupRun{
		ID:           "c44af8d7-8632-44c4-bdf2-a0a22cef2b4c",
		TenantStack:  "smoke-tier4--staging",
		ResourceName: "mydb",
		Driver:       "mongodb",
		StartedAt:    started,
		FinishedAt:   finished,
		Status:       backup.StatusSucceeded,
		S3Path:       "smoke-tier4--staging/2026/11/18/foo.archive.gz",
		SizeBytes:    116,
	})
	if rs.Databases == nil {
		t.Fatal("Databases is nil; SPA DbTable will crash")
	}
	if len(rs.Databases) != 1 {
		t.Fatalf("want 1 synthesized db row, got %d", len(rs.Databases))
	}
	db := rs.Databases[0]
	if db.Database != "mydb" {
		t.Errorf("Database: got %q, want mydb", db.Database)
	}
	if db.Status != "ok" {
		t.Errorf("Status: got %q, want ok", db.Status)
	}
	if db.Bytes != 116 {
		t.Errorf("Bytes: got %d, want 116", db.Bytes)
	}
	if db.Seconds != 4 {
		t.Errorf("Seconds: got %d, want 4", db.Seconds)
	}
	if db.Attempts != 1 {
		t.Errorf("Attempts: got %d, want 1", db.Attempts)
	}
	if db.Error != nil {
		t.Errorf("Error: got %v, want nil", *db.Error)
	}
	if db.Key == nil || *db.Key != "smoke-tier4--staging/2026/11/18/foo.archive.gz" {
		t.Errorf("Key: got %v, want the s3 path", db.Key)
	}
}

// TestBackupRunToSummary_FailedCarriesError — a failed run surfaces
// the run error into the per-database row so the SPA can render it
// inline under the collapsed row.
func TestBackupRunToSummary_FailedCarriesError(t *testing.T) {
	rs := backupRunToSummary(backup.BackupRun{
		ID:           "r1",
		ResourceName: "mydb",
		Driver:       "postgres",
		StartedAt:    time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
		FinishedAt:   time.Date(2026, 11, 18, 10, 0, 7, 0, time.UTC),
		Status:       backup.StatusFailed,
		Error:        "pg_dump exited 1",
	})
	if len(rs.Databases) != 1 {
		t.Fatalf("want 1 synthesized db row, got %d", len(rs.Databases))
	}
	db := rs.Databases[0]
	if db.Status != "failed" {
		t.Errorf("Status: got %q, want failed", db.Status)
	}
	if db.Error == nil || *db.Error != "pg_dump exited 1" {
		t.Errorf("Error: got %v, want pg_dump exited 1", db.Error)
	}
	if rs.DbFailed != 1 || rs.DbOK != 0 {
		t.Errorf("counts: ok=%d, failed=%d; want 0, 1", rs.DbOK, rs.DbFailed)
	}
}

// TestBackupRunToSummary_RunningHasZeroDuration — a still-in-flight
// run (FinishedAt zero) must not synthesize a negative duration from
// the zero time.
func TestBackupRunToSummary_RunningHasZeroDuration(t *testing.T) {
	rs := backupRunToSummary(backup.BackupRun{
		ID:           "r1",
		ResourceName: "mydb",
		Driver:       "mongodb",
		StartedAt:    time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
		Status:       backup.StatusRunning,
	})
	if len(rs.Databases) != 1 {
		t.Fatalf("want 1 db row, got %d", len(rs.Databases))
	}
	db := rs.Databases[0]
	if db.Status != "running" {
		t.Errorf("Status: got %q, want running", db.Status)
	}
	if db.Seconds != 0 {
		t.Errorf("Seconds: got %d, want 0", db.Seconds)
	}
	if rs.FinishedAt != nil {
		t.Errorf("FinishedAt: want nil, got %v", *rs.FinishedAt)
	}
}

// TestBackupRunToSummary_DatabasesSerializesAsArray — the critical
// SPA contract: the JSON must carry a `databases` array (not null,
// not absent). Round-trip through json.Marshal + json.Unmarshal into
// a map to assert the shape.
func TestBackupRunToSummary_DatabasesSerializesAsArray(t *testing.T) {
	rs := backupRunToSummary(backup.BackupRun{
		ID:         "r1",
		StartedAt:  time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 11, 18, 10, 0, 1, 0, time.UTC),
		Status:     backup.StatusSucceeded,
	})
	b, err := json.Marshal(rs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dbs, ok := out["databases"].([]any)
	if !ok {
		t.Fatalf("databases must be a JSON array, got %T (value: %v)", out["databases"], out["databases"])
	}
	if len(dbs) != 1 {
		t.Errorf("want 1 db row in JSON, got %d", len(dbs))
	}
}
