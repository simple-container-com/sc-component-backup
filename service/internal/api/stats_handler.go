// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/stats"
)

// Slice 1e.3: wire the stats aggregator (service/internal/backup/stats,
// 870941f) to HTTP + the SnapshotStore (09f4335 + this slice). The SPA's
// client.ts calls these three endpoints under /api/v1/projects/{name}.
//
// `/api/v1/projects/{name}/stats` — per-project day-by-day series
// `/api/v1/projects/{name}/stats/objects` — per-collection/table state
// `/api/v1/projects/{name}/stats/object-history` — per-object history
//
// Query-param validation goes through stats.CheckParams so handler +
// aggregator reject the same bad inputs. On a *stats.StatsError we
// return 400 with {error, field}; other failures are 500 with a plain
// error string.

// StatsConfig holds handler dependencies.
type StatsConfig struct {
	Snapshots scheduler.SnapshotStore
	Runs      scheduler.RunStore
}

// ProjectStats is GET /api/v1/projects/{name}/stats.
func (c *StatsConfig) ProjectStats(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	days, badDays := parseDays(r.URL.Query().Get("days"))
	if badDays != "" {
		writeStatsFieldError(w, "days", badDays)
		return
	}
	metric := r.URL.Query().Get("metric")
	tz := r.URL.Query().Get("tz")
	var database *string
	if v := r.URL.Query().Get("database"); v != "" {
		database = &v
	}

	since := cutoffFromDays(days)
	snaps, err := c.Snapshots.SnapshotsForProject(r.Context(), project, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "fetch snapshots: " + err.Error()})
		return
	}
	// stats.ProjectStats wants ASC-by-Ts snapshots (the store returns
	// DESC); the slice flip keeps the aggregator happy.
	reverseSnapshots(snaps)
	// Narrow by database if requested — the store returns the whole
	// project so Object* consumers can read neighbours; ProjectStats
	// expects pre-filtered rows per its doc comment.
	if database != nil {
		snaps = filterSnapshotsByDatabase(snaps, *database)
	}

	results, err := c.Snapshots.RunResultsForProject(r.Context(), project, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "fetch run results: " + err.Error()})
		return
	}

	resp, err := stats.ProjectStats(stats.ProjectStatsInput{
		Project: project, Days: days, Metric: metric, TZ: tz,
		Database:  database,
		Snapshots: snaps,
		DbResults: results,
	})
	if err != nil {
		var se *stats.StatsError
		if errors.As(err, &se) {
			writeStatsFieldError(w, se.Field, se.Message)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ObjectStats is GET /api/v1/projects/{name}/stats/objects.
func (c *StatsConfig) ObjectStats(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	database := r.URL.Query().Get("database")
	if database == "" {
		writeStatsFieldError(w, "database", "required")
		return
	}
	days, badDays := parseDays(r.URL.Query().Get("days"))
	if badDays != "" {
		writeStatsFieldError(w, "days", badDays)
		return
	}
	metric := r.URL.Query().Get("metric")
	tz := r.URL.Query().Get("tz")

	since := cutoffFromDays(days)
	snaps, err := c.Snapshots.SnapshotsForProject(r.Context(), project, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "fetch snapshots: " + err.Error()})
		return
	}
	reverseSnapshots(snaps)
	snaps = filterSnapshotsByDatabase(snaps, database)
	samples, err := c.Snapshots.ObjectSamplesForProject(r.Context(), project, database, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "fetch samples: " + err.Error()})
		return
	}

	resp, err := stats.ObjectStats(stats.ObjectStatsInput{
		Project: project, Database: database,
		Days: days, Metric: metric, TZ: tz,
		Snapshots: snaps, Samples: samples,
	})
	if err != nil {
		var se *stats.StatsError
		if errors.As(err, &se) {
			writeStatsFieldError(w, se.Field, se.Message)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ObjectHistory is GET /api/v1/projects/{name}/stats/object-history.
func (c *StatsConfig) ObjectHistory(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	database := r.URL.Query().Get("database")
	if database == "" {
		writeStatsFieldError(w, "database", "required")
		return
	}
	days, badDays := parseDays(r.URL.Query().Get("days"))
	if badDays != "" {
		writeStatsFieldError(w, "days", badDays)
		return
	}
	// ObjectHistory refuses days==0 (per the Python reference); the
	// stats.ObjectHistory call enforces it again so the SPA's TODO on
	// the all-time case lands one error, not two.
	tz := r.URL.Query().Get("tz")

	since := cutoffFromDays(days)
	snaps, err := c.Snapshots.SnapshotsForProject(r.Context(), project, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "fetch snapshots: " + err.Error()})
		return
	}
	reverseSnapshots(snaps)
	snaps = filterSnapshotsByDatabase(snaps, database)
	samples, err := c.Snapshots.ObjectSamplesForProject(r.Context(), project, database, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "fetch samples: " + err.Error()})
		return
	}

	resp, err := stats.ObjectHistory(stats.ObjectHistoryInput{
		Project: project, Database: database,
		Days: days, TZ: tz,
		Snapshots: snaps, Samples: samples,
	})
	if err != nil {
		var se *stats.StatsError
		if errors.As(err, &se) {
			writeStatsFieldError(w, se.Field, se.Message)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseDays accepts 7 / 30 / 90 / 0. "" is rejected — the SPA always
// sends a value.
func parseDays(raw string) (int, string) {
	if raw == "" {
		return 0, "period: 7, 30, 90 or 0 (all time)"
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, "period: 7, 30, 90 or 0 (all time)"
	}
	for _, ok := range []int{7, 30, 90, 0} {
		if n == ok {
			return n, ""
		}
	}
	return 0, "period: 7, 30, 90 or 0 (all time)"
}

// cutoffFromDays returns the Ts floor for a store query. Zero means
// "all time" — a zero time disables the cutoff in the store.
func cutoffFromDays(days int) time.Time {
	if days == 0 {
		return time.Time{}
	}
	return time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
}

// reverseSnapshots flips the newest-first slice the store returns into
// the oldest-first ordering stats.ProjectStats + stats.ObjectStats +
// stats.ObjectHistory all expect.
func reverseSnapshots(s []stats.DbSnapshot) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func filterSnapshotsByDatabase(in []stats.DbSnapshot, database string) []stats.DbSnapshot {
	out := make([]stats.DbSnapshot, 0, len(in))
	for _, s := range in {
		if s.Database == database {
			out = append(out, s)
		}
	}
	return out
}

func writeStatsFieldError(w http.ResponseWriter, field, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "field": field})
}
