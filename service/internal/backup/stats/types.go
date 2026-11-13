// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package stats — server-side aggregations for the SPA's charts.
//
// Pure port of local-db-backup/db_backup/stats.py (slice 1e.2 of
// sc-components-platform). The package operates on pre-fetched
// [DbSnapshot] + [ObjectSample] + [DbResult] slices (schema mirrors
// db_snapshots / object_samples / db_runs in the Python reference;
// see db_backup/history.py). No DB I/O here — the caller queries the
// store and hands the rows in.
//
// Shapes of the return types match the SPA's `ProjectStats` /
// `ObjectStats` / `ObjectHistory` TypeScript definitions in
// service/internal/ui/src/api/types.ts. JSON tags are snake_case to
// match the wire contract.
package stats

import "time"

// Constants that match local-db-backup/db_backup/stats.py.
const (
	StaleDays      = 30
	MaxPoints      = 400
	SparkPoints    = 30
	TopSeries      = 8
	TopObjects     = 100
	Leaders        = 5
	MinForecastPts = 14
	FitDays        = 90
	RestLabel      = "others"
)

// Periods is the set of accepted `days` query-param values; 0 = all time.
var Periods = []int{7, 30, 90, 0}

// DBMetrics maps the SPA's metric name → db_snapshots column.
var DBMetrics = map[string]string{
	"archive": "archive_bytes",
	"data":    "data_bytes",
	"records": "records",
}

// ObjectMetrics maps the SPA's object metric name → object_samples column.
var ObjectMetrics = map[string]string{
	"data":    "data_bytes",
	"disk":    "disk_bytes",
	"records": "records",
}

// StatsError carries a message and the field it refers to (matches the
// Python reference's StatsError(message, field) shape). The HTTP layer
// should render it as `{error, field}`.
type StatsError struct {
	Field   string
	Message string
}

func (e *StatsError) Error() string { return e.Message }

// DbSnapshot mirrors the `db_snapshots` row from local-db-backup. One
// measurement per stored archive; kept forever (not tied to runs
// cleanup).
type DbSnapshot struct {
	ID            int64     `json:"id"`
	Project       string    `json:"project"`
	Engine        string    `json:"engine"`
	Database      string    `json:"database"`
	RunID         *int64    `json:"run_id,omitempty"`
	Key           *string   `json:"key,omitempty"`
	Ts            time.Time `json:"ts"` // UTC
	Status        string    `json:"status"`
	ArchiveBytes  int64     `json:"archive_bytes"`
	DataBytes     *int64    `json:"data_bytes,omitempty"`
	DiskBytes     *int64    `json:"disk_bytes,omitempty"`
	IndexBytes    *int64    `json:"index_bytes,omitempty"`
	Records       *int64    `json:"records,omitempty"`
	Approx        bool      `json:"approx"`
	Partial       bool      `json:"partial"`
	DumpSeconds   *float64  `json:"dump_seconds,omitempty"`
	UploadSeconds *float64  `json:"upload_seconds,omitempty"`
	Source        string    `json:"source,omitempty"`
}

// ObjectSample mirrors the `object_samples` row. One row per
// collection/table in a snapshot.
type ObjectSample struct {
	SnapshotID int64  `json:"snapshot_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Records    *int64 `json:"records,omitempty"`
	DataBytes  *int64 `json:"data_bytes,omitempty"`
	DiskBytes  *int64 `json:"disk_bytes,omitempty"`
	IndexBytes *int64 `json:"index_bytes,omitempty"`
}

// DbResult mirrors the ok/failed counts the Python's history.db_results()
// returns — one row per per-database run attempt in the health window.
type DbResult struct {
	StartedAt time.Time `json:"started_at"`
	Status    string    `json:"status"`
}

// ---- SPA response shapes (match service/internal/ui/src/api/types.ts) ----

// StatsDelta is `{abs, pct}` — delta vs. N positions back. pct is nil
// when the previous value was zero.
type StatsDelta struct {
	Abs float64  `json:"abs"`
	Pct *float64 `json:"pct"`
}

// ForecastPoint is one entry of StatsForecast — projected value at a
// horizon in days with a low/high range from the 10th/90th percentile
// slopes.
type ForecastPoint struct {
	Days  int     `json:"days"`
	Value float64 `json:"value"`
	Low   float64 `json:"low"`
	High  float64 `json:"high"`
}

// StatsForecast — Theil-Sen regression result. Nil when there are fewer
// than MinForecastPts observed days.
type StatsForecast struct {
	Points      int           `json:"points"`
	SlopePerDay float64       `json:"slope_per_day"`
	H30         ForecastPoint `json:"h30"`
	H90         ForecastPoint `json:"h90"`
}

// StatsSummary is the top-of-page block: current value, deltas, forecast.
type StatsSummary struct {
	Current           *float64       `json:"current"`
	Delta7            *StatsDelta    `json:"delta7"`
	Delta30           *StatsDelta    `json:"delta30"`
	Forecast          *StatsForecast `json:"forecast"`
	ObservedDays      int            `json:"observed_days"`
	ForecastMinPoints int            `json:"forecast_min_points"`
	Approx            bool           `json:"approx"`
	Partial           bool           `json:"partial"`
	Databases         *int           `json:"databases,omitempty"`
	Objects           *int           `json:"objects,omitempty"`
}

// StatsSeries is one line on a chart. `rest=true` marks the aggregated
// remainder bucket.
type StatsSeries struct {
	Name   string     `json:"name"`
	Values []*float64 `json:"values"`
	Rest   bool       `json:"rest,omitempty"`
	Count  *int       `json:"count,omitempty"`
}

// StatsTimeline is the x-axis (days) + total series + top-N named
// series + optional `others` bucket.
type StatsTimeline struct {
	Days   []string      `json:"days"`
	Total  []*float64    `json:"total"`
	Series []StatsSeries `json:"series"`
}

// StatsEntity is one row in the databases/objects table: current value,
// deltas, spark line. Fields past `Spark` are optional and only set
// for the shape that needs them.
type StatsEntity struct {
	Name        string      `json:"name"`
	Current     *float64    `json:"current"`
	Delta7      *StatsDelta `json:"delta7"`
	Delta30     *StatsDelta `json:"delta30"`
	DeltaPeriod *float64    `json:"delta_period"`
	Spark       []*float64  `json:"spark"`
	LastTS      *string     `json:"last_ts,omitempty"`
	DumpSeconds *float64    `json:"dump_seconds,omitempty"`
	Approx      *bool       `json:"approx,omitempty"`
	Partial     *bool       `json:"partial,omitempty"`
	Kind        *string     `json:"kind,omitempty"`
	Records     *int64      `json:"records,omitempty"`
	DataBytes   *int64      `json:"data_bytes,omitempty"`
	DiskBytes   *int64      `json:"disk_bytes,omitempty"`
	IndexBytes  *int64      `json:"index_bytes,omitempty"`
}

// StatsBase is the common envelope for ProjectStats + ObjectStats.
type StatsBase struct {
	Days       int           `json:"days"`
	Unit       string        `json:"unit"` // "bytes" | "count"
	TZ         string        `json:"tz"`
	Since      *string       `json:"since"`
	Today      string        `json:"today"`
	PeriodFrom string        `json:"period_from"`
	Database   *string       `json:"database"`
	Summary    StatsSummary  `json:"summary"`
	Timeline   StatsTimeline `json:"timeline"`
}

// ProjectStatsResponse is GET /api/v1/projects/{name}/stats.
type ProjectStatsResponse struct {
	StatsBase
	Metric    string        `json:"metric"`
	Last7     []UploadDay   `json:"last7"`
	Databases []StatsEntity `json:"databases"`
	Health    []HealthDay   `json:"health"`
}

// UploadDay is one entry in the last7 strip: bytes + archive count per
// local day.
type UploadDay struct {
	Day      string `json:"day"`
	Bytes    int64  `json:"bytes"`
	Archives int    `json:"archives"`
}

// HealthDay is one entry in the dump-health strip: dump seconds + ok/
// failed counts per local day.
type HealthDay struct {
	Day         string   `json:"day"`
	DumpSeconds *float64 `json:"dump_seconds"`
	OK          int      `json:"ok"`
	Failed      int      `json:"failed"`
}

// ObjectStatsResponse is GET /api/v1/projects/{name}/stats/objects.
type ObjectStatsResponse struct {
	StatsBase
	Metric  string         `json:"metric"`
	Note    *string        `json:"note"`
	Objects []StatsEntity  `json:"objects"`
	Rest    ObjectRest     `json:"rest"`
	Leaders []ObjectLeader `json:"leaders"`
}

// ObjectRest is the aggregated remainder of the object list.
type ObjectRest struct {
	Count   int      `json:"count"`
	Current *float64 `json:"current"`
}

// ObjectLeader is one entry in the "top growth" strip.
type ObjectLeader struct {
	Name    string   `json:"name"`
	Delta   float64  `json:"delta"`
	Current *float64 `json:"current"`
}

// ObjectHistoryResponse is GET /api/v1/projects/{name}/stats/object-history.
type ObjectHistoryResponse struct {
	TZ       string                         `json:"tz"`
	Database string                         `json:"database"`
	Days     []string                       `json:"days"`
	Objects  map[string]ObjectHistorySeries `json:"objects"`
}

// ObjectHistorySeries carries the four daily columns per object.
type ObjectHistorySeries struct {
	Records []*int64 `json:"records"`
	Data    []*int64 `json:"data"`
	Disk    []*int64 `json:"disk"`
	Index   []*int64 `json:"index"`
}
