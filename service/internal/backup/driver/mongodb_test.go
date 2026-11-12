// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMongoDBDriver_Name(t *testing.T) {
	if (MongoDBDriver{}).Name() != "mongodb" {
		t.Errorf("name: %q", MongoDBDriver{}.Name())
	}
}

func TestMongoDBDriver_RestoreCommand(t *testing.T) {
	got := (MongoDBDriver{}).RestoreCommand()
	// SPA renders this verbatim with {cat} swapped for a download step.
	// Shape must stay stable for both the docs and the SPA.
	want := `{cat} | mongorestore --uri "<target>" --archive --gzip`
	if got != want {
		t.Errorf("RestoreCommand: got %q, want %q", got, want)
	}
}

func TestMongoDBDriver_Dump_EmptyURI(t *testing.T) {
	d := &MongoDBDriver{}
	err := d.Dump(context.Background(), ConnectionSpec{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "URI is empty") {
		t.Errorf("want URI-empty error, got %v", err)
	}
}

func TestMongoDBDriver_Restore_EmptyURI(t *testing.T) {
	d := &MongoDBDriver{}
	err := d.Restore(context.Background(), ConnectionSpec{}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "URI is empty") {
		t.Errorf("want URI-empty error, got %v", err)
	}
}

func TestMongoDBDriver_Dump_MongodumpMissing(t *testing.T) {
	// If mongodump IS installed on the test host, this test skips —
	// requireCmd returns nil and the subsequent code path needs a
	// reachable server, which this unit test doesn't set up.
	if _, err := lookPath("mongodump"); err == nil {
		t.Skip("mongodump is installed; the missing-CLI path is not exercised here")
	}
	d := &MongoDBDriver{}
	err := d.Dump(context.Background(), ConnectionSpec{URI: "mongodb://localhost:27017"}, io.Discard)
	var notFound *commandNotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("want commandNotFoundError, got %v (%T)", err, err)
	}
}

// Stops compiler from pulling in os/exec into the test package via
// LookPath — we only need it in one test.
func lookPath(name string) (string, error) {
	// A pure os/exec LookPath would do, but writing this here keeps
	// the exec import contained to one test site.
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			if st.Mode()&0o111 != 0 {
				return candidate, nil
			}
		}
	}
	return "", errors.New("not found")
}

// TestPrivateDir_Permissions verifies the 0700 mode on the tempdir
// and 0600 on the config file — the whole reason we don't pass the
// URI on argv.
func TestPrivateDir_And_WritePrivateFile_Permissions(t *testing.T) {
	dir, cleanup, err := privateDir()
	if err != nil {
		t.Fatalf("privateDir: %v", err)
	}
	t.Cleanup(cleanup)
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	// Only the perm bits matter — the type bit is ModeDir.
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Errorf("tempdir perm: got %o, want 0700", perm)
	}

	path, err := writePrivateFile(dir, "cfg.yaml", "uri: \"mongodb://x\"\n")
	if err != nil {
		t.Fatalf("writePrivateFile: %v", err)
	}
	fst, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fst.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm: got %o, want 0600", perm)
	}

	// Writing the same file twice must refuse — O_EXCL.
	if _, err := writePrivateFile(dir, "cfg.yaml", "x"); err == nil {
		t.Errorf("want second-write refusal, got nil")
	}
}

func TestRedact(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		secrets []string
		want    string
	}{
		{
			name:    "password embedded in URI — URI substituted first (longest-first)",
			in:      "connection failed to mongodb://u:hunter2@h/ with hunter2 bad",
			secrets: []string{"mongodb://u:hunter2@h/", "hunter2"},
			want:    "connection failed to <uri> with <password> bad",
		},
		{
			name:    "empty secrets are skipped",
			in:      "hello hunter2",
			secrets: []string{"", "hunter2"},
			want:    "hello <password>",
		},
		{
			name:    "no secrets — unchanged",
			in:      "no creds here",
			secrets: []string{""},
			want:    "no creds here",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redact(c.in, c.secrets); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestCleanEnv_ScrubsPrefixes(t *testing.T) {
	t.Setenv("PGHOST", "attacker")
	t.Setenv("MONGODB_URI", "attacker")
	t.Setenv("MYSQL_PWD", "attacker")
	t.Setenv("MARIADB_DUMP_TEST", "attacker")
	t.Setenv("PATH_OK", "preserved")

	env := cleanEnv()
	pairs := map[string]string{}
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq > 0 {
			pairs[kv[:eq]] = kv[eq+1:]
		}
	}
	for _, scrubbed := range []string{"PGHOST", "MONGODB_URI", "MYSQL_PWD", "MARIADB_DUMP_TEST"} {
		if v, ok := pairs[scrubbed]; ok {
			t.Errorf("env %s present after scrub: %q", scrubbed, v)
		}
	}
	if pairs["PATH_OK"] != "preserved" {
		t.Errorf("non-scrub env dropped")
	}
}

func TestInventory_RequiresDB(t *testing.T) {
	d := &MongoDBDriver{}
	_, err := d.Inventory(context.Background(), ConnectionSpec{URI: "mongodb://localhost"}, "")
	if err == nil || !strings.Contains(err.Error(), "requires a database name") {
		t.Errorf("want requires-db error, got %v", err)
	}
}

// Integration test — skipped unless MONGO_TEST_URI is set.
func TestMongoDBDriver_Integration_ListDatabases(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("integration — needs MONGO_TEST_URI")
	}
	d := &MongoDBDriver{}
	ctx := context.Background()
	names, err := d.ListDatabases(ctx, ConnectionSpec{URI: uri})
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	for _, n := range names {
		if _, sys := mongoSystemDBs[n]; sys {
			t.Errorf("system DB %q leaked into result", n)
		}
	}
}
