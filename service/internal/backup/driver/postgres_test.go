// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPostgresDriver_Name(t *testing.T) {
	if (PostgresDriver{}).Name() != "postgres" {
		t.Errorf("name: %q", PostgresDriver{}.Name())
	}
}

func TestPostgresDriver_RestoreCommand(t *testing.T) {
	got := (PostgresDriver{}).RestoreCommand()
	want := `{cat} > db.pgdump && pg_restore --dbname "<target>" --no-owner --exit-on-error db.pgdump`
	if got != want {
		t.Errorf("RestoreCommand: got %q, want %q", got, want)
	}
}

func TestPostgresDriver_ByName_Resolves(t *testing.T) {
	for _, n := range []string{"postgres", "postgresql"} {
		d, err := ByName(n)
		if err != nil {
			t.Fatalf("ByName(%q): %v", n, err)
		}
		if d.Name() != "postgres" {
			t.Errorf("ByName(%q): driver name %q", n, d.Name())
		}
	}
}

func TestPostgresDriver_Dump_MissingPgDump(t *testing.T) {
	if _, err := lookPath("pg_dump"); err == nil {
		t.Skip("pg_dump is installed; the missing-CLI path is not exercised here")
	}
	d := &PostgresDriver{}
	err := d.Dump(context.Background(), ConnectionSpec{
		Host: "localhost", Port: 5432, User: "u", Password: "p", Database: "db",
	}, io.Discard)
	var notFound *commandNotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("want commandNotFoundError, got %v (%T)", err, err)
	}
}

func TestPostgresDriver_Inventory_RequiresDB(t *testing.T) {
	d := &PostgresDriver{}
	_, err := d.Inventory(context.Background(), ConnectionSpec{Host: "localhost", User: "u"}, "")
	if err == nil || !strings.Contains(err.Error(), "requires a database name") {
		t.Errorf("want requires-db error, got %v", err)
	}
}

func TestPostgresConninfo_FromExplicitFields(t *testing.T) {
	d := &PostgresDriver{}
	got, err := d.conninfo(ConnectionSpec{
		Host: "h.example.net", Port: 5432, User: "alice", Database: "prod",
	}, "")
	if err != nil {
		t.Fatalf("conninfo: %v", err)
	}
	// Order is stable: host, port, user, dbname (then sorted query params if any).
	want := "host='h.example.net' port=5432 user='alice' dbname='prod'"
	if got != want {
		t.Errorf("conninfo:\n  got %q\n want %q", got, want)
	}
}

func TestPostgresConninfo_FromURI(t *testing.T) {
	d := &PostgresDriver{}
	got, err := d.conninfo(ConnectionSpec{
		URI: "postgresql://alice:hunter2@h.example.net:5433/prod?sslmode=require&foo=bar",
	}, "")
	if err != nil {
		t.Fatalf("conninfo: %v", err)
	}
	// foo=bar is NOT in pgPassParams → dropped.
	want := "host='h.example.net' port=5433 user='alice' dbname='prod' sslmode='require'"
	if got != want {
		t.Errorf("conninfo:\n  got %q\n want %q", got, want)
	}
}

func TestPostgresConninfo_DBOverride(t *testing.T) {
	d := &PostgresDriver{}
	got, err := d.conninfo(ConnectionSpec{
		Host: "h", User: "alice", Database: "default",
	}, "override")
	if err != nil {
		t.Fatalf("conninfo: %v", err)
	}
	if !strings.Contains(got, "dbname='override'") {
		t.Errorf("db override not applied: %q", got)
	}
}

func TestPostgresConninfo_HostRequired(t *testing.T) {
	d := &PostgresDriver{}
	_, err := d.conninfo(ConnectionSpec{User: "u"}, "")
	if err == nil || !strings.Contains(err.Error(), "host is empty") {
		t.Errorf("want host-empty error, got %v", err)
	}
}

func TestPostgresConninfo_UserRequired(t *testing.T) {
	d := &PostgresDriver{}
	_, err := d.conninfo(ConnectionSpec{Host: "h"}, "")
	if err == nil || !strings.Contains(err.Error(), "user is empty") {
		t.Errorf("want user-empty error, got %v", err)
	}
}

func TestPostgresConninfo_QuotesSpecialChars(t *testing.T) {
	d := &PostgresDriver{}
	got, err := d.conninfo(ConnectionSpec{
		Host: "h", User: `al\ice`, Database: "with'apos",
	}, "")
	if err != nil {
		t.Fatalf("conninfo: %v", err)
	}
	// Backslash doubles, single-quote → \'.
	if !strings.Contains(got, `user='al\\ice'`) {
		t.Errorf("backslash not doubled: %q", got)
	}
	if !strings.Contains(got, `dbname='with\'apos'`) {
		t.Errorf("apostrophe not escaped: %q", got)
	}
}

func TestPgPassEscape(t *testing.T) {
	// `:` MUST be backslash-prefixed (field separator); `\` must double.
	cases := []struct {
		in, want string
	}{
		{"simple", "simple"},
		{"has:colon", `has\:colon`},
		{`has\back`, `has\\back`},
		{`::`, `\:\:`},
	}
	for _, c := range cases {
		if got := pgPassEscape(c.in); got != c.want {
			t.Errorf("pgPassEscape(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPgMajor(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"16.1 (Debian 16.1-1.pgdg120+1)", 16},
		{"13.14", 13},
		{"", 0},
		{"not-a-version", 0},
		{"17", 17},
	}
	for _, c := range cases {
		if got := pgMajor(c.in); got != c.want {
			t.Errorf("pgMajor(%q): got %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParsePgURI(t *testing.T) {
	host, port, user, db, q := parsePgURI("postgresql://alice:hunter2@db.example.net:5433/prod?sslmode=require&connect_timeout=10")
	if host != "db.example.net" {
		t.Errorf("host: %q", host)
	}
	if port != 5433 {
		t.Errorf("port: %d", port)
	}
	if user != "alice" {
		t.Errorf("user: %q", user)
	}
	if db != "prod" {
		t.Errorf("db: %q", db)
	}
	if q["sslmode"] != "require" || q["connect_timeout"] != "10" {
		t.Errorf("query: %v", q)
	}
}

func TestParsePgURI_Defaults(t *testing.T) {
	// No credentials, no port, no DB — host-only.
	host, port, user, db, _ := parsePgURI("postgres://db.example.net")
	if host != "db.example.net" || port != 0 || user != "" || db != "" {
		t.Errorf("minimal URI: host=%q port=%d user=%q db=%q", host, port, user, db)
	}
}

func TestSslmodeFromURI(t *testing.T) {
	cases := []struct {
		uri, want string
	}{
		{"postgres://h?sslmode=require", "require"},
		{"postgres://h", ""},
		{"postgres://h?foo=bar", ""},
	}
	for _, c := range cases {
		if got := sslmodeFromURI(c.uri); got != c.want {
			t.Errorf("sslmodeFromURI(%q): got %q, want %q", c.uri, got, c.want)
		}
	}
}

// Integration test — skipped unless POSTGRES_TEST_URI is set.
func TestPostgresDriver_Integration_ListDatabases(t *testing.T) {
	uri := os.Getenv("POSTGRES_TEST_URI")
	if uri == "" {
		t.Skip("integration — needs POSTGRES_TEST_URI")
	}
	d := &PostgresDriver{}
	ctx := context.Background()
	names, err := d.ListDatabases(ctx, ConnectionSpec{URI: uri})
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	for _, n := range names {
		if _, sys := pgSystemDBs[n]; sys {
			t.Errorf("system DB %q leaked into result", n)
		}
	}
}
