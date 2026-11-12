// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package retention — archive naming + Grandfather-Father-Son rotation.
//
// Pure functions over S3 object keys. Port of local-db-backup
// db_backup/archives.py (slice 1e of sc-components-platform), kept pure
// so callers can reason about retention without hitting S3.
//
// Layout: <project>/<db-enc>/<UTC ts><suffix>
// Globals at <project>/@globals/<ts>.sql.gz.
// <db-enc> is the database name percent-encoded so any name is one safe
// path segment.
package retention

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// TimestampFormat is the archive filename's UTC timestamp shape: an
// RFC-3339-ish compact form "20060102T150405Z" (Python's strftime
// "%Y%m%dT%H%M%SZ"). Sortable lexicographically = chronologically.
const TimestampFormat = "20060102T150405Z"

// Globals is the reserved segment for cluster-globals archives (Postgres).
const Globals = "@globals"

// GlobalsSuffix is the only suffix globals archives carry.
const GlobalsSuffix = ".sql.gz"

// Suffixes are the archive suffixes this component produces. Known to
// the retention code so foreign files (operator uploads, backup-in-progress
// `.partial`) are left alone.
var Suffixes = []string{
	".archive.gz", // mongodump --archive --gzip
	".pgdump",     // pg_dump --format=custom
	".sql.gz",     // mariadb-dump | gzip + globals
	".tar.gz",     // reserved for future per-dir dumps
}

// Retention is the set of rules; an archive survives iff ANY rule keeps
// it. The newest archive always survives (see KeepReasons).
//
// Zero values disable the corresponding rule. Default Last=7 matches the
// Python reference.
type Retention struct {
	// Last keeps the N most recent archives regardless of date buckets.
	Last int `json:"last" yaml:"last"`
	// Daily keeps the newest archive of each of the last N days that
	// have at least one backup.
	Daily int `json:"daily" yaml:"daily"`
	// Weekly keeps the newest archive of each of the last N ISO weeks.
	Weekly int `json:"weekly" yaml:"weekly"`
	// Monthly keeps the newest archive of each of the last N months.
	Monthly int `json:"monthly" yaml:"monthly"`
}

// Default returns a sensible starting Retention — same shape as the
// Python reference's defaults (7 last, no date buckets).
func Default() Retention {
	return Retention{Last: 7}
}

// Enc percent-encodes a database name so any value becomes one safe path
// segment. `.` and `..` get special treatment because those would
// collide with filesystem/key traversal semantics.
func Enc(name string) string {
	if name == "." || name == ".." {
		return strings.ReplaceAll(name, ".", "%2E")
	}
	// Match Python's quote(safe="-_.") — leave hyphen, underscore, and
	// dot unescaped; everything else gets percent-encoded.
	return pathSegmentEscape(name)
}

// Dec reverses Enc.
func Dec(segment string) string {
	u, err := url.PathUnescape(segment)
	if err != nil {
		return segment
	}
	return u
}

// Basename returns the last `/`-separated segment of key.
func Basename(key string) string {
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return key
	}
	return key[i+1:]
}

// SplitSuffix tries each known suffix in order. Returns the base + suffix
// + true, or "" + "" + false if no known suffix matches.
func SplitSuffix(name string) (string, string, bool) {
	for _, s := range Suffixes {
		if strings.HasSuffix(name, s) {
			return name[:len(name)-len(s)], s, true
		}
	}
	return "", "", false
}

// ParseTS extracts the archive timestamp from a key. The ".partial"
// suffix an in-flight dump wears (cf. the Python engines) is NOT
// recognised — ParseTS returns false for those, which is what we want:
// they are not finished archives and must not participate in retention
// decisions.
func ParseTS(key string) (time.Time, bool) {
	base := Basename(key)
	prefix, _, ok := SplitSuffix(base)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(TimestampFormat, prefix)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// FormatTS is the inverse of ParseTS — produce an archive timestamp
// segment from a UTC instant.
func FormatTS(t time.Time) string {
	return t.UTC().Format(TimestampFormat)
}

// KeepReasons applies the Grandfather-Father-Son rotation to a key set.
// Returns, per input key, the rules that would keep it. A key with an
// empty reason list is a candidate for deletion.
//
// Rules, evaluated in this order:
//
//   - "last"    — the N most recent dated archives (N = r.Last).
//   - "newest"  — the single newest archive, if "last" didn't already
//     catch it (i.e. r.Last == 0). Guarantees at least one survivor.
//   - "daily"   — the newest archive of each of the N most recent
//     calendar days that have at least one backup (N = r.Daily).
//   - "weekly"  — the newest of each of the N most recent ISO weeks.
//   - "monthly" — the newest of each of the N most recent (year, month)
//     pairs.
//   - "foreign" — the key has no known suffix or an unparseable
//     timestamp. Never touch files we did not name.
//
// Two implementation points worth flagging:
//
//   - The iteration order over each rule's bucket is newest-first. The
//     first archive seen in a bucket (which is the newest by sort) wins
//     that bucket and gets the rule; subsequent archives in the same
//     bucket don't accumulate the rule — only the one chosen to
//     represent the bucket does. This matches the Python reference.
//
//   - ISO weeks come from time.ISOWeek() — the ISO 8601 definition
//     where week 1 is the week containing the year's first Thursday.
//     Python's datetime.isocalendar() returns the same shape. A
//     December 31st that lands in ISO week 1 of the next year buckets
//     with next year's January; a January 1st that lands in ISO week
//     52 or 53 of the previous year buckets with the previous
//     December. The (year, week) tuple handles the year-boundary case.
func KeepReasons(keys []string, r Retention) map[string][]string {
	type dated struct {
		key string
		t   time.Time
	}
	ds := make([]dated, 0, len(keys))
	reasons := make(map[string][]string, len(keys))
	for _, k := range keys {
		reasons[k] = nil
		if t, ok := ParseTS(k); ok {
			ds = append(ds, dated{k, t})
		}
	}
	// Sort newest-first.
	sort.Slice(ds, func(i, j int) bool { return ds[i].t.After(ds[j].t) })

	// "last" rule.
	for i, d := range ds {
		if i < r.Last {
			reasons[d.key] = append(reasons[d.key], "last")
		}
	}
	// "newest" — the newest one always survives if nothing else caught it.
	if len(ds) > 0 && len(reasons[ds[0].key]) == 0 {
		reasons[ds[0].key] = append(reasons[ds[0].key], "newest")
	}

	type bucketer struct {
		rule string
		max  int
		key  func(time.Time) string
	}
	buckets := []bucketer{
		{"daily", r.Daily, func(t time.Time) string {
			y, m, d := t.Date()
			return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
		}},
		{"weekly", r.Weekly, func(t time.Time) string {
			y, w := t.ISOWeek()
			return fmt.Sprintf("%04d-W%02d", y, w)
		}},
		{"monthly", r.Monthly, func(t time.Time) string {
			y, m, _ := t.Date()
			return fmt.Sprintf("%04d-%02d", y, m)
		}},
	}
	for _, b := range buckets {
		if b.max <= 0 {
			continue
		}
		seen := map[string]bool{}
		for _, d := range ds {
			bk := b.key(d.t)
			if seen[bk] {
				continue
			}
			if len(seen) >= b.max {
				break
			}
			seen[bk] = true
			reasons[d.key] = append(reasons[d.key], b.rule)
		}
	}

	// "foreign" — anything we couldn't parse.
	for _, k := range keys {
		if _, ok := ParseTS(k); !ok {
			reasons[k] = append(reasons[k], "foreign")
		}
	}
	return reasons
}

// SelectKeep returns the subset of keys that should be retained. A key
// is kept iff KeepReasons returns a non-empty rule list for it.
func SelectKeep(keys []string, r Retention) map[string]bool {
	out := map[string]bool{}
	for k, why := range KeepReasons(keys, r) {
		if len(why) > 0 {
			out[k] = true
		}
	}
	return out
}

// Human formats a byte count for operator UI. Decimal points only
// appear from KB upwards; raw byte counts render as "<n> B".
func Human(n float64) string {
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if n < 1024 {
			if unit == "B" {
				return fmt.Sprintf("%d B", int64(n))
			}
			return fmt.Sprintf("%.1f %s", n, unit)
		}
		n /= 1024
	}
	return fmt.Sprintf("%.1f TB", n)
}

// ShrinkWarning returns a human string when a new archive is more than
// `percent` smaller than its previous one. Returns "" when the drop is
// within tolerance or when prev is zero/negative (no baseline).
//
// The guard rails this catches: an engine that silently failed partway
// (small archive, nonzero exit), a database that was truncated by hand,
// a DB rename that landed a mostly-empty namespace at the backed-up
// name. Loud but non-blocking — the archive is kept, the warning
// surfaces in the run log.
func ShrinkWarning(project, db string, prev, newSize int64, percent float64) string {
	if prev <= 0 {
		return ""
	}
	threshold := float64(prev) * (1 - percent/100)
	if float64(newSize) >= threshold {
		return ""
	}
	drop := float64(prev-newSize) / float64(prev) * 100
	return fmt.Sprintf("%s/%s: archive is smaller than the previous one: %s → %s (−%.1f%%)",
		project, db, Human(float64(prev)), Human(float64(newSize)), drop)
}

// pathSegmentEscape percent-encodes a single path segment. Matches
// Python's urllib.parse.quote(name, safe="-_.") byte-for-byte for the
// range we care about.
func pathSegmentEscape(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' || c == '_' || c == '.' ||
			(c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') {
			sb.WriteByte(c)
			continue
		}
		sb.WriteString(fmt.Sprintf("%%%02X", c))
	}
	return sb.String()
}
