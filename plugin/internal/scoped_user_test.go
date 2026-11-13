// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"strings"
	"testing"

	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
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

// mkDeps builds a `ResourcesOutputs` fixture — the shape plugin bus
// unmarshals a `ComputeProcessRequest.DependencyOutputsJson` into.
func mkDeps(t *testing.T, m map[string]map[string]any) pApi.ResourcesOutputs {
	t.Helper()
	out := make(pApi.ResourcesOutputs, len(m))
	for name, kv := range m {
		pro := &api.PluginResourceOutput{
			PluginName: "test",
			Urn:        "urn:pulumi:test::test::aws:foo::" + name,
			Outputs:    make(map[string]api.PluginOutputValue, len(kv)),
		}
		for k, v := range kv {
			pro.Outputs[k] = api.PluginOutputValue{Value: v}
		}
		out[name] = &api.ResourceOutput{Ref: pro}
	}
	return out
}

func TestMatchConsumerToDatabase_HappyPath(t *testing.T) {
	// sc-plugin-mongodb-style exports keyed on the cluster prefix.
	deps := mkDeps(t, map[string]map[string]any{
		"the-mongo": {
			"my-cluster-mongo-uri": "mongodb://host:27017/",
			"my-cluster-database":  "forge_conductor",
		},
	})
	databases := []BackupDatabase{
		{Resource: "the-mongo", Driver: "mongodb"},
	}
	resName, dbName, matched, err := MatchConsumerToDatabase(deps, databases, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resName != "the-mongo" || dbName != "forge_conductor" || matched == nil {
		t.Errorf("got resName=%q dbName=%q matched=%v", resName, dbName, matched)
	}
}

func TestMatchConsumerToDatabase_NoConsumerUsage(t *testing.T) {
	deps := mkDeps(t, map[string]map[string]any{
		"the-rds": {"driver": "postgres"},
	})
	databases := []BackupDatabase{{Resource: "the-mongo", Driver: "mongodb"}}
	_, _, matched, err := MatchConsumerToDatabase(deps, databases, nil)
	if err != nil {
		t.Fatal(err)
	}
	if matched != nil {
		t.Errorf("expected no match, got %v", matched)
	}
}

func TestMatchConsumerToDatabase_NoDatabaseField(t *testing.T) {
	// Sibling resource is there but no database-name key → no match.
	deps := mkDeps(t, map[string]map[string]any{
		"the-mongo": {"my-cluster-mongo-uri": "mongodb://h"},
	})
	databases := []BackupDatabase{{Resource: "the-mongo", Driver: "mongodb"}}
	_, _, matched, err := MatchConsumerToDatabase(deps, databases, nil)
	if err != nil {
		t.Fatal(err)
	}
	if matched != nil {
		t.Errorf("expected no match, got %v", matched)
	}
}

func TestMatchConsumerToDatabase_CanonicalKeys(t *testing.T) {
	// Exact `database`/`dbName`/`databaseName` for canonical-contract
	// plugins + synthetic test fixtures.
	for _, key := range []string{"database", "dbName", "databaseName"} {
		deps := mkDeps(t, map[string]map[string]any{
			"the-mongo": {key: "forge_conductor"},
		})
		databases := []BackupDatabase{{Resource: "the-mongo", Driver: "mongodb"}}
		_, dbName, matched, err := MatchConsumerToDatabase(deps, databases, nil)
		if err != nil {
			t.Fatal(err)
		}
		if matched == nil || dbName != "forge_conductor" {
			t.Errorf("key=%q: got dbName=%q matched=%v", key, dbName, matched)
		}
	}
}

func TestMatchConsumerToDatabase_EmptyInputs(t *testing.T) {
	_, _, matched, err := MatchConsumerToDatabase(nil, []BackupDatabase{{Resource: "x", Driver: "mongodb"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if matched != nil {
		t.Errorf("expected nil match on nil deps, got %v", matched)
	}
	_, _, matched, err = MatchConsumerToDatabase(mkDeps(t, nil), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if matched != nil {
		t.Errorf("expected nil match on nil dbs, got %v", matched)
	}
}

// Slice 1d.4-redux: Cluster field defaults to Resource on populate for
// stacks that don't set `cluster:` explicitly.
func TestPopulateDatabasesFromRaw_ClusterDefaultsToResource(t *testing.T) {
	raw := map[string]any{
		"databases": []any{
			map[string]any{"resource": "the-mongo", "driver": "mongodb"},
			map[string]any{"resource": "the-mongo-2", "driver": "mongodb", "cluster": "custom-name"},
		},
		"atlasAdmin": map[string]any{"privateKey": "p", "publicKey": "pub", "projectId": "x"},
	}
	dbs, _, err := populateDatabasesFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) != 2 {
		t.Fatalf("count: %d", len(dbs))
	}
	if dbs[0].Cluster != "the-mongo" {
		t.Errorf("default: got %q, want %q", dbs[0].Cluster, "the-mongo")
	}
	if dbs[1].Cluster != "custom-name" {
		t.Errorf("explicit: got %q", dbs[1].Cluster)
	}
}
