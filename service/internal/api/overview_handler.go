// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// Overview shapes mirror service/internal/ui/src/api/types.ts:139. Returned
// by the home page's first query.
//
// Slice 1e.2 scope: populate the Projects list from tenants + each
// tenant's latest run; stub the Running / Storage / Alerts sections
// minimally. A runtime-wire smoke will tell us which fields matter —
// those get populated on slice 1e.3.

type OverviewResponse struct {
	Projects []ProjectSummary `json:"projects"`
	Running  []RunningOp      `json:"running"`
	Storage  StorageStatus    `json:"storage"`
	Alerts   []Alert          `json:"alerts"`
}

type ProjectSummary struct {
	Name          string      `json:"name"`
	Engine        string      `json:"engine"`
	Databases     []string    `json:"databases"`
	Interval      string      `json:"interval"`
	StartAt       *string     `json:"start_at,omitempty"`
	URISet        bool        `json:"uri_set"`
	State         string      `json:"state"` // idle|queued|running|cancelling
	LastRun       *RunSummary `json:"last_run"`
	LastSuccessAt *string     `json:"last_success_at"`
	NextDue       *string     `json:"next_due"`
	TotalBytes    int64       `json:"total_bytes"`
	ArchiveCount  int         `json:"archive_count"`
	DatabaseCount int         `json:"database_count"`
	OverdueSince  *string     `json:"overdue_since,omitempty"`
	OverdueAfter  int64       `json:"overdue_after,omitempty"`
}

type RunSummary struct {
	ID         string   `json:"id"`
	Project    string   `json:"project"`
	Trigger    string   `json:"trigger"` // schedule|manual
	Status     string   `json:"status"`  // running|ok|partial|failed|interrupted
	StartedAt  string   `json:"started_at"`
	FinishedAt *string  `json:"finished_at"`
	Error      *string  `json:"error"`
	Warnings   []string `json:"warnings"`
	DbOK       int      `json:"db_ok"`
	DbFailed   int      `json:"db_failed"`
	Bytes      int64    `json:"bytes"`
}

type RunningOp struct {
	Project string `json:"project"`
	RunID   string `json:"run_id"`
}

type StorageStatus struct {
	Kind     string `json:"kind"`
	Location string `json:"location"`
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
}

type Alert struct {
	ID      int    `json:"id"`
	TS      string `json:"ts"`
	Level   string `json:"level"`
	Project string `json:"project"`
	Text    string `json:"text"`
}

// OverviewConfig holds the overview handler's dependencies.
type OverviewConfig struct {
	Tenants         tenant.Store
	Runs            scheduler.RunStore
	StorageLocation string // e.g. "s3://bucket/prefix" — for the Storage block
}

// Overview returns the aggregate home-page response.
func (c *OverviewConfig) Overview(w http.ResponseWriter, r *http.Request) {
	resp := OverviewResponse{
		Projects: []ProjectSummary{},
		Running:  []RunningOp{},
		Storage:  StorageStatus{Kind: "s3", Location: c.StorageLocation, OK: true},
		Alerts:   []Alert{},
	}
	tenants, err := c.Tenants.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list tenants failed"})
		return
	}
	for _, t := range tenants {
		summary := tenantToProjectSummary(r.Context(), t, c.Runs)
		resp.Projects = append(resp.Projects, summary)
		// A running latest-run contributes to the Running list.
		if summary.LastRun != nil && summary.LastRun.Status == "running" {
			resp.Running = append(resp.Running, RunningOp{Project: summary.Name, RunID: summary.LastRun.ID})
		}
	}
	// Alphabetical for stable rendering.
	sort.Slice(resp.Projects, func(i, j int) bool { return resp.Projects[i].Name < resp.Projects[j].Name })
	writeJSON(w, http.StatusOK, resp)
}

// tenantToProjectSummary synthesises a ProjectSummary from a tenant.Record
// + its run history. The tenant.Resource list is folded into Engine +
// Databases — engine picks the first DB resource's driver, databases
// are the connection.database values across all DB-flavoured resources.
func tenantToProjectSummary(ctx context.Context, t tenant.Record, runs scheduler.RunStore) ProjectSummary {
	summary := ProjectSummary{
		Name:      t.StackName,
		Interval:  "1h", // placeholder — per-project schedule config comes in slice 1e.3
		State:     "idle",
		Databases: []string{},
	}
	databases := map[string]struct{}{}
	for _, res := range t.Resources {
		driver := res.Connection["driver"]
		if driver == "" {
			continue
		}
		if summary.Engine == "" {
			summary.Engine = driver
		}
		if db := res.Connection["database"]; db != "" {
			databases[db] = struct{}{}
		}
		summary.URISet = summary.URISet || res.Connection["uri"] != ""
	}
	for db := range databases {
		summary.Databases = append(summary.Databases, db)
	}
	sort.Strings(summary.Databases)
	summary.DatabaseCount = len(summary.Databases)

	// Latest run → LastRun + LastSuccessAt + state.
	rows, _ := runs.ListBackupsByTenant(ctx, t.StackName)
	// Oldest first for success-at scan, newest first for last-run pick.
	sort.Slice(rows, func(i, j int) bool { return rows[i].StartedAt.Before(rows[j].StartedAt) })
	var lastSuccess time.Time
	for _, r := range rows {
		summary.TotalBytes += r.SizeBytes
		if r.S3Path != "" {
			summary.ArchiveCount++
		}
		if r.Status == backup.StatusSucceeded && r.FinishedAt.After(lastSuccess) {
			lastSuccess = r.FinishedAt
		}
	}
	if !lastSuccess.IsZero() {
		s := lastSuccess.UTC().Format(time.RFC3339)
		summary.LastSuccessAt = &s
	}
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		rs := backupRunToSummary(last)
		summary.LastRun = &rs
		if last.Status == backup.StatusRunning {
			summary.State = "running"
		}
	}
	return summary
}

// backupRunToSummary maps a BackupRun (our store shape) to the SPA's
// RunSummary (service/internal/ui/src/api/types.ts:32). Status +
// trigger are translated to the SPA's enums.
func backupRunToSummary(r backup.BackupRun) RunSummary {
	rs := RunSummary{
		ID:        r.ID,
		Project:   r.TenantStack,
		Trigger:   mapTrigger(r.TriggerSource),
		Status:    mapStatus(r.Status),
		StartedAt: r.StartedAt.UTC().Format(time.RFC3339),
		Warnings:  []string{},
		Bytes:     r.SizeBytes,
	}
	if !r.FinishedAt.IsZero() {
		s := r.FinishedAt.UTC().Format(time.RFC3339)
		rs.FinishedAt = &s
	}
	if r.Error != "" {
		e := r.Error
		rs.Error = &e
	}
	// One run == one database today; SPA tracks db_ok/db_failed for
	// multi-DB runs (future). Set accordingly.
	if r.Status == backup.StatusSucceeded {
		rs.DbOK = 1
	}
	if r.Status == backup.StatusFailed {
		rs.DbFailed = 1
	}
	return rs
}

func mapTrigger(src string) string {
	switch src {
	case "schedule":
		return "schedule"
	case "operator", "manual":
		return "manual"
	default:
		return "manual"
	}
}

func mapStatus(s backup.Status) string {
	switch s {
	case backup.StatusSucceeded:
		return "ok"
	case backup.StatusRunning:
		return "running"
	case backup.StatusFailed:
		return "failed"
	case backup.StatusCancelled:
		return "interrupted"
	case backup.StatusPending:
		return "running"
	default:
		return string(s)
	}
}
