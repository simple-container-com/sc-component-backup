// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// ReconcileStuckRuns scans every backup-run row and marks any that are
// still in `running` state past a reasonable timeout as `interrupted`.
// Intended to run exactly once at service startup — a Lambda that OOM'd
// mid-run (or was replaced for any other reason) leaves its row stuck
// in `running` forever because the UpdateBackup call never fires; the
// SPA then shows a permanent spinner that doesn't clear.
//
// `now` is injected so tests can pin the clock. `cutoff` is the maximum
// age a `running` row can have before it's reclassified — for the
// scheduler's default BackupTimeout (1h) a 2x margin is generous
// headroom for a slow-but-honest run. Callers typically pass
// Scheduler.cfg.BackupTimeout * 2; a zero cutoff defaults to 2h.
//
// Idempotent: a row already in a terminal state is skipped; a row with
// a zero StartedAt is skipped (shouldn't happen but defensive).
// Failures to update individual rows are LOGGED, never returned — a
// Mongo hiccup on one row shouldn't block the sweep for the rest.
func ReconcileStuckRuns(ctx context.Context, runs RunStore, log *slog.Logger, now time.Time, cutoff time.Duration) (reconciled int) {
	if runs == nil {
		return 0
	}
	if log == nil {
		log = slog.Default()
	}
	if cutoff <= 0 {
		cutoff = 2 * time.Hour
	}

	rows, err := runs.ListAllBackups(ctx, 0)
	if err != nil {
		log.Warn("watchdog list runs failed", "err", err)
		return 0
	}

	for _, row := range rows {
		if row.Status != backup.StatusRunning && row.Status != backup.StatusPending {
			continue
		}
		if row.StartedAt.IsZero() {
			continue
		}
		if now.Sub(row.StartedAt) < cutoff {
			continue
		}
		row.Status = backup.StatusCancelled
		row.FinishedAt = now
		row.Error = "watchdog: run exceeded cutoff without a terminal status; probably orphaned by a worker that didn't check in"
		if err := runs.UpdateBackup(ctx, row); err != nil {
			log.Warn(
				"watchdog mark interrupted failed",
				"run_id", row.ID,
				"tenant", row.TenantStack,
				"started_at", row.StartedAt.Format(time.RFC3339),
				"err", err,
			)
			continue
		}
		log.Info(
			"watchdog marked stuck run interrupted",
			"run_id", row.ID,
			"tenant", row.TenantStack,
			"started_at", row.StartedAt.Format(time.RFC3339),
			"age", now.Sub(row.StartedAt).String(),
		)
		reconciled++
	}
	return reconciled
}
