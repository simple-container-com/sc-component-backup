// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/retention"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// ArchivesConfig carries the stores the archives handler needs. The
// handler returns the SPA's `ArchivesResponse` shape (see
// internal/ui/src/api/types.ts).
type ArchivesConfig struct {
	Tenants  tenant.Store
	Runs     scheduler.RunStore
	Projects project.Store
}

// archiveJSON mirrors internal/ui/src/api/types.ts Archive.
type archiveJSON struct {
	Key    string   `json:"key"`
	TS     string   `json:"ts"`
	Size   int64    `json:"size"`
	KeptBy []string `json:"kept_by"`
}

type databaseArchivesJSON struct {
	Database   string        `json:"database"`
	TotalBytes int64         `json:"total_bytes"`
	Archives   []archiveJSON `json:"archives"`
}

type restoreHintJSON struct {
	Command  string            `json:"command"`
	BySuffix map[string]string `json:"by_suffix,omitempty"`
}

type archivesResponseJSON struct {
	Databases []databaseArchivesJSON `json:"databases"`
	Globals   []archiveJSON          `json:"globals"`
	Restore   restoreHintJSON        `json:"restore"`
}

// Archives serves `GET /api/v1/projects/{name}/archives` — the SPA's
// ArchivesTab inventory. Lists successful backup runs for the tenant,
// groups them by target database, and attaches retention keep-reasons
// (what's protecting each archive from the next sweep) so the operator
// can see at a glance what will fall off next.
//
// Sources:
//   - Run history: scheduler.RunStore.ListBackupsByTenant.
//   - Retention policy: project.Config if the operator ran the slice-1e.3
//     project-create flow; else the fleet default (retention.Default,
//     "keep last 7").
//   - Database name: derived from the first tenant.Resource's
//     Connection["database"]. A multi-DB tenant (not a v1 shape) falls
//     back to the engine's default-all literal "<all databases>" so the
//     SPA still renders.
//
// Restore hint: one fleet-wide command per engine. The SPA shows the
// hint next to each archive so the operator has a copy-paste starting
// point. The `by_suffix` map is reserved for future per-suffix variants
// (e.g. `.sql` vs `.archive.gz` on the same engine).
func (c *ArchivesConfig) Archives(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		http.Error(w, `{"error":"project name required"}`, http.StatusBadRequest)
		return
	}

	rec, ok, err := c.Tenants.Get(r.Context(), name)
	if err != nil {
		http.Error(w, `{"error":"tenant lookup failed"}`, http.StatusInternalServerError)
		return
	}
	// Tenant may legitimately be absent — an operator can view a
	// freshly created project.Config before any consumer has
	// push-registered. Treat as "no archives yet" rather than 404
	// so the SPA renders an empty state.
	var database, engine string
	if ok {
		for _, res := range rec.Resources {
			if db := res.Connection["database"]; db != "" {
				database = db
				engine = res.Type
				break
			}
		}
	}

	runs, err := c.Runs.ListBackupsByTenant(r.Context(), name)
	if err != nil {
		http.Error(w, `{"error":"list runs failed"}`, http.StatusInternalServerError)
		return
	}

	// Retention from the project config, or default.
	retPolicy := retention.Default()
	if cfg, pok, perr := c.Projects.Get(r.Context(), name); perr == nil && pok {
		retPolicy = retention.Retention{
			Last:    cfg.Retention.Last,
			Daily:   cfg.Retention.Daily,
			Weekly:  cfg.Retention.Weekly,
			Monthly: cfg.Retention.Monthly,
		}
		if engine == "" && cfg.Engine != "" {
			engine = cfg.Engine
		}
	}

	// Build the key list for retention.KeepReasons. Only keep SUCCEEDED
	// runs — a failed/partial run has no archive to retain.
	type archiveRow struct {
		run backup.BackupRun
	}
	rowsByKey := map[string]archiveRow{}
	var keys []string
	var totalBytes int64
	for _, run := range runs {
		if run.Status != backup.StatusSucceeded {
			continue
		}
		if run.S3Path == "" {
			continue
		}
		rowsByKey[run.S3Path] = archiveRow{run: run}
		keys = append(keys, run.S3Path)
		totalBytes += run.SizeBytes
	}

	reasonsByKey := retention.KeepReasons(keys, retPolicy)

	// Newest archive first — matches the SPA's expected order + is
	// what operators care about.
	sort.Slice(keys, func(i, j int) bool {
		return rowsByKey[keys[i]].run.FinishedAt.After(rowsByKey[keys[j]].run.FinishedAt)
	})

	archives := make([]archiveJSON, 0, len(keys))
	for _, k := range keys {
		row := rowsByKey[k]
		archives = append(archives, archiveJSON{
			Key:    row.run.S3Path,
			TS:     row.run.FinishedAt.UTC().Format("2006-01-02T15:04:05Z"),
			Size:   row.run.SizeBytes,
			KeptBy: reasonsByKey[k],
		})
	}

	resp := archivesResponseJSON{Databases: []databaseArchivesJSON{}, Globals: []archiveJSON{}}
	if len(archives) > 0 {
		dbLabel := database
		if dbLabel == "" {
			dbLabel = "<all databases>"
		}
		resp.Databases = []databaseArchivesJSON{{
			Database:   dbLabel,
			TotalBytes: totalBytes,
			Archives:   archives,
		}}
	}
	resp.Restore = restoreHint(engine, database)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Encoder already wrote part of the body — nothing to recover.
		// Log at caller's layer via middleware.Recoverer if applicable.
		_ = err
	}
}

// restoreHint returns the one-liner the SPA renders alongside the
// archive list. One line per engine; the `{key}` token is replaced by
// the SPA client-side with the archive key the operator clicked. The
// command is a STARTING POINT — operators adapt it (target host,
// credentials, drop vs. merge) before running.
func restoreHint(engine, database string) restoreHintJSON {
	// Database name is operator-provided and may contain shell
	// metacharacters. The hint is copy-paste, not auto-executed, so
	// a quoted placeholder plus a visible warning is honest enough.
	db := database
	if db == "" {
		db = "<db>"
	}
	switch engine {
	case "mongodb-atlas", "mongodb":
		return restoreHintJSON{
			Command: fmt.Sprintf(
				"aws s3 cp s3://<bucket>/{key} - | gunzip | mongorestore --archive --db %q --drop --uri \"$MONGO_URI\"",
				db,
			),
		}
	case "postgres", "rds-postgres":
		return restoreHintJSON{
			Command: fmt.Sprintf(
				"aws s3 cp s3://<bucket>/{key} - | gunzip | pg_restore --clean --if-exists -d %q --format=custom",
				db,
			),
		}
	case "mysql", "rds-mysql":
		return restoreHintJSON{
			Command: fmt.Sprintf(
				"aws s3 cp s3://<bucket>/{key} - | gunzip | mysql %q",
				db,
			),
		}
	}
	// Unknown engine → let the operator read the key and decide.
	return restoreHintJSON{
		Command: "aws s3 cp s3://<bucket>/{key} - | gunzip > archive.local; # engine-specific restore then",
	}
}

// Sentinel is fine via errors.Is on the storage package; keep a
// local guard against accidental import removal by referencing the
// package once at package-init time via var-block below.
var _ = errors.Is
