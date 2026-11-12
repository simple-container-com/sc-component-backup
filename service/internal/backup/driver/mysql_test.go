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

func TestMySQLDriver_Name(t *testing.T) {
	if (MySQLDriver{}).Name() != "mysql" {
		t.Errorf("name: %q", MySQLDriver{}.Name())
	}
}

func TestMySQLDriver_RestoreCommand(t *testing.T) {
	got := (MySQLDriver{}).RestoreCommand()
	want := `{cat} | gunzip | mariadb --database="<target_db>"   # credentials via ~/.my.cnf`
	if got != want {
		t.Errorf("RestoreCommand: got %q, want %q", got, want)
	}
}

func TestMySQLDriver_ByName_Resolves(t *testing.T) {
	d, err := ByName("mysql")
	if err != nil {
		t.Fatalf("ByName: %v", err)
	}
	if d.Name() != "mysql" {
		t.Errorf("driver name: %q", d.Name())
	}
}

func TestMySQLDriver_Dump_RequiresDatabase(t *testing.T) {
	d := &MySQLDriver{}
	err := d.Dump(context.Background(), ConnectionSpec{Host: "h", User: "u"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Database is required") {
		t.Errorf("want DB-required error, got %v", err)
	}
}

func TestMySQLDriver_Dump_HyphenPrefixedDBRefused(t *testing.T) {
	d := &MySQLDriver{}
	err := d.Dump(context.Background(), ConnectionSpec{
		Host: "h", User: "u", Database: "-evil",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "starts with") {
		t.Errorf("want hyphen-prefixed-DB refusal, got %v", err)
	}
}

func TestMySQLDriver_Dump_MissingDumper(t *testing.T) {
	for _, bin := range []string{"mariadb-dump", "mysqldump"} {
		if _, err := lookPath(bin); err == nil {
			t.Skipf("%s is installed; the missing-CLI path is not exercised here", bin)
		}
	}
	d := &MySQLDriver{}
	err := d.Dump(context.Background(), ConnectionSpec{
		Host: "h", User: "u", Database: "db",
	}, io.Discard)
	var notFound *commandNotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("want commandNotFoundError, got %v (%T)", err, err)
	}
}

func TestMySQLDriver_Inventory_RequiresDB(t *testing.T) {
	d := &MySQLDriver{}
	_, err := d.Inventory(context.Background(), ConnectionSpec{Host: "h", User: "u"}, "")
	if err == nil || !strings.Contains(err.Error(), "requires a database name") {
		t.Errorf("want requires-db error, got %v", err)
	}
}

func TestMySQLWriteDefaults_Shape(t *testing.T) {
	d := &MySQLDriver{}
	dir, cleanup, err := privateDir()
	if err != nil {
		t.Fatalf("privateDir: %v", err)
	}
	t.Cleanup(cleanup)

	path, err := d.writeDefaults(dir, ConnectionSpec{
		Host: "h.example.net", Port: 3307, User: "alice", Password: "hunter2",
	})
	if err != nil {
		t.Fatalf("writeDefaults: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm: got %o, want 0600", perm)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(content)
	for _, want := range []string{
		"[client]",
		`host="h.example.net"`,
		"port=3307",
		`user="alice"`,
		`password="hunter2"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("defaults file missing %q; got:\n%s", want, s)
		}
	}
}

func TestMySQLWriteDefaults_SSLFromURI(t *testing.T) {
	d := &MySQLDriver{}

	// writeDefaults uses O_EXCL on my.cnf, so each case needs a fresh
	// private dir rather than reusing one.
	dir1, cleanup1, err := privateDir()
	if err != nil {
		t.Fatalf("privateDir: %v", err)
	}
	t.Cleanup(cleanup1)
	path, err := d.writeDefaults(dir1, ConnectionSpec{
		URI: "mariadb://alice:p@h:3306/?ssl=true",
	})
	if err != nil {
		t.Fatalf("writeDefaults(ssl=true): %v", err)
	}
	s, _ := os.ReadFile(path)
	if !strings.Contains(string(s), "\nssl\n") && !strings.HasSuffix(strings.TrimSpace(string(s)), "ssl") {
		t.Errorf("ssl truthy → `ssl` line missing; got:\n%s", s)
	}

	dir2, cleanup2, err := privateDir()
	if err != nil {
		t.Fatalf("privateDir: %v", err)
	}
	t.Cleanup(cleanup2)
	path2, err := d.writeDefaults(dir2, ConnectionSpec{
		URI: "mariadb://alice:p@h:3306/?ssl=0",
	})
	if err != nil {
		t.Fatalf("writeDefaults(ssl=0): %v", err)
	}
	s2, _ := os.ReadFile(path2)
	if !strings.Contains(string(s2), "skip-ssl") {
		t.Errorf("ssl=0 → `skip-ssl` line missing; got:\n%s", s2)
	}
}

func TestMySQLWriteDefaults_HostRequired(t *testing.T) {
	d := &MySQLDriver{}
	dir, cleanup, _ := privateDir()
	t.Cleanup(cleanup)
	_, err := d.writeDefaults(dir, ConnectionSpec{User: "u"})
	if err == nil || !strings.Contains(err.Error(), "host is empty") {
		t.Errorf("want host-empty error, got %v", err)
	}
}

func TestMySQLWriteDefaults_UserRequired(t *testing.T) {
	d := &MySQLDriver{}
	dir, cleanup, _ := privateDir()
	t.Cleanup(cleanup)
	_, err := d.writeDefaults(dir, ConnectionSpec{Host: "h"})
	if err == nil || !strings.Contains(err.Error(), "user is empty") {
		t.Errorf("want user-empty error, got %v", err)
	}
}

func TestMySQLOptQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"simple", `"simple"`},
		{`has"quote`, `"has\"quote"`},
		{`has\back`, `"has\\back"`},
		{"line\nbreak", `"line\nbreak"`},
	}
	for _, c := range cases {
		if got := mysqlOptQuote(c.in); got != c.want {
			t.Errorf("mysqlOptQuote(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMysqlSQLStr(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"simple", `'simple'`},
		{"with'apos", `'with''apos'`},
		{`back\slash`, `'back\\slash'`},
	}
	for _, c := range cases {
		if got := mysqlSQLStr(c.in); got != c.want {
			t.Errorf("mysqlSQLStr(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMysqlUnescape(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`plain`, "plain"},
		{`a\tb`, "a\tb"},
		{`a\nb`, "a\nb"},
		{`a\\b`, `a\b`},
		{`end\0`, "end\x00"},
		// Unknown escape — leave as-is (edge case).
		{`a\xb`, `a\xb`},
	}
	for _, c := range cases {
		if got := mysqlUnescape(c.in); got != c.want {
			t.Errorf("mysqlUnescape(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseMySQLURI(t *testing.T) {
	host, port, user, pw, q := parseMySQLURI("mariadb://alice:hunter2@db.example.net:3307/?ssl=true")
	if host != "db.example.net" {
		t.Errorf("host: %q", host)
	}
	if port != 3307 {
		t.Errorf("port: %d", port)
	}
	if user != "alice" {
		t.Errorf("user: %q", user)
	}
	if pw != "hunter2" {
		t.Errorf("password: %q", pw)
	}
	if q["ssl"] != "true" {
		t.Errorf("ssl: %v", q)
	}

	// mysql:// prefix also works.
	host2, _, _, _, _ := parseMySQLURI("mysql://u@h:3306/")
	if host2 != "h" {
		t.Errorf("mysql:// host: %q", host2)
	}
}

// Integration test — skipped unless MYSQL_TEST_URI is set.
func TestMySQLDriver_Integration_ListDatabases(t *testing.T) {
	uri := os.Getenv("MYSQL_TEST_URI")
	if uri == "" {
		t.Skip("integration — needs MYSQL_TEST_URI")
	}
	d := &MySQLDriver{}
	ctx := context.Background()
	names, err := d.ListDatabases(ctx, ConnectionSpec{URI: uri})
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	for _, n := range names {
		if _, sys := mysqlSystemDBs[n]; sys {
			t.Errorf("system DB %q leaked into result", n)
		}
	}
}
