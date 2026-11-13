// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package stats

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Zone resolves an IANA zone name; falls back to UTC with name "UTC"
// when the input is empty or Go's loader rejects it. Mirrors the
// Python reference's zone(tz) helper (ZoneInfoNotFoundError / ValueError →
// UTC).
func Zone(tz string) (*time.Location, string) {
	if tz == "" {
		return time.UTC, "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || loc == nil {
		return time.UTC, "UTC"
	}
	return loc, tz
}

// CheckParams validates the (days, metric) pair against the set of
// metrics (DBMetrics or ObjectMetrics). Returns a *StatsError whose
// Field names the offending parameter — the HTTP layer surfaces it as
// a 422.
func CheckParams(days int, metric string, metrics map[string]string) error {
	ok := false
	for _, p := range Periods {
		if p == days {
			ok = true
			break
		}
	}
	if !ok {
		return &StatsError{Field: "days", Message: "period: 7, 30, 90 or 0 (all time)"}
	}
	if _, present := metrics[metric]; !present {
		names := make([]string, 0, len(metrics))
		for k := range metrics {
			names = append(names, k)
		}
		sort.Strings(names)
		return &StatsError{Field: "metric", Message: "metric: " + strings.Join(names, ", ")}
	}
	return nil
}

// dayOf converts a UTC timestamp to a calendar date in `loc`.
func dayOf(ts time.Time, loc *time.Location) time.Time {
	t := ts.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// dateKey is a sortable canonical form of a date in a given location.
func dateKey(d time.Time) string {
	return d.Format("2006-01-02")
}

// carryForward produces, per day in `days`, the latest observation at
// or before that day, carried for at most `stale` days. `stale<0`
// disables the staleness cutoff (used by object_stats where a dropped
// collection records 0 rather than expiring).
//
// The input `obs` maps date-key (yyyy-mm-dd) → value. `days` is the
// ordered list of target dates.
func carryForward(obs map[string]float64, days []time.Time, stale int) []*float64 {
	// Sort observations chronologically by key (keys are ISO yyyy-mm-dd, so
	// lexicographic order == chronological).
	type kv struct {
		day time.Time
		v   float64
	}
	items := make([]kv, 0, len(obs))
	for k, v := range obs {
		t, err := time.Parse("2006-01-02", k)
		if err == nil {
			items = append(items, kv{t.UTC(), v})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].day.Before(items[j].day) })

	out := make([]*float64, len(days))
	i := 0
	var last *float64
	var lastDay time.Time
	have := false
	for di, d := range days {
		for i < len(items) && !items[i].day.After(d) {
			v := items[i].v
			last = &v
			lastDay = items[i].day
			have = true
			i++
		}
		if !have {
			out[di] = nil
			continue
		}
		if stale >= 0 {
			diff := int(d.Sub(lastDay).Hours() / 24)
			if diff > stale {
				out[di] = nil
				continue
			}
		}
		// Copy to a fresh float so distinct slice entries don't share a pointer.
		v := *last
		out[di] = &v
	}
	return out
}

// sumPtrs sums non-nil values; returns nil iff all values are nil.
func sumPtrs(values []*float64) *float64 {
	var total float64
	any := false
	for _, v := range values {
		if v != nil {
			total += *v
			any = true
		}
	}
	if !any {
		return nil
	}
	return &total
}

// pick returns the indices of at most `limit` evenly-spread points
// including the last one. Matches the Python _pick reducer.
func pick(n, limit int) []int {
	if n <= 0 {
		return []int{}
	}
	if n <= limit {
		out := make([]int, n)
		for i := 0; i < n; i++ {
			out[i] = i
		}
		return out
	}
	step := int(math.Ceil(float64(n) / float64(limit)))
	// Walk backward from n-1 by step, then reverse.
	out := []int{}
	for i := n - 1; i >= 0; i -= step {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

// deltaBack returns the delta of the last value vs. `back` positions
// earlier. Nil if either endpoint is nil or the slice is too short.
func deltaBack(values []*float64, back int) *StatsDelta {
	if len(values) <= back {
		return nil
	}
	cur := values[len(values)-1]
	prev := values[len(values)-1-back]
	if cur == nil || prev == nil {
		return nil
	}
	diff := *cur - *prev
	d := &StatsDelta{Abs: diff}
	if *prev != 0 {
		pct := round2(diff / *prev * 100)
		d.Pct = &pct
	}
	return d
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}

// percentile returns a linear-interpolated percentile of a sorted
// slice. Mirrors Python's math.floor/ceil interpolation.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	k := float64(len(sorted)-1) * q
	lo := int(math.Floor(k))
	hi := int(math.Ceil(k))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(k-float64(lo))
}

// Forecast runs Theil-Sen regression over (x, y) points and extrapolates
// to each horizon. Returns nil if fewer than MinForecastPts points.
//
// Robust to a few outliers because it takes the median of pairwise
// slopes. The low/high come from the 10th/90th percentile slopes.
func Forecast(points [][2]float64, lastX float64, horizons []int) *StatsForecast {
	if len(points) < MinForecastPts {
		return nil
	}
	slopes := []float64{}
	for i, p1 := range points {
		for _, p2 := range points[i+1:] {
			if p2[0] == p1[0] {
				continue
			}
			slopes = append(slopes, (p2[1]-p1[1])/(p2[0]-p1[0]))
		}
	}
	if len(slopes) == 0 {
		return nil
	}
	sort.Float64s(slopes)
	slope := percentile(slopes, 0.5)
	// Theil intercept: median of (y - slope*x).
	ints := make([]float64, len(points))
	for i, p := range points {
		ints[i] = p[1] - slope*p[0]
	}
	sort.Float64s(ints)
	intercept := percentile(ints, 0.5)
	loS := percentile(slopes, 0.1)
	hiS := percentile(slopes, 0.9)
	base := intercept + slope*lastX

	fc := &StatsForecast{Points: len(points), SlopePerDay: slope}
	for _, h := range horizons {
		pt := ForecastPoint{
			Days:  h,
			Value: math.Max(0, base+slope*float64(h)),
			Low:   math.Max(0, base+loS*float64(h)),
			High:  math.Max(0, base+hiS*float64(h)),
		}
		switch h {
		case 30:
			fc.H30 = pt
		case 90:
			fc.H90 = pt
		}
	}
	return fc
}

// span returns the list of dates needed to compute the full window and
// the index at which the user-visible period starts.
func span(days int, first *time.Time, today time.Time, loc *time.Location) ([]time.Time, int) {
	need := days
	if need < FitDays+1 {
		need = FitDays + 1
	}
	start := today.AddDate(0, 0, -(need - 1))
	if days == 0 && first != nil && first.Before(start) {
		start = *first
	}
	n := int(today.Sub(start).Hours()/24) + 1
	out := make([]time.Time, n)
	for i := 0; i < n; i++ {
		d := start.AddDate(0, 0, i)
		out[i] = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	}
	var periodStart int
	if days > 0 {
		periodStart = len(out) - days
		if periodStart < 0 {
			periodStart = 0
		}
	} else if first != nil {
		diff := int(first.Sub(start).Hours() / 24)
		if diff < 0 {
			diff = 0
		}
		periodStart = diff
	} else {
		periodStart = len(out) - 1
	}
	return out, periodStart
}

// entities packs the per-database (or per-object) rows with deltas +
// spark. `extra` supplies per-row optional fields the shape wants.
func entities(
	matrix map[string][]*float64,
	periodStart int,
	sparkIdx []int,
	extra map[string]StatsEntity,
) []StatsEntity {
	out := make([]StatsEntity, 0, len(matrix))
	for name, vals := range matrix {
		var cur *float64
		if len(vals) > 0 {
			cur = vals[len(vals)-1]
		}
		var first *float64
		if periodStart < len(vals) {
			for _, v := range vals[periodStart:] {
				if v != nil {
					first = v
					break
				}
			}
		}
		var deltaPeriod *float64
		if cur != nil && first != nil {
			d := *cur - *first
			deltaPeriod = &d
		}
		spark := make([]*float64, 0, len(sparkIdx))
		for _, i := range sparkIdx {
			j := periodStart + i
			if j < 0 || j >= len(vals) {
				spark = append(spark, nil)
				continue
			}
			spark = append(spark, vals[j])
		}
		e := StatsEntity{
			Name:        name,
			Current:     cur,
			Delta7:      deltaBack(vals, 7),
			Delta30:     deltaBack(vals, 30),
			DeltaPeriod: deltaPeriod,
			Spark:       spark,
		}
		if ex, ok := extra[name]; ok {
			e.LastTS = ex.LastTS
			e.DumpSeconds = ex.DumpSeconds
			e.Approx = ex.Approx
			e.Partial = ex.Partial
			e.Kind = ex.Kind
			e.Records = ex.Records
			e.DataBytes = ex.DataBytes
			e.DiskBytes = ex.DiskBytes
			e.IndexBytes = ex.IndexBytes
		}
		out = append(out, e)
	}
	// Sort: current desc (nil at the bottom), then name asc.
	sort.Slice(out, func(i, j int) bool {
		ci := out[i].Current
		cj := out[j].Current
		if (ci == nil) != (cj == nil) {
			return cj == nil
		}
		if ci != nil && cj != nil && *ci != *cj {
			return *ci > *cj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// timeline assembles the chart's x/total/series. `periodDays` are the
// user-visible days; `idx` is a subsampled index into them.
func timeline(
	matrix map[string][]*float64,
	periodStart int,
	periodDays []time.Time,
	idx []int,
) StatsTimeline {
	// Total across all databases/objects, over the FULL span, then
	// sliced into the user-visible period.
	nFull := 0
	for _, v := range matrix {
		nFull = len(v)
		break
	}
	totalFull := make([]*float64, nFull)
	for i := 0; i < nFull; i++ {
		row := make([]*float64, 0, len(matrix))
		for _, vals := range matrix {
			if i < len(vals) {
				row = append(row, vals[i])
			}
		}
		totalFull[i] = sumPtrs(row)
	}
	totalAll := totalFull
	if periodStart < len(totalAll) {
		totalAll = totalAll[periodStart:]
	} else {
		totalAll = []*float64{}
	}

	// Sort names by current-value desc (nil last), then name.
	names := make([]string, 0, len(matrix))
	for n := range matrix {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		ci := lastVal(matrix[names[i]])
		cj := lastVal(matrix[names[j]])
		if (ci == nil) != (cj == nil) {
			return cj == nil
		}
		if ci != nil && cj != nil && *ci != *cj {
			return *ci > *cj
		}
		return names[i] < names[j]
	})
	top := names
	var rest []string
	if len(names) > TopSeries {
		top = names[:TopSeries]
		rest = names[TopSeries:]
	}

	days := make([]string, len(idx))
	for ii, i := range idx {
		days[ii] = dateKey(periodDays[i])
	}
	total := make([]*float64, len(idx))
	for ii, i := range idx {
		if i < len(totalAll) {
			total[ii] = totalAll[i]
		}
	}

	series := make([]StatsSeries, 0, len(top)+1)
	for _, name := range top {
		vals := matrix[name]
		periodVals := vals[periodStart:]
		s := StatsSeries{Name: name, Values: make([]*float64, len(idx))}
		for ii, i := range idx {
			if i < len(periodVals) {
				s.Values[ii] = periodVals[i]
			}
		}
		series = append(series, s)
	}
	if len(rest) > 0 {
		s := StatsSeries{Name: RestLabel, Values: make([]*float64, len(idx)), Rest: true}
		cnt := len(rest)
		s.Count = &cnt
		for ii, i := range idx {
			col := make([]*float64, 0, len(rest))
			for _, n := range rest {
				vals := matrix[n]
				j := periodStart + i
				if j < len(vals) {
					col = append(col, vals[j])
				}
			}
			s.Values[ii] = sumPtrs(col)
		}
		series = append(series, s)
	}

	return StatsTimeline{Days: days, Total: total, Series: series}
}

func lastVal(v []*float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	return v[len(v)-1]
}

// summary builds the summary block + the forecast over the fit window.
func summary(matrix map[string][]*float64, spanDays []time.Time, observed map[string]bool) StatsSummary {
	nFull := len(spanDays)
	total := make([]*float64, nFull)
	for i := 0; i < nFull; i++ {
		col := make([]*float64, 0, len(matrix))
		for _, vals := range matrix {
			if i < len(vals) {
				col = append(col, vals[i])
			}
		}
		total[i] = sumPtrs(col)
	}
	todayX := len(spanDays) - 1
	fitFrom := todayX - FitDays + 1
	if fitFrom < 0 {
		fitFrom = 0
	}
	pts := [][2]float64{}
	for i := fitFrom; i <= todayX; i++ {
		if observed[dateKey(spanDays[i])] && total[i] != nil {
			pts = append(pts, [2]float64{float64(i), *total[i]})
		}
	}

	s := StatsSummary{
		ForecastMinPoints: MinForecastPts,
		ObservedDays:      len(pts),
	}
	if len(total) > 0 {
		s.Current = total[len(total)-1]
	}
	s.Delta7 = deltaBack(total, 7)
	s.Delta30 = deltaBack(total, 30)
	s.Forecast = Forecast(pts, float64(todayX), []int{30, 90})
	return s
}

func base(days int, metric, tzname string, spanDays []time.Time, periodStart int, since *string, database *string) StatsBase {
	unit := "bytes"
	if metric == "records" {
		unit = "count"
	}
	periodFrom := spanDays[len(spanDays)-1]
	if periodStart < len(spanDays) {
		periodFrom = spanDays[periodStart]
	}
	return StatsBase{
		Days:       days,
		Unit:       unit,
		TZ:         tzname,
		Since:      since,
		Today:      dateKey(spanDays[len(spanDays)-1]),
		PeriodFrom: dateKey(periodFrom),
		Database:   database,
	}
}

// ProjectStatsInput bundles everything ProjectStats needs. The caller
// pre-fetches rows from its store.
type ProjectStatsInput struct {
	Project   string
	Days      int
	Metric    string
	TZ        string
	Database  *string
	Now       time.Time
	Snapshots []DbSnapshot
	// DbResults is used for the ok/failed health strip. One per per-db run.
	DbResults []DbResult
	// FirstSnapshotTS is history.first_snapshot_ts in Python — used
	// when days==0 to seed `since`. Nil → derived from Snapshots[0].
	FirstSnapshotTS *time.Time
}

// ProjectStats aggregates db_snapshots into the ProjectStats shape.
// Caller MUST have already filtered Snapshots to (project, database,
// since) per the Python reference. Rows should be ordered by Ts ASC.
func ProjectStats(in ProjectStatsInput) (*ProjectStatsResponse, error) {
	if err := CheckParams(in.Days, in.Metric, DBMetrics); err != nil {
		return nil, err
	}
	loc, tzname := Zone(in.TZ)
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	today := dayOf(now, loc)

	// Collect observations keyed by (database → date-key → value).
	obs := map[string]map[string]float64{}
	observed := map[string]bool{}
	extra := map[string]StatsEntity{}
	uploads := map[string][2]int64{} // date-key → {bytes, archives}
	dumpByDay := map[string]float64{}
	var first *time.Time
	anyApprox := false
	anyPartial := false

	for _, s := range in.Snapshots {
		d := dayOf(s.Ts, loc)
		dk := dateKey(d)
		if first == nil || d.Before(*first) {
			dc := d
			first = &dc
		}
		up := uploads[dk]
		up[0] += s.ArchiveBytes
		up[1]++
		uploads[dk] = up
		if s.DumpSeconds != nil {
			dumpByDay[dk] += *s.DumpSeconds
		}
		ex := extra[s.Database]
		ts := s.Ts.UTC().Format(time.RFC3339)
		ex.LastTS = &ts
		if s.DumpSeconds != nil {
			ds := *s.DumpSeconds
			ex.DumpSeconds = &ds
		}
		approx := s.Approx && in.Metric == "records"
		partial := s.Partial && in.Metric != "archive"
		ex.Approx = &approx
		ex.Partial = &partial
		extra[s.Database] = ex

		v, ok := metricValue(s, in.Metric)
		if !ok {
			continue
		}
		if obs[s.Database] == nil {
			obs[s.Database] = map[string]float64{}
		}
		obs[s.Database][dk] = v
		observed[dk] = true
	}
	for name, ex := range extra {
		if _, ok := obs[name]; ok {
			if ex.Approx != nil {
				anyApprox = anyApprox || *ex.Approx
			}
			if ex.Partial != nil {
				anyPartial = anyPartial || *ex.Partial
			}
		}
	}

	spanDays, periodStart := span(in.Days, first, today, loc)
	periodDays := spanDays[periodStart:]
	matrix := map[string][]*float64{}
	for name, o := range obs {
		matrix[name] = carryForward(o, spanDays, StaleDays)
	}

	idx := pick(len(periodDays), MaxPoints)
	sparkIdx := pick(len(periodDays), SparkPoints)

	sum := summary(matrix, spanDays, observed)
	dbCount := 0
	for _, v := range matrix {
		if len(v) > 0 && v[len(v)-1] != nil {
			dbCount++
		}
	}
	sum.Databases = &dbCount
	sum.Approx = anyApprox
	sum.Partial = anyPartial

	last7Days := make([]time.Time, 7)
	for i := 0; i < 7; i++ {
		last7Days[i] = today.AddDate(0, 0, -(6 - i))
	}
	last7 := make([]UploadDay, 7)
	for i, d := range last7Days {
		dk := dateKey(d)
		u := uploads[dk]
		last7[i] = UploadDay{Day: dk, Bytes: u[0], Archives: int(u[1])}
	}

	healthStart := len(periodDays) - MaxPoints
	if healthStart < 0 {
		healthStart = 0
	}
	healthDays := periodDays[healthStart:]
	// Bucket DbResults by day.
	results := map[string][2]int{} // date-key → {ok, failed}
	for _, r := range in.DbResults {
		d := dayOf(r.StartedAt, loc)
		dk := dateKey(d)
		b := results[dk]
		if r.Status == "ok" || r.Status == "unchanged" {
			b[0]++
		} else {
			b[1]++
		}
		results[dk] = b
	}
	health := make([]HealthDay, len(healthDays))
	for i, d := range healthDays {
		dk := dateKey(d)
		var ds *float64
		if v, ok := dumpByDay[dk]; ok {
			dsv := v
			ds = &dsv
		}
		r := results[dk]
		health[i] = HealthDay{Day: dk, DumpSeconds: ds, OK: r[0], Failed: r[1]}
	}

	ents := entities(matrix, periodStart, sparkIdx, extra)

	since := deriveSince(in, 0)
	b := base(in.Days, in.Metric, tzname, spanDays, periodStart, since, in.Database)
	b.Summary = sum
	b.Timeline = timeline(matrix, periodStart, periodDays, idx)
	return &ProjectStatsResponse{
		StatsBase: b,
		Metric:    in.Metric,
		Last7:     last7,
		Databases: ents,
		Health:    health,
	}, nil
}

// deriveSince picks the "since" field: Snapshots[0].Ts when days != 0,
// else history.first_snapshot_ts (passed in via FirstSnapshotTS).
func deriveSince(in ProjectStatsInput, _mode int) *string {
	if in.Days == 0 {
		if in.FirstSnapshotTS != nil {
			s := in.FirstSnapshotTS.UTC().Format(time.RFC3339)
			return &s
		}
	}
	if len(in.Snapshots) > 0 {
		s := in.Snapshots[0].Ts.UTC().Format(time.RFC3339)
		return &s
	}
	return nil
}

func metricValue(s DbSnapshot, metric string) (float64, bool) {
	switch metric {
	case "archive":
		return float64(s.ArchiveBytes), true
	case "data":
		if s.DataBytes == nil {
			return 0, false
		}
		return float64(*s.DataBytes), true
	case "records":
		if s.Records == nil {
			return 0, false
		}
		return float64(*s.Records), true
	}
	return 0, false
}

// ObjectHistoryInput bundles the inputs for ObjectHistory.
type ObjectHistoryInput struct {
	Project   string
	Database  string
	Days      int // 7 | 30 | 90
	TZ        string
	Now       time.Time
	Snapshots []DbSnapshot   // of (Project, Database), ordered by Ts ASC
	Samples   []ObjectSample // of the snapshots above
}

// ObjectHistory returns day-by-day values of every collection/table of
// one database over the last `days` local days.
func ObjectHistory(in ObjectHistoryInput) (*ObjectHistoryResponse, error) {
	if in.Days != 7 && in.Days != 30 && in.Days != 90 {
		return nil, &StatsError{Field: "days", Message: "period: 7, 30 or 90"}
	}
	loc, tzname := Zone(in.TZ)
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	today := dayOf(now, loc)
	days := make([]time.Time, in.Days)
	for i := 0; i < in.Days; i++ {
		days[i] = today.AddDate(0, 0, -(in.Days - 1 - i))
	}
	pos := map[string]int{}
	for i, d := range days {
		pos[dateKey(d)] = i
	}

	daily := map[string]DbSnapshot{}
	for _, s := range in.Snapshots {
		if s.DataBytes == nil && s.Records == nil {
			continue
		}
		dk := dateKey(dayOf(s.Ts, loc))
		if _, ok := pos[dk]; !ok {
			continue
		}
		daily[dk] = s // snapshots in ASC order → last of day wins
	}
	byID := map[int64]string{}
	for dk, s := range daily {
		byID[s.ID] = dk
	}

	objects := map[string]*ObjectHistorySeries{}
	ensure := func(name string) *ObjectHistorySeries {
		o, ok := objects[name]
		if !ok {
			o = &ObjectHistorySeries{
				Records: make([]*int64, in.Days),
				Data:    make([]*int64, in.Days),
				Disk:    make([]*int64, in.Days),
				Index:   make([]*int64, in.Days),
			}
			objects[name] = o
		}
		return o
	}
	seen := map[string]map[string]bool{}
	for _, r := range in.Samples {
		dk, ok := byID[r.SnapshotID]
		if !ok {
			continue
		}
		if seen[dk] == nil {
			seen[dk] = map[string]bool{}
		}
		seen[dk][r.Name] = true
		o := ensure(r.Name)
		i := pos[dk]
		o.Records[i] = r.Records
		o.Data[i] = r.DataBytes
		o.Disk[i] = r.DiskBytes
		o.Index[i] = r.IndexBytes
	}
	// For each complete (non-partial) snapshot, missing objects count as 0.
	for dk, s := range daily {
		if s.Partial {
			continue
		}
		for name, o := range objects {
			if seen[dk] != nil && seen[dk][name] {
				continue
			}
			i := pos[dk]
			zero := int64(0)
			o.Records[i] = &zero
			o.Data[i] = &zero
			o.Disk[i] = &zero
			o.Index[i] = &zero
		}
	}

	out := map[string]ObjectHistorySeries{}
	for name, o := range objects {
		out[name] = *o
	}
	daysStr := make([]string, len(days))
	for i, d := range days {
		daysStr[i] = dateKey(d)
	}
	return &ObjectHistoryResponse{
		TZ:       tzname,
		Database: in.Database,
		Days:     daysStr,
		Objects:  out,
	}, nil
}

// ObjectStatsInput bundles ObjectStats inputs.
type ObjectStatsInput struct {
	Project         string
	Database        string
	Days            int
	Metric          string
	TZ              string
	Now             time.Time
	Snapshots       []DbSnapshot
	Samples         []ObjectSample
	FirstSnapshotTS *time.Time
}

// ObjectStats aggregates object_samples into the ObjectStats shape.
// If Metric=="archive" the caller gets a note and data is substituted
// (archive bytes are per-DB, not per-object).
func ObjectStats(in ObjectStatsInput) (*ObjectStatsResponse, error) {
	var note *string
	metric := in.Metric
	if metric == "archive" {
		metric = "data"
		n := "archive size is not split by collection — showing data size"
		note = &n
	}
	if err := CheckParams(in.Days, metric, ObjectMetrics); err != nil {
		return nil, err
	}
	loc, tzname := Zone(in.TZ)
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	today := dayOf(now, loc)

	withInv := []DbSnapshot{}
	for _, s := range in.Snapshots {
		if s.DataBytes != nil || s.Records != nil {
			withInv = append(withInv, s)
		}
	}
	daily := map[string]DbSnapshot{}
	for _, s := range withInv {
		daily[dateKey(dayOf(s.Ts, loc))] = s
	}
	var first *time.Time
	for dk := range daily {
		t, _ := time.Parse("2006-01-02", dk)
		t = t.UTC()
		if first == nil || t.Before(*first) {
			tc := t
			first = &tc
		}
	}
	spanDays, periodStart := span(in.Days, first, today, loc)
	lo := spanDays[0]

	type keyedDay struct {
		dk string
		d  time.Time
	}
	keepDays := []keyedDay{}
	var seedBefore *keyedDay
	for dk := range daily {
		t, _ := time.Parse("2006-01-02", dk)
		t = t.UTC()
		if !t.Before(lo) {
			keepDays = append(keepDays, keyedDay{dk, t})
		} else if seedBefore == nil || t.After(seedBefore.d) {
			kd := keyedDay{dk, t}
			seedBefore = &kd
		}
	}
	sort.Slice(keepDays, func(i, j int) bool { return keepDays[i].d.Before(keepDays[j].d) })
	if seedBefore != nil {
		keepDays = append([]keyedDay{*seedBefore}, keepDays...)
	}

	chosenIDs := map[int64]string{}
	for _, kd := range keepDays {
		chosenIDs[daily[kd.dk].ID] = kd.dk
	}
	samplesBySnap := map[int64][]ObjectSample{}
	for _, r := range in.Samples {
		if _, ok := chosenIDs[r.SnapshotID]; ok {
			samplesBySnap[r.SnapshotID] = append(samplesBySnap[r.SnapshotID], r)
		}
	}

	known := map[string]bool{}
	for _, rows := range samplesBySnap {
		for _, r := range rows {
			if objectValue(r, metric) != nil {
				known[r.Name] = true
			}
		}
	}

	obs := map[string]map[string]float64{}
	kinds := map[string]StatsEntity{}
	approx := false
	for _, kd := range keepDays {
		s := daily[kd.dk]
		rows := samplesBySnap[s.ID]
		seen := map[string]bool{}
		for _, r := range rows {
			seen[r.Name] = true
			ts := s.Ts.UTC().Format(time.RFC3339)
			k := r.Kind
			kinds[r.Name] = StatsEntity{
				Name:       r.Name,
				Kind:       &k,
				Records:    r.Records,
				DataBytes:  r.DataBytes,
				DiskBytes:  r.DiskBytes,
				IndexBytes: r.IndexBytes,
				LastTS:     &ts,
			}
			v := objectValue(r, metric)
			if v != nil {
				if obs[r.Name] == nil {
					obs[r.Name] = map[string]float64{}
				}
				obs[r.Name][kd.dk] = *v
			}
		}
		if !s.Partial {
			for name := range known {
				if seen[name] {
					continue
				}
				if obs[name] == nil {
					obs[name] = map[string]float64{}
				}
				obs[name][kd.dk] = 0
			}
		}
		if s.Approx && in.Metric == "records" {
			approx = true
		}
	}

	observed := map[string]bool{}
	for _, kd := range keepDays {
		if !kd.d.Before(lo) {
			observed[kd.dk] = true
		}
	}
	periodDays := spanDays[periodStart:]
	matrix := map[string][]*float64{}
	for name, o := range obs {
		matrix[name] = carryForward(o, spanDays, -1) // stale disabled
	}
	idx := pick(len(periodDays), MaxPoints)
	sparkIdx := pick(len(periodDays), SparkPoints)

	sum := summary(matrix, spanDays, observed)
	objectsCount := 0
	for _, v := range matrix {
		if len(v) > 0 && v[len(v)-1] != nil && *v[len(v)-1] > 0 {
			objectsCount++
		}
	}
	sum.Objects = &objectsCount
	sum.Approx = approx
	// Last snapshot's partial flag.
	var lastSnap *DbSnapshot
	for _, s := range withInv {
		sc := s
		if lastSnap == nil || s.Ts.After(lastSnap.Ts) {
			lastSnap = &sc
		}
	}
	sum.Partial = lastSnap != nil && lastSnap.Partial

	ents := entities(matrix, periodStart, sparkIdx, kinds)
	// Drop entries with no current + no delta (dropped long ago).
	ents = filterEntities(ents)
	var top, rest []StatsEntity
	if len(ents) > TopObjects {
		top = ents[:TopObjects]
		rest = ents[TopObjects:]
	} else {
		top = ents
	}
	restAgg := ObjectRest{Count: len(rest)}
	restSum := make([]*float64, 0, len(rest))
	for _, e := range rest {
		restSum = append(restSum, e.Current)
	}
	restAgg.Current = sumPtrs(restSum)

	// Leaders: sort by delta_period desc, keep positive.
	type leader struct {
		name    string
		delta   float64
		current *float64
	}
	leaderList := []leader{}
	for _, e := range ents {
		if e.DeltaPeriod == nil || *e.DeltaPeriod <= 0 {
			continue
		}
		leaderList = append(leaderList, leader{e.Name, *e.DeltaPeriod, e.Current})
	}
	sort.Slice(leaderList, func(i, j int) bool { return leaderList[i].delta > leaderList[j].delta })
	if len(leaderList) > Leaders {
		leaderList = leaderList[:Leaders]
	}
	leadersOut := make([]ObjectLeader, len(leaderList))
	for i, l := range leaderList {
		leadersOut[i] = ObjectLeader{Name: l.name, Delta: l.delta, Current: l.current}
	}

	var since *string
	if in.Days == 0 && in.FirstSnapshotTS != nil {
		s := in.FirstSnapshotTS.UTC().Format(time.RFC3339)
		since = &s
	} else if len(withInv) > 0 {
		s := withInv[0].Ts.UTC().Format(time.RFC3339)
		since = &s
	}

	dbCopy := in.Database
	b := base(in.Days, metric, tzname, spanDays, periodStart, since, &dbCopy)
	b.Summary = sum
	b.Timeline = timeline(matrix, periodStart, periodDays, idx)
	return &ObjectStatsResponse{
		StatsBase: b,
		Metric:    metric,
		Note:      note,
		Objects:   top,
		Rest:      restAgg,
		Leaders:   leadersOut,
	}, nil
}

func objectValue(r ObjectSample, metric string) *float64 {
	switch metric {
	case "data":
		if r.DataBytes == nil {
			return nil
		}
		v := float64(*r.DataBytes)
		return &v
	case "disk":
		if r.DiskBytes == nil {
			return nil
		}
		v := float64(*r.DiskBytes)
		return &v
	case "records":
		if r.Records == nil {
			return nil
		}
		v := float64(*r.Records)
		return &v
	}
	return nil
}

func filterEntities(ents []StatsEntity) []StatsEntity {
	out := make([]StatsEntity, 0, len(ents))
	for _, e := range ents {
		if (e.Current != nil && *e.Current != 0) || (e.DeltaPeriod != nil && *e.DeltaPeriod != 0) {
			out = append(out, e)
		}
	}
	return out
}

// Helpers to format days + printable summaries for debug.

var _ = fmt.Sprintf // keep fmt imported in case of future diagnostics
