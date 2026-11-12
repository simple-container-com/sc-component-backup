// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package driver

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// mysqlSystemDBs are the server-owned schemas a user-scoped list should
// hide. Matches local-db-backup/db_backup/config.py:SYSTEM_DBS["mariadb"].
var mysqlSystemDBs = map[string]struct{}{
	"mysql":              {},
	"performance_schema": {},
	"information_schema": {},
	"sys":                {},
}

// MySQLDriver shells `mariadb-dump` (falls back to `mysqldump`) with
// `--single-transaction --quick --routines --triggers --events` and
// gzips the stream in-process. Credentials travel via a 0600
// `--defaults-extra-file` whose contents is a `[client]` section.
// Query helpers (ListDatabases / Inventory / Test) shell out to the
// `mariadb`/`mysql` client — same justification as the Postgres port.
//
// Ported from local-db-backup/db_backup/engines/mariadb.py.
type MySQLDriver struct{}

func (MySQLDriver) Name() string { return "mysql" }

// RestoreCommand is the human-readable one-liner the SPA renders on
// the archive's restore tab.
func (MySQLDriver) RestoreCommand() string {
	return `{cat} | gunzip | mariadb --database="<target_db>"   # credentials via ~/.my.cnf`
}

// pickDumperClient returns the available (dumper, client) binary pair.
// Prefers mariadb-dump + mariadb; falls back to mysqldump + mysql.
// If NEITHER dumper (resp. client) is in PATH the preferred name is
// returned anyway — callers invoke requireCmd on the result to surface
// a commandNotFoundError for operator clarity.
func (MySQLDriver) pickDumperClient() (dumper, client string) {
	dumper = "mariadb-dump"
	if _, err := exec.LookPath(dumper); err != nil {
		if _, err := exec.LookPath("mysqldump"); err == nil {
			dumper = "mysqldump"
		}
	}
	client = "mariadb"
	if _, err := exec.LookPath(client); err != nil {
		if _, err := exec.LookPath("mysql"); err == nil {
			client = "mysql"
		}
	}
	return dumper, client
}

// Dump streams a mariadb-dump (or mysqldump) output through an
// in-process gzip into out. The dump is made without --databases so
// there's no CREATE DATABASE / USE and the archive restores into any
// target DB name.
//
// Streams directly; drops the "-- Dump completed" tail-check the
// Python reference does (which required buffering to a tempfile).
// Same rationale as the Postgres port: the archive goes to S3 and a
// corrupt tail surfaces on the next restore.
func (d *MySQLDriver) Dump(ctx context.Context, conn ConnectionSpec, out io.Writer) error {
	if conn.Database == "" {
		return fmt.Errorf("mysql driver: ConnectionSpec.Database is required for a dump")
	}
	if strings.HasPrefix(conn.Database, "-") {
		return fmt.Errorf("mysql driver: database name %q starts with %q and cannot be passed to mariadb-dump",
			conn.Database, "-")
	}
	dumper, _ := d.pickDumperClient()
	if err := requireCmd(dumper); err != nil {
		// Try the other common name before giving up.
		alt := "mysqldump"
		if dumper == "mysqldump" {
			alt = "mariadb-dump"
		}
		if err2 := requireCmd(alt); err2 != nil {
			return err
		}
		dumper = alt
	}
	dir, cleanup, err := privateDir()
	if err != nil {
		return err
	}
	defer cleanup()
	defaults, err := d.writeDefaults(dir, conn)
	if err != nil {
		return err
	}
	args := []string{
		"--defaults-extra-file=" + defaults,
		"--single-transaction",
		"--quick",
		"--routines",
		"--triggers",
		"--events",
		"--hex-blob",
		"--skip-lock-tables",
		"--default-character-set=utf8mb4",
		conn.Database,
	}
	cmd := exec.CommandContext(ctx, dumper, args...)
	cmd.Env = cleanEnv()

	// Pipe stdout through gzip into out.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mariadb-dump pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	gw := gzip.NewWriter(out)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mariadb-dump start: %w", err)
	}
	if _, err := io.Copy(gw, stdout); err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("mariadb-dump copy: %w; stderr tail: %s", err,
			redact(tailStderr(stderr.Bytes()), secretsFromConn(conn)))
	}
	if err := gw.Close(); err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("gzip close: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s failed: %w; stderr tail: %s", dumper, err,
			redact(tailStderr(stderr.Bytes()), secretsFromConn(conn)))
	}
	return nil
}

// Restore streams a gunzipped SQL script into the `mariadb` (or
// `mysql`) client. The target database must exist; the dump has no
// CREATE DATABASE.
func (d *MySQLDriver) Restore(ctx context.Context, conn ConnectionSpec, in io.Reader) error {
	if conn.Database == "" {
		return fmt.Errorf("mysql driver: ConnectionSpec.Database is required for a restore")
	}
	_, client := d.pickDumperClient()
	if err := requireCmd(client); err != nil {
		if err := requireCmd("mysql"); err != nil {
			return err
		}
		client = "mysql"
	}
	dir, cleanup, err := privateDir()
	if err != nil {
		return err
	}
	defer cleanup()
	defaults, err := d.writeDefaults(dir, conn)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("gzip open: %w", err)
	}
	defer gz.Close()
	args := []string{
		"--defaults-extra-file=" + defaults,
		"--default-character-set=utf8mb4",
		conn.Database,
	}
	cmd := exec.CommandContext(ctx, client, args...)
	cmd.Env = cleanEnv()
	cmd.Stdin = gz
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s restore failed: %w; stderr tail: %s", client, err,
			redact(tailStderr(stderr.Bytes()), secretsFromConn(conn)))
	}
	return nil
}

// ListDatabases returns the user-visible schemas, sorted.
func (d *MySQLDriver) ListDatabases(ctx context.Context, conn ConnectionSpec) ([]string, error) {
	rows, err := d.query(ctx, conn, "SHOW DATABASES", 30*time.Second)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r)
		if name == "" {
			continue
		}
		if _, sys := mysqlSystemDBs[name]; sys {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Inventory enumerates tables of `db` from information_schema. Row
// counts (TABLE_ROWS) are exact for MyISAM-like engines and the
// optimizer's estimate for InnoDB; we flag them via the field but do
// not change the Docs shape — the SPA interprets these as approximate
// when Kind="table".
func (d *MySQLDriver) Inventory(ctx context.Context, conn ConnectionSpec, db string) ([]CollectionStat, error) {
	if db == "" {
		return nil, fmt.Errorf("mysql driver: Inventory requires a database name")
	}
	sql := "SELECT table_name, IFNULL(table_rows, ''), IFNULL(data_length, ''), IFNULL(index_length, '')" +
		" FROM information_schema.tables WHERE table_schema = " + mysqlSQLStr(db) +
		" AND table_type = 'BASE TABLE'"
	rows, err := d.query(ctx, conn, sql, 30*time.Second)
	if err != nil {
		return nil, err
	}
	out := make([]CollectionStat, 0, len(rows))
	for _, row := range rows {
		parts := strings.Split(row, "\t")
		if len(parts) < 4 {
			continue
		}
		cs := CollectionStat{Name: mysqlUnescape(parts[0]), Kind: "table"}
		if docs, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil {
			cs.Docs = docs
		}
		if dataLen, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64); err == nil {
			cs.Bytes = dataLen
			cs.StorageBytes = dataLen
		}
		if indexLen, err := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64); err == nil {
			cs.IndexBytes = indexLen
		}
		out = append(out, cs)
	}
	// Sort largest-first on (bytes + indexBytes), then name.
	sort.SliceStable(out, func(i, j int) bool {
		li := out[i].Bytes + out[i].IndexBytes
		lj := out[j].Bytes + out[j].IndexBytes
		if li != lj {
			return li > lj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Test probes connectivity + collects version info + warns if the
// server banner suggests MySQL rather than MariaDB (mariadb-dump
// usually copes but the operator should verify a restore).
func (d *MySQLDriver) Test(ctx context.Context, conn ConnectionSpec) (TestResult, error) {
	dumper, _ := d.pickDumperClient()
	res := TestResult{
		ClientVersion: toolVersion(ctx, dumper, "--version"),
	}
	rows, err := d.query(ctx, conn, "SELECT VERSION()", 15*time.Second)
	if err != nil {
		res.Error = redact(err.Error(), secretsFromConn(conn))
		if res.ClientVersion == "" {
			res.Warnings = append(res.Warnings, dumper+" not found in the container")
		}
		return res, nil
	}
	if len(rows) > 0 {
		res.ServerVersion = strings.TrimSpace(rows[0])
	}

	dbs, lerr := d.ListDatabases(ctx, conn)
	if lerr == nil {
		res.Databases = dbs
	} else {
		res.Error = redact(lerr.Error(), secretsFromConn(conn))
	}

	if res.ClientVersion == "" {
		res.Warnings = append(res.Warnings, dumper+" not found in the container")
	}
	if !strings.Contains(strings.ToLower(res.ServerVersion), "mariadb") && res.ServerVersion != "" {
		res.Warnings = append(res.Warnings,
			"this looks like MySQL rather than MariaDB: mariadb-dump usually copes, but verify a restore")
	}
	res.OK = res.Error == ""
	return res, nil
}

// query runs a one-shot SQL via the mariadb/mysql client in --batch
// mode and returns the result rows (tab-separated, header suppressed).
func (d *MySQLDriver) query(ctx context.Context, conn ConnectionSpec, sql string, timeout time.Duration) ([]string, error) {
	_, client := d.pickDumperClient()
	if err := requireCmd(client); err != nil {
		if err := requireCmd("mysql"); err != nil {
			return nil, err
		}
		client = "mysql"
	}
	dir, cleanup, err := privateDir()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	defaults, err := d.writeDefaults(dir, conn)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--defaults-extra-file=" + defaults,
		"--batch",
		"--skip-column-names",
		"--connect-timeout=30",
		"-e", sql,
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, client, args...)
	cmd.Env = cleanEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s failed: %w; stderr tail: %s", client, err,
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

// writeDefaults writes a 0600 `[client]` config file with host/port/
// user/password + optional ssl, returning its path for use on the
// `--defaults-extra-file` option. Credentials never hit argv or env.
func (d *MySQLDriver) writeDefaults(dir string, conn ConnectionSpec) (string, error) {
	host := conn.Host
	port := conn.Port
	user := conn.User
	password := conn.Password
	ssl := ""
	if conn.URI != "" {
		uh, up, uu, upw, uq := parseMySQLURI(conn.URI)
		if host == "" {
			host = uh
		}
		if port == 0 {
			port = up
		}
		if user == "" {
			user = uu
		}
		if password == "" {
			password = upw
		}
		if v, ok := uq["ssl"]; ok {
			ssl = v
		} else if v, ok := uq["ssl-mode"]; ok {
			ssl = v
		}
	}
	if host == "" {
		return "", fmt.Errorf("mysql driver: host is empty; set ConnectionSpec.Host or supply a URI")
	}
	if user == "" {
		return "", fmt.Errorf("mysql driver: user is empty; set ConnectionSpec.User or embed one in the URI")
	}
	if port == 0 {
		port = 3306
	}
	lines := []string{
		"[client]",
		"host=" + mysqlOptQuote(host),
		"port=" + strconv.Itoa(port),
		"user=" + mysqlOptQuote(user),
		"password=" + mysqlOptQuote(password),
	}
	switch strings.ToLower(ssl) {
	case "1", "true", "yes", "on", "required", "require":
		lines = append(lines, "ssl")
	case "0", "false", "off", "disabled":
		lines = append(lines, "skip-ssl")
	}
	return writePrivateFile(dir, "my.cnf", strings.Join(lines, "\n")+"\n")
}

// mysqlOptQuote wraps v in double quotes with backslash + double-quote +
// newline escaping, matching the `--defaults-extra-file` rules.
func mysqlOptQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return `"` + v + `"`
}

// mysqlSQLStr escapes a string literal for inline SQL. Backslash and
// apostrophe are the only sequences the mariadb client parser cares
// about here; we use ” for the apostrophe (ANSI standard, safer than
// backslash-escape which depends on NO_BACKSLASH_ESCAPES mode).
func mysqlSQLStr(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "'", "''")
	return "'" + v + "'"
}

// mysqlUnescape reverses the `--batch` output escapes (\t \n \0 \\),
// so a table name that contains a tab survives roundtrip.
func mysqlUnescape(v string) string {
	var sb strings.Builder
	sb.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			switch v[i+1] {
			case 't':
				sb.WriteByte('\t')
				i++
				continue
			case 'n':
				sb.WriteByte('\n')
				i++
				continue
			case '0':
				sb.WriteByte(0)
				i++
				continue
			case '\\':
				sb.WriteByte('\\')
				i++
				continue
			}
		}
		sb.WriteByte(v[i])
	}
	return sb.String()
}

// parseMySQLURI pulls host/port/user/password + query params out of a
// mysql:// or mariadb:// URI. Minimal, same best-effort spirit as the
// Postgres one.
func parseMySQLURI(uri string) (host string, port int, user, password string, query map[string]string) {
	query = map[string]string{}
	rest := uri
	for _, prefix := range []string{"mariadb://", "mysql://"} {
		if strings.HasPrefix(rest, prefix) {
			rest = rest[len(prefix):]
			break
		}
	}
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
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}
	if idx := strings.Index(rest, "@"); idx >= 0 {
		userinfo := rest[:idx]
		rest = rest[idx+1:]
		if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
			user = userinfo[:colon]
			password = userinfo[colon+1:]
		} else {
			user = userinfo
		}
	}
	if idx := strings.LastIndex(rest, ":"); idx >= 0 {
		host = rest[:idx]
		if p, err := strconv.Atoi(rest[idx+1:]); err == nil {
			port = p
		}
	} else {
		host = rest
	}
	return host, port, user, password, query
}
