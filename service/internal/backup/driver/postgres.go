// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pgSystemDBs are the server-owned databases a user-scoped list should
// hide. `postgres` is a real user DB on vanilla installs but is the
// maintenance DB on most managed services — list it so the UI can offer
// it when operators actually want to back it up, but drop the templates.
// Matches local-db-backup/db_backup/config.py:SYSTEM_DBS["postgres"].
var pgSystemDBs = map[string]struct{}{
	"template0": {},
	"template1": {},
}

// pgManagedDBs are maintenance databases of managed services (AWS RDS,
// Azure, GCP Cloud SQL). Backing them up is useless at best and
// permission-denied at worst — always hide them.
var pgManagedDBs = map[string]struct{}{
	"rdsadmin":          {},
	"azure_maintenance": {},
	"azure_sys":         {},
	"cloudsqladmin":     {},
}

// pgPassParams names the libpq query parameters propagated from the URI
// into the conninfo string. Anything outside this list is ignored —
// unknown params break libpq. Matches the Python reference's _PASS_PARAMS.
var pgPassParams = map[string]struct{}{
	"sslmode":              {},
	"sslrootcert":          {},
	"connect_timeout":      {},
	"target_session_attrs": {},
	"application_name":     {},
	"options":              {},
}

// PostgresDriver shells `pg_dump --format=custom` (writes a signed TOC
// + per-table streams, `pg_restore` can read it back selectively) and
// `pg_restore`. The password goes through a 0600 PGPASSFILE; host/port/
// user/db travel in a libpq conninfo string passed as --dbname.
//
// Query helpers (ListDatabases / Inventory / Test) also shell out to
// psql rather than linking a Go SQL driver — the same reasoning as the
// Mongo port: the native client knows its own back-compat matrix and
// zero extra deps enters go.mod.
//
// Ported from local-db-backup/db_backup/engines/postgres.py.
type PostgresDriver struct{}

func (PostgresDriver) Name() string { return "postgres" }

// RestoreCommand is the human-readable one-liner the SPA renders on
// the archive's restore tab. `{cat}` is a placeholder the frontend
// swaps for the concrete download step.
func (PostgresDriver) RestoreCommand() string {
	return `{cat} > db.pgdump && pg_restore --dbname "<target>" --no-owner --exit-on-error db.pgdump`
}

// Dump streams a pg_dump custom-format archive to out. The caller is
// responsible for closing out.
//
// Streams directly to out rather than buffering to a tempfile +
// pg_restore --list integrity check (which the Python reference does).
// Rationale: for this component the output goes straight to S3; a
// corrupt dump surfaces on the next restore, and we'd rather fail fast
// than double the local disk requirement. Same trade-off the Mongo
// port makes.
func (p *PostgresDriver) Dump(ctx context.Context, conn ConnectionSpec, out io.Writer) error {
	if err := requireCmd("pg_dump"); err != nil {
		return err
	}
	dir, cleanup, err := privateDir()
	if err != nil {
		return err
	}
	defer cleanup()
	env, err := p.env(dir, conn)
	if err != nil {
		return err
	}
	ci, err := p.conninfo(conn, conn.Database)
	if err != nil {
		return err
	}
	// --file omitted → pg_dump writes to stdout.
	args := []string{
		"--format=custom",
		"--compress=6",
		"--no-password",
		"--dbname=" + ci,
	}
	cmd := exec.CommandContext(ctx, "pg_dump", args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump failed: %w; stderr tail: %s", err,
			redact(tailStderr(stderr.Bytes()), secretsFromConn(conn)))
	}
	return nil
}

// Restore streams a pg_restore from in. The archive must be the shape
// pg_dump --format=custom produced.
func (p *PostgresDriver) Restore(ctx context.Context, conn ConnectionSpec, in io.Reader) error {
	if err := requireCmd("pg_restore"); err != nil {
		return err
	}
	dir, cleanup, err := privateDir()
	if err != nil {
		return err
	}
	defer cleanup()
	env, err := p.env(dir, conn)
	if err != nil {
		return err
	}
	ci, err := p.conninfo(conn, conn.Database)
	if err != nil {
		return err
	}
	args := []string{
		"--clean", "--if-exists", "--no-owner", "--no-acl",
		"--exit-on-error",
		"--no-password",
		"--dbname=" + ci,
	}
	cmd := exec.CommandContext(ctx, "pg_restore", args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stdin = in
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore failed: %w; stderr tail: %s", err,
			redact(tailStderr(stderr.Bytes()), secretsFromConn(conn)))
	}
	return nil
}

// ListDatabases returns the user-visible databases, sorted. Templates
// and managed-service maintenance DBs are filtered out so the SPA
// shows only what the operator cares about.
func (p *PostgresDriver) ListDatabases(ctx context.Context, conn ConnectionSpec) ([]string, error) {
	const sql = "select datname from pg_database where datallowconn and not datistemplate order by 1"
	rows, err := p.psql(ctx, conn, sql, "", 30*time.Second)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r)
		if name == "" {
			continue
		}
		if _, sys := pgSystemDBs[name]; sys {
			continue
		}
		if _, mgd := pgManagedDBs[name]; mgd {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Inventory enumerates user tables (relkind 'r') and materialized
// views ('m') of `db` with per-table size breakdown read from the
// catalog. Row counts are the planner's estimate (`reltuples`), flagged
// `approx: true` in the Python reference. We use Bytes = pg_table_size
// (heap+toast, no indexes), IndexBytes = pg_indexes_size, StorageBytes
// = pg_total_relation_size - pg_indexes_size. Partitioned parents have
// no storage of their own and don't appear — each partition counts once.
func (p *PostgresDriver) Inventory(ctx context.Context, conn ConnectionSpec, db string) ([]CollectionStat, error) {
	if db == "" {
		return nil, fmt.Errorf("postgres driver: Inventory requires a database name")
	}
	const sql = "select c.relkind, c.reltuples::bigint, pg_table_size(c.oid), pg_indexes_size(c.oid)," +
		" pg_total_relation_size(c.oid), n.nspname || '.' || c.relname" +
		" from pg_class c join pg_namespace n on n.oid = c.relnamespace" +
		" where c.relkind in ('r', 'm') and n.nspname not in ('pg_catalog', 'information_schema')" +
		" and n.nspname !~ '^pg_(toast|temp_|toast_temp_)'" +
		" order by pg_total_relation_size(c.oid) desc"
	rows, err := p.psqlFS(ctx, conn, sql, db, "\t", 30*time.Second)
	if err != nil {
		return nil, err
	}
	out := make([]CollectionStat, 0, len(rows))
	for _, row := range rows {
		parts := strings.SplitN(row, "\t", 6)
		if len(parts) != 6 {
			continue
		}
		// parts: [relkind, reltuples, table_bytes, index_bytes, total_bytes, name]
		docs, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			continue
		}
		tableBytes, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		if err != nil {
			continue
		}
		indexBytes, err := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64)
		if err != nil {
			continue
		}
		totalBytes, err := strconv.ParseInt(strings.TrimSpace(parts[4]), 10, 64)
		if err != nil {
			continue
		}
		storage := totalBytes - indexBytes
		if storage < 0 {
			storage = 0
		}
		cs := CollectionStat{
			Name:         parts[5],
			Kind:         "table",
			Bytes:        tableBytes,
			StorageBytes: storage,
			IndexBytes:   indexBytes,
		}
		if docs >= 0 {
			cs.Docs = docs
		}
		out = append(out, cs)
	}
	return out, nil
}

// Test probes connectivity + collects version info + warns about
// common misconfigurations (sslmode missing on a remote server, server
// newer than pg_dump).
func (p *PostgresDriver) Test(ctx context.Context, conn ConnectionSpec) (TestResult, error) {
	res := TestResult{
		ClientVersion: toolVersion(ctx, "pg_dump", "--version"),
	}
	// server_version is a GUC; `show server_version` returns one line
	// like "16.1 (Debian 16.1-1.pgdg120+1)". We just want the major.
	rows, err := p.psql(ctx, conn, "show server_version", "", 15*time.Second)
	if err != nil {
		res.Error = redact(err.Error(), secretsFromConn(conn))
		if res.ClientVersion == "" {
			res.Warnings = append(res.Warnings, "pg_dump not found in the container")
		}
		return res, nil
	}
	if len(rows) > 0 {
		res.ServerVersion = strings.Fields(strings.TrimSpace(rows[0]))[0]
	}

	dbs, lerr := p.ListDatabases(ctx, conn)
	if lerr == nil {
		res.Databases = dbs
	} else {
		res.Error = redact(lerr.Error(), secretsFromConn(conn))
	}

	// Version-skew warning: pg_dump N can't dump a server > N.
	srvMajor := pgMajor(res.ServerVersion)
	cliMajor := pgMajor(res.ClientVersion)
	if res.ClientVersion == "" {
		res.Warnings = append(res.Warnings, "pg_dump not found in the container")
	} else if srvMajor > 0 && cliMajor > 0 && srvMajor > cliMajor {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("server %d is newer than pg_dump %d: dumps will fail", srvMajor, cliMajor))
	}

	// sslmode warning — matches Python reference.
	host := conn.Host
	if host == "" && conn.URI != "" {
		host = hostFromURI(conn.URI)
	}
	if host != "" && host != "localhost" && host != "127.0.0.1" {
		sslmode := sslmodeFromURI(conn.URI)
		switch sslmode {
		case "", "disable", "allow", "prefer":
			res.Warnings = append(res.Warnings,
				"for a remote server, sslmode=require (or verify-full) is recommended")
		}
	}
	res.OK = res.Error == ""
	return res, nil
}

// psql runs a SQL query via `psql -A -t -q -v ON_ERROR_STOP=1 -c <sql>`
// against the specified database (empty → default conninfo path/postgres).
// Returns the output lines with empties stripped.
func (p *PostgresDriver) psql(ctx context.Context, conn ConnectionSpec, sql, db string, timeout time.Duration) ([]string, error) {
	return p.psqlFS(ctx, conn, sql, db, "", timeout)
}

// psqlFS is psql with an optional field separator. Non-empty fieldsep
// turns on tab-separated output for parsers (Inventory uses this).
func (p *PostgresDriver) psqlFS(ctx context.Context, conn ConnectionSpec, sql, db, fieldsep string, timeout time.Duration) ([]string, error) {
	if err := requireCmd("psql"); err != nil {
		return nil, err
	}
	dir, cleanup, err := privateDir()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	env, err := p.env(dir, conn)
	if err != nil {
		return nil, err
	}
	ci, err := p.conninfo(conn, db)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-X", "-A", "-t", "-q",
		"-v", "ON_ERROR_STOP=1",
		"--no-password",
	}
	if fieldsep != "" {
		args = append(args, "-F", fieldsep)
	}
	args = append(args, "-c", sql, "--dbname="+ci)
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, "psql", args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("psql failed: %w; stderr tail: %s", err,
			redact(tailStderr(stderr.Bytes()), secretsFromConn(conn)))
	}
	out := []string{}
	for _, ln := range strings.Split(stdout.String(), "\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out, nil
}

// env writes the 0600 PGPASSFILE in dir and returns an env slice with
// PGPASSFILE pointing at it, plus connect timeout and app name. Keeps
// the password out of argv.
func (p *PostgresDriver) env(dir string, conn ConnectionSpec) ([]string, error) {
	line := fmt.Sprintf("*:*:*:%s:%s\n", pgPassEscape(conn.User), pgPassEscape(conn.Password))
	passfile, err := writePrivateFile(dir, "pgpass", line)
	if err != nil {
		return nil, err
	}
	return cleanEnv(
		"PGPASSFILE="+passfile,
		"PGCONNECT_TIMEOUT=30",
		"PGAPPNAME=sc-component-backup",
	), nil
}

// conninfo builds a libpq conninfo string from a ConnectionSpec + an
// optional database override. Falls back to parsing bits out of
// ConnectionSpec.URI when the explicit fields aren't set — some
// consumers hand us URI-only specs.
func (p *PostgresDriver) conninfo(conn ConnectionSpec, db string) (string, error) {
	host := conn.Host
	port := conn.Port
	user := conn.User
	dbname := db
	if dbname == "" {
		dbname = conn.Database
	}
	query := map[string]string{}
	if conn.URI != "" {
		uh, up, uu, udb, uq := parsePgURI(conn.URI)
		if host == "" {
			host = uh
		}
		if port == 0 {
			port = up
		}
		if user == "" {
			user = uu
		}
		if dbname == "" {
			dbname = udb
		}
		for k, v := range uq {
			if _, ok := pgPassParams[k]; ok {
				query[k] = v
			}
		}
	}
	if host == "" {
		return "", fmt.Errorf("postgres driver: host is empty; set ConnectionSpec.Host or supply a URI")
	}
	if user == "" {
		return "", fmt.Errorf("postgres driver: user is empty; set ConnectionSpec.User or embed one in the URI")
	}
	if port == 0 {
		port = 5432
	}
	if dbname == "" {
		dbname = "postgres"
	}
	// Order is stable for test-friendliness: host, port, user, dbname,
	// then query params alphabetically.
	parts := []string{
		"host=" + pgConnQuote(host),
		"port=" + strconv.Itoa(port),
		"user=" + pgConnQuote(user),
		"dbname=" + pgConnQuote(dbname),
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+pgConnQuote(query[k]))
	}
	return strings.Join(parts, " "), nil
}

// pgConnQuote wraps v in single quotes with backslash + apostrophe
// escaping, matching libpq conninfo value rules.
func pgConnQuote(v string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), "'", `\'`) + "'"
}

// pgPassEscape escapes a value for ~/.pgpass — backslash doubles,
// colon is backslash-prefixed.
func pgPassEscape(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), ":", `\:`)
}

// pgMajor extracts the major version integer from a version string
// like "16.1 (Debian 16.1-1.pgdg120+1)".
func pgMajor(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	// Grab the leading run of digits.
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(v[:end])
	if err != nil {
		return 0
	}
	return n
}

// parsePgURI pulls host/port/user/db + query params out of a
// postgres:// URI. Returns empty string / zero int for absent fields —
// the caller falls back to ConnectionSpec explicit fields in that case.
//
// Deliberately minimal: we rely on libpq to validate the final conninfo
// string, so this is just a best-effort decomposition.
func parsePgURI(uri string) (host string, port int, user, dbname string, query map[string]string) {
	query = map[string]string{}
	rest := uri
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if strings.HasPrefix(rest, prefix) {
			rest = rest[len(prefix):]
			break
		}
	}
	// Split off query.
	if idx := strings.Index(rest, "?"); idx >= 0 {
		q := rest[idx+1:]
		rest = rest[:idx]
		for _, kv := range strings.Split(q, "&") {
			if kv == "" {
				continue
			}
			eq := strings.IndexByte(kv, '=')
			if eq < 0 {
				continue
			}
			query[kv[:eq]] = kv[eq+1:]
		}
	}
	// Split off path (dbname).
	if idx := strings.Index(rest, "/"); idx >= 0 {
		dbname = rest[idx+1:]
		rest = rest[:idx]
	}
	// Split off userinfo.
	if idx := strings.Index(rest, "@"); idx >= 0 {
		userinfo := rest[:idx]
		rest = rest[idx+1:]
		if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
			user = userinfo[:colon]
		} else {
			user = userinfo
		}
	}
	// What's left is host[:port].
	if idx := strings.LastIndex(rest, ":"); idx >= 0 {
		host = rest[:idx]
		if p, err := strconv.Atoi(rest[idx+1:]); err == nil {
			port = p
		}
	} else {
		host = rest
	}
	return host, port, user, dbname, query
}

// hostFromURI is a one-shot host extractor for the Test() ssl warning.
func hostFromURI(uri string) string {
	h, _, _, _, _ := parsePgURI(uri)
	return h
}

// sslmodeFromURI returns the sslmode query parameter from a postgres://
// URI, or empty string if absent.
func sslmodeFromURI(uri string) string {
	_, _, _, _, q := parsePgURI(uri)
	return q["sslmode"]
}
