// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package retention

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(TimestampFormat, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestEncDec_Roundtrip(t *testing.T) {
	cases := []struct {
		in      string
		wantEnc string
		noUnesc bool // skip the dec roundtrip (dots are preserved as-is by URL unescape)
		roundOK bool
	}{
		{"users", "users", false, true},
		{"logs-2026", "logs-2026", false, true},
		{"a_b", "a_b", false, true},
		{"has space", "has%20space", false, true},
		{"has/slash", "has%2Fslash", false, true},
		{"тенант", "%D1%82%D0%B5%D0%BD%D0%B0%D0%BD%D1%82", false, true},
		{".", "%2E", true, false}, // special-cased
		{"..", "%2E%2E", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := Enc(tc.in)
			if got != tc.wantEnc {
				t.Errorf("Enc(%q): got %q, want %q", tc.in, got, tc.wantEnc)
			}
			if tc.roundOK {
				if back := Dec(got); back != tc.in {
					t.Errorf("Dec(Enc(%q)): got %q, want %q", tc.in, back, tc.in)
				}
			}
		})
	}
}

func TestSplitSuffix(t *testing.T) {
	cases := []struct {
		in         string
		wantPrefix string
		wantSuffix string
		wantOK     bool
	}{
		{"20261113T000000Z.archive.gz", "20261113T000000Z", ".archive.gz", true},
		{"20261113T000000Z.pgdump", "20261113T000000Z", ".pgdump", true},
		{"20261113T000000Z.sql.gz", "20261113T000000Z", ".sql.gz", true},
		{"20261113T000000Z.tar.gz", "20261113T000000Z", ".tar.gz", true},
		{"20261113T000000Z.archive.gz.partial", "", "", false},
		{"README.md", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			p, s, ok := SplitSuffix(tc.in)
			if p != tc.wantPrefix || s != tc.wantSuffix || ok != tc.wantOK {
				t.Errorf("SplitSuffix(%q): got (%q, %q, %v), want (%q, %q, %v)",
					tc.in, p, s, ok, tc.wantPrefix, tc.wantSuffix, tc.wantOK)
			}
		})
	}
}

func TestParseTS(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
		when   string
	}{
		{"proj/db/20261113T143015Z.archive.gz", true, "20261113T143015Z"},
		{"proj/db/20261113T143015Z.sql.gz", true, "20261113T143015Z"},
		{"proj/db/20261113T143015Z.archive.gz.partial", false, ""},
		{"proj/db/not-a-timestamp.archive.gz", false, ""},
		{"README.md", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseTS(tc.in)
			if ok != tc.wantOK {
				t.Errorf("ParseTS(%q): ok = %v, want %v", tc.in, ok, tc.wantOK)
				return
			}
			if ok && got != mustTime(tc.when) {
				t.Errorf("ParseTS(%q): got %v, want %v", tc.in, got, mustTime(tc.when))
			}
		})
	}
}

// keyAt returns a fake archive key at a given UTC timestamp.
func keyAt(prefix, when string) string {
	return fmt.Sprintf("%s/%s.archive.gz", prefix, when)
}

func TestKeepReasons_LastRule(t *testing.T) {
	keys := []string{
		keyAt("p/db", "20261113T000000Z"),
		keyAt("p/db", "20261112T000000Z"),
		keyAt("p/db", "20261111T000000Z"),
		keyAt("p/db", "20261110T000000Z"),
		keyAt("p/db", "20261109T000000Z"),
	}
	got := KeepReasons(keys, Retention{Last: 2})
	// First two (newest) have "last"; others empty.
	for i, k := range keys {
		want := []string{}
		if i < 2 {
			want = []string{"last"}
		}
		if !reflect.DeepEqual(got[k], want) {
			if i == 0 || got[k] == nil && len(want) == 0 {
				// nil vs empty slice: tolerate; our implementation uses nil
				if i >= 2 && len(got[k]) == 0 {
					continue
				}
			}
			t.Errorf("key %d (%s): got %v, want %v", i, k, got[k], want)
		}
	}
}

func TestKeepReasons_NewestAlwaysSurvives(t *testing.T) {
	keys := []string{
		keyAt("p/db", "20261113T000000Z"),
		keyAt("p/db", "20261112T000000Z"),
	}
	got := KeepReasons(keys, Retention{}) // zero rules
	if len(got[keys[0]]) == 0 || got[keys[0]][0] != "newest" {
		t.Errorf("newest archive without any rule should get 'newest'; got %v", got[keys[0]])
	}
	if len(got[keys[1]]) != 0 {
		t.Errorf("older archive with zero rules should get []; got %v", got[keys[1]])
	}
}

func TestKeepReasons_DailyBucket(t *testing.T) {
	// Two archives on 2026-11-13, one on 2026-11-12, one on 2026-11-11.
	// Daily=2 should keep the newest of each of the last 2 days (2026-11-13 + 2026-11-12).
	keys := []string{
		keyAt("p/db", "20261113T100000Z"),
		keyAt("p/db", "20261113T080000Z"),
		keyAt("p/db", "20261112T100000Z"),
		keyAt("p/db", "20261111T100000Z"),
	}
	got := KeepReasons(keys, Retention{Daily: 2})
	// Expected: [daily, "", daily, "newest" or ""]
	// Specifically: newest on 11-13 (keys[0]) + 11-12 (keys[2]) both get "daily"
	// The 2nd backup on 11-13 (keys[1]) doesn't get "daily" because the bucket was taken
	// The 11-11 backup (keys[3]) doesn't get "daily" because 2 buckets already used
	if !contains(got[keys[0]], "daily") {
		t.Errorf("newest archive on 11-13 should get 'daily'; got %v", got[keys[0]])
	}
	if contains(got[keys[1]], "daily") {
		t.Errorf("older archive on same day should NOT get 'daily'; got %v", got[keys[1]])
	}
	if !contains(got[keys[2]], "daily") {
		t.Errorf("newest archive on 11-12 should get 'daily'; got %v", got[keys[2]])
	}
	if contains(got[keys[3]], "daily") {
		t.Errorf("11-11 archive should NOT get 'daily' (bucket cap 2); got %v", got[keys[3]])
	}
}

func TestKeepReasons_WeeklyBucket(t *testing.T) {
	// Three ISO weeks: 2026-W46, 2026-W45, 2026-W44.
	// Weekly=2 keeps newest of W46 + W45.
	keys := []string{
		keyAt("p/db", "20261113T100000Z"), // ISO week 2026-W46 (Friday)
		keyAt("p/db", "20261106T100000Z"), // ISO week 2026-W45 (Friday)
		keyAt("p/db", "20261030T100000Z"), // ISO week 2026-W44 (Friday)
	}
	got := KeepReasons(keys, Retention{Weekly: 2})
	if !contains(got[keys[0]], "weekly") {
		t.Errorf("W46 archive should get 'weekly'; got %v", got[keys[0]])
	}
	if !contains(got[keys[1]], "weekly") {
		t.Errorf("W45 archive should get 'weekly'; got %v", got[keys[1]])
	}
	if contains(got[keys[2]], "weekly") {
		t.Errorf("W44 archive should NOT get 'weekly' (cap 2); got %v", got[keys[2]])
	}
}

func TestKeepReasons_MonthlyBucket(t *testing.T) {
	keys := []string{
		keyAt("p/db", "20261113T100000Z"), // Nov
		keyAt("p/db", "20261013T100000Z"), // Oct
		keyAt("p/db", "20260913T100000Z"), // Sep
		keyAt("p/db", "20260813T100000Z"), // Aug
	}
	got := KeepReasons(keys, Retention{Monthly: 3})
	for _, i := range []int{0, 1, 2} {
		if !contains(got[keys[i]], "monthly") {
			t.Errorf("key %d should get 'monthly'; got %v", i, got[keys[i]])
		}
	}
	if contains(got[keys[3]], "monthly") {
		t.Errorf("Aug archive should NOT get 'monthly' (cap 3); got %v", got[keys[3]])
	}
}

func TestKeepReasons_Foreign(t *testing.T) {
	keys := []string{
		keyAt("p/db", "20261113T100000Z"),
		"p/db/README.md",                           // no known suffix
		"p/db/20261113T100000Z.archive.gz.partial", // in-flight dump
	}
	got := KeepReasons(keys, Retention{Last: 10})
	if !contains(got[keys[1]], "foreign") {
		t.Errorf("README should be marked 'foreign'; got %v", got[keys[1]])
	}
	if !contains(got[keys[2]], "foreign") {
		t.Errorf(".partial should be marked 'foreign'; got %v", got[keys[2]])
	}
	if contains(got[keys[0]], "foreign") {
		t.Errorf("real archive should NOT be 'foreign'; got %v", got[keys[0]])
	}
}

func TestSelectKeep(t *testing.T) {
	keys := []string{
		keyAt("p/db", "20261113T100000Z"),
		keyAt("p/db", "20261112T100000Z"),
		keyAt("p/db", "20261111T100000Z"),
	}
	kept := SelectKeep(keys, Retention{Last: 2})
	if !kept[keys[0]] || !kept[keys[1]] {
		t.Errorf("Last=2 should keep newest two; kept = %v", kept)
	}
	if kept[keys[2]] {
		t.Errorf("Last=2 should NOT keep 3rd; kept = %v", kept)
	}
}

func TestHuman(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
		{1536, "1.5 KB"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := Human(tc.in); got != tc.want {
				t.Errorf("Human(%v): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestShrinkWarning(t *testing.T) {
	cases := []struct {
		name    string
		prev    int64
		newSize int64
		percent float64
		wantHas string
	}{
		{"no prev", 0, 100, 10, ""},
		{"within tolerance", 1000, 950, 10, ""},      // 5% drop, 10% tolerance
		{"below tolerance", 1000, 500, 10, "−50.0%"}, // 50% drop
		{"same size", 1000, 1000, 10, ""},
		{"bigger", 1000, 2000, 10, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShrinkWarning("p", "db", tc.prev, tc.newSize, tc.percent)
			if tc.wantHas == "" {
				if got != "" {
					t.Errorf("got %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantHas) {
				t.Errorf("got %q, want to contain %q", got, tc.wantHas)
			}
		})
	}
}

func TestFormatTS_ParseTS_Roundtrip(t *testing.T) {
	now := time.Date(2026, 11, 13, 14, 30, 15, 0, time.UTC)
	s := FormatTS(now)
	k := fmt.Sprintf("p/db/%s.archive.gz", s)
	got, ok := ParseTS(k)
	if !ok {
		t.Fatalf("parse failed for %q", k)
	}
	if !got.Equal(now) {
		t.Errorf("roundtrip: got %v, want %v", got, now)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// Sanity check: the KeepReasons order is deterministic for the same input.
func TestKeepReasons_Deterministic(t *testing.T) {
	keys := []string{
		keyAt("p/db", "20261113T100000Z"),
		keyAt("p/db", "20261112T100000Z"),
		keyAt("p/db", "20261111T100000Z"),
	}
	first := KeepReasons(keys, Retention{Last: 1, Daily: 2})
	for i := 0; i < 5; i++ {
		again := KeepReasons(keys, Retention{Last: 1, Daily: 2})
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("non-deterministic KeepReasons on iteration %d", i)
		}
	}
	// Make sure the set of kept keys is non-empty (sanity).
	kept := SelectKeep(keys, Retention{Last: 1, Daily: 2})
	if len(kept) == 0 {
		t.Fatalf("expected at least one kept archive")
	}
	keptList := []string{}
	for k := range kept {
		keptList = append(keptList, k)
	}
	sort.Strings(keptList)
	_ = keptList // alphabetical order check isn't meaningful; just ensure sort compiles
}
