// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package stats

import (
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestZone(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
	}{
		{"", "UTC"},
		{"UTC", "UTC"},
		{"Europe/Berlin", "Europe/Berlin"},
		{"Not/A/Zone", "UTC"}, // fallback
		{"invalid", "UTC"},    // fallback
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			loc, name := Zone(tc.in)
			if loc == nil {
				t.Fatal("nil location")
			}
			if name != tc.wantName {
				t.Errorf("name: got %q, want %q", name, tc.wantName)
			}
		})
	}
}

func TestCheckParams(t *testing.T) {
	cases := []struct {
		days    int
		metric  string
		metrics map[string]string
		wantErr string
		wantFld string
	}{
		{7, "archive", DBMetrics, "", ""},
		{30, "data", DBMetrics, "", ""},
		{0, "records", DBMetrics, "", ""},
		{14, "archive", DBMetrics, "period:", "days"},
		{7, "nope", DBMetrics, "metric:", "metric"},
		{7, "disk", ObjectMetrics, "", ""},
		{7, "archive", ObjectMetrics, "metric:", "metric"},
	}
	for i, tc := range cases {
		err := CheckParams(tc.days, tc.metric, tc.metrics)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("case %d: unexpected error: %v", i, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("case %d: expected error, got nil", i)
			continue
		}
		var se *StatsError
		if !errors.As(err, &se) {
			t.Errorf("case %d: not *StatsError: %T", i, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("case %d: error %q missing %q", i, err.Error(), tc.wantErr)
		}
		if se.Field != tc.wantFld {
			t.Errorf("case %d: field: got %q, want %q", i, se.Field, tc.wantFld)
		}
	}
}

func TestDayOf(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	// 2026-11-13 23:30Z == 2026-11-14 00:30 local (Berlin UTC+1 CET)
	ts := time.Date(2026, 11, 13, 23, 30, 0, 0, time.UTC)
	d := dayOf(ts, loc)
	if d.Year() != 2026 || d.Month() != 11 || d.Day() != 14 {
		t.Errorf("Berlin day: got %v", d)
	}
	d = dayOf(ts, time.UTC)
	if d.Year() != 2026 || d.Month() != 11 || d.Day() != 13 {
		t.Errorf("UTC day: got %v", d)
	}
}

func TestCarryForward(t *testing.T) {
	// Observations on days 1, 3, 10 (July 2026).
	obs := map[string]float64{
		"2026-07-01": 100,
		"2026-07-03": 200,
		"2026-07-10": 300,
	}
	days := []time.Time{
		d(2026, 7, 1), d(2026, 7, 2), d(2026, 7, 3), d(2026, 7, 4),
		d(2026, 7, 10), d(2026, 7, 11), d(2026, 7, 15),
	}
	out := carryForward(obs, days, 30)
	// Expected: 100, 100, 200, 200, 300, 300, 300
	wantVals := []float64{100, 100, 200, 200, 300, 300, 300}
	for i, v := range out {
		if v == nil {
			t.Errorf("i=%d: nil, want %v", i, wantVals[i])
			continue
		}
		if *v != wantVals[i] {
			t.Errorf("i=%d: got %v, want %v", i, *v, wantVals[i])
		}
	}
	// Staleness: day 2026-08-15 is 36 days after last obs → nil at stale=30
	out2 := carryForward(obs, []time.Time{d(2026, 8, 15)}, 30)
	if len(out2) != 1 || out2[0] != nil {
		t.Errorf("stale: got %v, want nil", out2[0])
	}
	// stale=-1 disables cutoff
	out3 := carryForward(obs, []time.Time{d(2026, 8, 15)}, -1)
	if out3[0] == nil || *out3[0] != 300 {
		t.Errorf("stale disabled: got %v, want 300", out3[0])
	}
	// No observation at or before the first day: nil until the first obs.
	out4 := carryForward(obs, []time.Time{d(2026, 6, 30), d(2026, 7, 1)}, 30)
	if out4[0] != nil {
		t.Errorf("before-first: got %v, want nil", *out4[0])
	}
	if out4[1] == nil || *out4[1] != 100 {
		t.Errorf("at-first: got %v, want 100", out4[1])
	}
}

func d(y int, m time.Month, dd int) time.Time {
	return time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)
}

func TestSumPtrs(t *testing.T) {
	v := func(f float64) *float64 { return &f }
	cases := []struct {
		in   []*float64
		want *float64
	}{
		{nil, nil},
		{[]*float64{nil, nil}, nil},
		{[]*float64{v(1), v(2), v(3)}, v(6)},
		{[]*float64{v(1), nil, v(3)}, v(4)},
	}
	for i, tc := range cases {
		got := sumPtrs(tc.in)
		if (got == nil) != (tc.want == nil) {
			t.Errorf("case %d: nil mismatch", i)
			continue
		}
		if got != nil && *got != *tc.want {
			t.Errorf("case %d: got %v, want %v", i, *got, *tc.want)
		}
	}
}

func TestPick(t *testing.T) {
	cases := []struct {
		n, limit int
		want     []int
	}{
		{0, 10, []int{}},
		{5, 10, []int{0, 1, 2, 3, 4}},
		{10, 10, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}},
		{20, 10, nil}, // check only the last == n-1
	}
	for i, tc := range cases {
		got := pick(tc.n, tc.limit)
		if tc.want != nil {
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("case %d: got %v, want %v", i, got, tc.want)
			}
			continue
		}
		if len(got) == 0 || got[len(got)-1] != tc.n-1 {
			t.Errorf("case %d: last should be %d, got %v", i, tc.n-1, got)
		}
		if len(got) > tc.limit {
			t.Errorf("case %d: length %d > limit %d", i, len(got), tc.limit)
		}
		if !sort.IntsAreSorted(got) {
			t.Errorf("case %d: not sorted: %v", i, got)
		}
	}
}

func TestDeltaBack(t *testing.T) {
	v := func(f float64) *float64 { return &f }
	cases := []struct {
		vals    []*float64
		back    int
		wantAbs float64
		wantPct *float64
		wantNil bool
	}{
		{[]*float64{v(100), v(110)}, 1, 10, p(10), false},
		{[]*float64{v(100), nil}, 1, 0, nil, true},
		{[]*float64{nil, v(110)}, 1, 0, nil, true},
		{[]*float64{v(100)}, 1, 0, nil, true},
		{[]*float64{v(0), v(100)}, 1, 100, nil, false}, // prev=0 → pct nil
	}
	for i, tc := range cases {
		got := deltaBack(tc.vals, tc.back)
		if tc.wantNil {
			if got != nil {
				t.Errorf("case %d: expected nil, got %+v", i, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("case %d: unexpected nil", i)
			continue
		}
		if got.Abs != tc.wantAbs {
			t.Errorf("case %d: abs got %v, want %v", i, got.Abs, tc.wantAbs)
		}
		if (got.Pct == nil) != (tc.wantPct == nil) {
			t.Errorf("case %d: pct nil mismatch", i)
			continue
		}
		if got.Pct != nil && math.Abs(*got.Pct-*tc.wantPct) > 1e-9 {
			t.Errorf("case %d: pct got %v, want %v", i, *got.Pct, *tc.wantPct)
		}
	}
}

func p(f float64) *float64 { return &f }

func TestPercentile(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5}
	if math.Abs(percentile(sorted, 0.5)-3) > 1e-9 {
		t.Errorf("p50: got %v", percentile(sorted, 0.5))
	}
	if math.Abs(percentile(sorted, 0)-1) > 1e-9 {
		t.Errorf("p0: got %v", percentile(sorted, 0))
	}
	if math.Abs(percentile(sorted, 1)-5) > 1e-9 {
		t.Errorf("p100: got %v", percentile(sorted, 1))
	}
	// linear interpolation at p25 between 1 and 2
	got := percentile(sorted, 0.25)
	// Python _percentile([1..5], 0.25) = 2.0 (k=(5-1)*0.25=1 → sorted[1]=2)
	if math.Abs(got-2.0) > 1e-9 {
		t.Errorf("p25: got %v", got)
	}
}

func TestForecastBelowMinPoints(t *testing.T) {
	pts := [][2]float64{{0, 1}, {1, 2}}
	if f := Forecast(pts, 1, []int{30, 90}); f != nil {
		t.Errorf("expected nil forecast below MinForecastPts")
	}
}

func TestForecastLinear(t *testing.T) {
	// Perfect line y = 2x over 20 points.
	pts := make([][2]float64, 20)
	for i := 0; i < 20; i++ {
		pts[i] = [2]float64{float64(i), 2 * float64(i)}
	}
	f := Forecast(pts, 19, []int{30, 90})
	if f == nil {
		t.Fatal("expected forecast")
	}
	if math.Abs(f.SlopePerDay-2) > 1e-9 {
		t.Errorf("slope: got %v, want 2", f.SlopePerDay)
	}
	// At x=19 the base is y=38; horizon 30 → 38 + 60 = 98
	if math.Abs(f.H30.Value-98) > 1e-9 {
		t.Errorf("h30: got %v, want 98", f.H30.Value)
	}
}

func TestProjectStatsEndToEnd(t *testing.T) {
	// Two databases, one measurement per day for 10 days (newest last).
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []DbSnapshot{}
	for day := 0; day < 10; day++ {
		ts := now.Add(time.Duration(-(9-day)*24) * time.Hour)
		snaps = append(
			snaps,
			DbSnapshot{
				ID: int64(day*2 + 1), Project: "p", Database: "users", Engine: "mongodb",
				Ts:           ts,
				Status:       "ok",
				ArchiveBytes: int64(1000 + day*100),
				DataBytes:    i64(int64(2000 + day*200)),
				Records:      i64(int64(100 + day*5)),
			},
			DbSnapshot{
				ID: int64(day*2 + 2), Project: "p", Database: "logs", Engine: "mongodb",
				Ts:           ts,
				Status:       "ok",
				ArchiveBytes: int64(500 + day*50),
				DataBytes:    i64(int64(1500 + day*100)),
				Records:      i64(int64(500 + day*20)),
			},
		)
	}
	resp, err := ProjectStats(ProjectStatsInput{
		Project: "p", Days: 7, Metric: "archive", TZ: "UTC",
		Now: now, Snapshots: snaps,
	})
	if err != nil {
		t.Fatalf("ProjectStats: %v", err)
	}
	if resp.Days != 7 {
		t.Errorf("days: %d", resp.Days)
	}
	if resp.Unit != "bytes" {
		t.Errorf("unit: %q", resp.Unit)
	}
	if resp.Summary.Current == nil {
		t.Fatalf("current is nil")
	}
	// Day-9 totals: users=1900 + logs=950 = 2850
	if *resp.Summary.Current != 2850 {
		t.Errorf("current: got %v, want 2850", *resp.Summary.Current)
	}
	// Databases entity list sorted by current desc: users first (1900 > 950)
	if len(resp.Databases) != 2 {
		t.Fatalf("databases len %d", len(resp.Databases))
	}
	if resp.Databases[0].Name != "users" {
		t.Errorf("first db: %q", resp.Databases[0].Name)
	}
	// Last7 has exactly 7 entries
	if len(resp.Last7) != 7 {
		t.Errorf("last7 len: %d", len(resp.Last7))
	}
	// records metric
	resp2, err := ProjectStats(ProjectStatsInput{
		Project: "p", Days: 30, Metric: "records", TZ: "UTC",
		Now: now, Snapshots: snaps,
	})
	if err != nil {
		t.Fatalf("records metric: %v", err)
	}
	if resp2.Unit != "count" {
		t.Errorf("unit: %q", resp2.Unit)
	}
}

func TestObjectHistoryShape(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []DbSnapshot{
		{ID: 1, Project: "p", Database: "users", Ts: now.Add(-48 * time.Hour), DataBytes: i64(1000), Partial: false},
		{ID: 2, Project: "p", Database: "users", Ts: now.Add(-24 * time.Hour), DataBytes: i64(1500), Partial: false},
	}
	samples := []ObjectSample{
		{SnapshotID: 1, Name: "a", Kind: "collection", DataBytes: i64(600), Records: i64(10)},
		{SnapshotID: 1, Name: "b", Kind: "collection", DataBytes: i64(400), Records: i64(20)},
		{SnapshotID: 2, Name: "a", Kind: "collection", DataBytes: i64(900), Records: i64(15)},
		// b missing from snap 2 and snap 2 is non-partial → b→0
	}
	resp, err := ObjectHistory(ObjectHistoryInput{
		Project: "p", Database: "users", Days: 7, TZ: "UTC",
		Now: now, Snapshots: snaps, Samples: samples,
	})
	if err != nil {
		t.Fatalf("ObjectHistory: %v", err)
	}
	if len(resp.Days) != 7 {
		t.Errorf("days len: %d", len(resp.Days))
	}
	if _, ok := resp.Objects["a"]; !ok {
		t.Error("missing object a")
	}
	if _, ok := resp.Objects["b"]; !ok {
		t.Error("missing object b")
	}
	// days[0..6] = 2026-11-07..13. Snap1 at today-2 (index 4) holds b=400;
	// snap2 at today-1 (index 5) is non-partial without b → 0.
	b := resp.Objects["b"]
	if b.Data[4] == nil || *b.Data[4] != 400 {
		t.Errorf("b.data[4]: got %v, want 400", ptrVal(b.Data[4]))
	}
	if b.Data[5] == nil || *b.Data[5] != 0 {
		t.Errorf("b.data[5]: got %v, want 0 (dropped in non-partial)", ptrVal(b.Data[5]))
	}
	if b.Data[6] != nil {
		t.Errorf("b.data[6]: got %v, want nil (no snapshot today)", ptrVal(b.Data[6]))
	}
}

func TestObjectStatsArchiveMetricSubstitutes(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []DbSnapshot{
		{ID: 1, Project: "p", Database: "users", Ts: now.Add(-24 * time.Hour), DataBytes: i64(1000)},
	}
	samples := []ObjectSample{
		{SnapshotID: 1, Name: "a", Kind: "collection", DataBytes: i64(600)},
	}
	resp, err := ObjectStats(ObjectStatsInput{
		Project: "p", Database: "users", Days: 7, Metric: "archive", TZ: "UTC",
		Now: now, Snapshots: snaps, Samples: samples,
	})
	if err != nil {
		t.Fatalf("ObjectStats: %v", err)
	}
	if resp.Metric != "data" {
		t.Errorf("metric: got %q, want data (archive substituted)", resp.Metric)
	}
	if resp.Note == nil {
		t.Error("expected note about archive substitution")
	}
}

func TestSpanShape(t *testing.T) {
	today := d(2026, 11, 13)
	// days=7 → span covers at least FitDays+1 (91) days, periodStart at length-7
	sp, ps := span(7, nil, today, time.UTC)
	if len(sp) < FitDays+1 {
		t.Errorf("span too short: %d", len(sp))
	}
	if ps != len(sp)-7 {
		t.Errorf("period start: got %d, want %d", ps, len(sp)-7)
	}
	// days=0 with first: span starts at first (or earlier for fit window)
	first := d(2026, 1, 1)
	sp2, ps2 := span(0, &first, today, time.UTC)
	if sp2[0].After(first) {
		t.Errorf("span doesn't reach first: starts at %v, first %v", sp2[0], first)
	}
	// periodStart points at or after `first`
	if ps2 < 0 || ps2 >= len(sp2) {
		t.Errorf("period start OOB: %d / %d", ps2, len(sp2))
	}
}

func i64(v int64) *int64 { return &v }

func ptrVal[T any](p *T) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}
