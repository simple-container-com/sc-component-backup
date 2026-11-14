// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"testing"

	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
)

// fakeCollector is a minimal `pApi.ComputeContextCollector` that only
// implements the two read methods `discoverDatabasesFromCollector`
// needs. Mirrors the collector's wire contract without pulling in
// the `runtime.Collector` type or any Pulumi SDK plumbing.
type fakeCollector struct {
	plain                        []pApi.ComputeEnvVariable
	secret                       []pApi.ComputeEnvVariable
	pApi.ComputeContextCollector // embed to satisfy the rest of the interface (panics on any method we don't override — but the function under test doesn't call them)
}

func (f *fakeCollector) EnvVariables() []pApi.ComputeEnvVariable {
	return f.plain
}

func (f *fakeCollector) SecretEnvVariables() []pApi.ComputeEnvVariable {
	return f.secret
}

func TestDiscoverDatabasesFromCollector_Mongo(t *testing.T) {
	c := &fakeCollector{
		plain: []pApi.ComputeEnvVariable{
			{Name: "MONGO_USER", Value: "the-mongo-service-user"},
			{Name: "MONGO_DATABASE", Value: "forge_app"},
		},
		secret: []pApi.ComputeEnvVariable{
			{Name: "MONGO_URI", Value: "mongodb+srv://u:p@host/?replicaSet=rs"},
			{Name: "MONGO_PASSWORD", Value: "p"},
		},
	}
	got := discoverDatabasesFromCollector(c)
	if len(got) != 1 {
		t.Fatalf("expected 1 discovered DB, got %d: %+v", len(got), got)
	}
	d := got[0]
	if d.driver != "mongodb" {
		t.Errorf("driver: %q", d.driver)
	}
	if d.database != "forge_app" {
		t.Errorf("database: %q", d.database)
	}
	if d.uri != "mongodb+srv://u:p@host/?replicaSet=rs" {
		t.Errorf("uri: %q", d.uri)
	}
	if d.user != "the-mongo-service-user" {
		t.Errorf("user: %q", d.user)
	}
}

func TestDiscoverDatabasesFromCollector_NoMongo(t *testing.T) {
	// Consumer that uses no DB siblings — collector carries only the
	// backup component's own env (BACKUP_AGENT_URL, BACKUP_TOKEN emitted
	// AFTER this discovery call, so they're not even here yet).
	c := &fakeCollector{}
	got := discoverDatabasesFromCollector(c)
	if len(got) != 0 {
		t.Errorf("expected 0 discovered DBs, got %d", len(got))
	}
}

func TestDiscoverDatabasesFromCollector_MongoURIWithoutDatabase(t *testing.T) {
	// URI but no DB — we still report it so the caller can decide. The
	// caller skips minting when `database` is empty (see
	// `maybeProvisionScopedUsers`).
	c := &fakeCollector{
		secret: []pApi.ComputeEnvVariable{
			{Name: "MONGO_URI", Value: "mongodb://host/"},
		},
	}
	got := discoverDatabasesFromCollector(c)
	if len(got) != 1 {
		t.Fatalf("expected 1 discovered DB, got %d", len(got))
	}
	if got[0].database != "" {
		t.Errorf("expected empty database, got %q", got[0].database)
	}
}

// rewriteMongoURICreds is the small helper that swaps out a scoped
// user's creds into a sibling-emitted mongo URI. Pin all three shapes
// the mongodb plugin emits (bare + srv + already-has-creds).
func TestRewriteMongoURICreds(t *testing.T) {
	cases := map[string]string{
		"mongodb://u:p@host:27017/":          "mongodb://backup-x:newpass@host:27017/",
		"mongodb+srv://u:p@h/?replicaSet=rs": "mongodb+srv://backup-x:newpass@h/?replicaSet=rs",
		"mongodb://host-no-at/":              "mongodb://backup-x:newpass@host-no-at/",
	}
	for in, want := range cases {
		got, err := rewriteMongoURICreds(in, "backup-x", "newpass")
		if err != nil {
			t.Fatalf("uri=%q err=%v", in, err)
		}
		if got != want {
			t.Errorf("uri=%q: got %q, want %q", in, got, want)
		}
	}
}

func TestRewriteMongoURICreds_UnknownScheme(t *testing.T) {
	_, err := rewriteMongoURICreds("postgres://u:p@h/db", "backup-x", "newpass")
	if err == nil {
		t.Error("expected error on non-mongo URI")
	}
}
