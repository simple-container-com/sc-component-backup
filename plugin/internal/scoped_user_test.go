// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"strings"
	"testing"
)

func TestScopedUsername(t *testing.T) {
	cases := map[string]string{
		"forge-conductor":       "backup-forge-conductor",
		"":                      "backup-",
		strings.Repeat("x", 80): "backup-" + strings.Repeat("x", 57), // 64 total
	}
	for in, want := range cases {
		if got := ScopedUsername(in); got != want {
			t.Errorf("ScopedUsername(%q) = %q, want %q", in, got, want)
		}
		if len(ScopedUsername(in)) > 64 {
			t.Errorf("exceeded 64-char Atlas cap: %q", ScopedUsername(in))
		}
	}
}

func TestPopulateDatabasesFromRaw_Empty(t *testing.T) {
	for _, raw := range []map[string]any{nil, {}} {
		dbs, admin, err := populateDatabasesFromRaw(raw)
		if err != nil {
			t.Fatalf("unexpected err on empty raw: %v", err)
		}
		if dbs != nil || admin != nil {
			t.Errorf("empty raw: got dbs=%v admin=%v", dbs, admin)
		}
	}
}

func TestPopulateDatabasesFromRaw_HappyPath(t *testing.T) {
	raw := map[string]any{
		"databases": []any{
			map[string]any{"resource": "the-mongo", "driver": "mongodb"},
			map[string]any{"resource": "the-rds", "driver": "postgres"},
		},
		"atlasAdmin": map[string]any{
			"privateKey": "priv",
			"publicKey":  "pub",
			"projectId":  "proj1",
		},
	}
	dbs, admin, err := populateDatabasesFromRaw(raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(dbs) != 2 {
		t.Fatalf("dbs count: %d", len(dbs))
	}
	if dbs[0].Resource != "the-mongo" || dbs[0].Driver != "mongodb" {
		t.Errorf("db0: %+v", dbs[0])
	}
	if admin == nil || admin.PrivateKey != "priv" || admin.PublicKey != "pub" || admin.ProjectID != "proj1" {
		t.Errorf("admin: %+v", admin)
	}
}

func TestPopulateDatabasesFromRaw_MongoWithoutAdminFails(t *testing.T) {
	raw := map[string]any{
		"databases": []any{
			map[string]any{"resource": "the-mongo", "driver": "mongodb"},
		},
	}
	_, _, err := populateDatabasesFromRaw(raw)
	if err == nil || !strings.Contains(err.Error(), "atlasAdmin not set") {
		t.Fatalf("expected admin-required err, got %v", err)
	}
}

func TestPopulateDatabasesFromRaw_MissingFields(t *testing.T) {
	raw := map[string]any{
		"databases": []any{
			map[string]any{"driver": "mongodb"},
		},
	}
	_, _, err := populateDatabasesFromRaw(raw)
	if err == nil || !strings.Contains(err.Error(), "resource + driver required") {
		t.Fatalf("expected missing-resource err, got %v", err)
	}
}

func TestMatchConsumerToDatabase_HappyPath(t *testing.T) {
	deps := []byte(`{
		"the-mongo": {
			"mongo-uri": "mongodb://host:27017/",
			"database":  "forge_conductor"
		}
	}`)
	databases := []BackupDatabase{
		{Resource: "the-mongo", Driver: "mongodb"},
	}
	resName, dbName, matched, err := MatchConsumerToDatabase(deps, databases)
	if err != nil {
		t.Fatal(err)
	}
	if resName != "the-mongo" || dbName != "forge_conductor" || matched == nil {
		t.Errorf("got resName=%q dbName=%q matched=%v", resName, dbName, matched)
	}
}

func TestMatchConsumerToDatabase_NoConsumerUsage(t *testing.T) {
	// Consumer doesn't use any of the configured Databases → no match.
	deps := []byte(`{"the-rds": {"driver":"postgres"}}`)
	databases := []BackupDatabase{{Resource: "the-mongo", Driver: "mongodb"}}
	_, _, matched, err := MatchConsumerToDatabase(deps, databases)
	if err != nil {
		t.Fatal(err)
	}
	if matched != nil {
		t.Errorf("expected no match, got %v", matched)
	}
}

func TestMatchConsumerToDatabase_NoDatabaseField(t *testing.T) {
	// Sibling resource is there but no database-name key → no match.
	deps := []byte(`{"the-mongo": {"mongo-uri":"mongodb://h"}}`)
	databases := []BackupDatabase{{Resource: "the-mongo", Driver: "mongodb"}}
	_, _, matched, err := MatchConsumerToDatabase(deps, databases)
	if err != nil {
		t.Fatal(err)
	}
	if matched != nil {
		t.Errorf("expected no match, got %v", matched)
	}
}

func TestMatchConsumerToDatabase_AltKeyNames(t *testing.T) {
	// dbName / databaseName both accepted.
	for _, key := range []string{"dbName", "databaseName"} {
		deps := []byte(`{"the-mongo": {"` + key + `":"forge_conductor"}}`)
		databases := []BackupDatabase{{Resource: "the-mongo", Driver: "mongodb"}}
		_, dbName, matched, err := MatchConsumerToDatabase(deps, databases)
		if err != nil {
			t.Fatal(err)
		}
		if matched == nil || dbName != "forge_conductor" {
			t.Errorf("key=%q: got dbName=%q matched=%v", key, dbName, matched)
		}
	}
}

func TestMatchConsumerToDatabase_EmptyInputs(t *testing.T) {
	_, _, matched, err := MatchConsumerToDatabase(nil, []BackupDatabase{{Resource: "x", Driver: "mongodb"}})
	if err != nil {
		t.Fatal(err)
	}
	if matched != nil {
		t.Errorf("expected nil match on nil deps, got %v", matched)
	}
	_, _, matched, err = MatchConsumerToDatabase([]byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if matched != nil {
		t.Errorf("expected nil match on nil dbs, got %v", matched)
	}
}
