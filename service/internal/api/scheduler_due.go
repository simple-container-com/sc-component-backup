// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// DefaultIntervalMinutes is how often a tenant is backed up when it
// declares nothing.
//
// Four hours, matching the cadence the parent stack's schedule is set
// to. Deliberately not "hourly": the fleet's largest tenant dumps 1.8 GB
// per run, so an hourly default would mean ~150 GB/day of archives for
// six tenants before anyone had asked for it.
const DefaultIntervalMinutes = 240

// runLister is the slice of RunStore the due-check needs. Narrow on
// purpose so the check is testable without standing up the whole store.
type runLister interface {
	ListBackupsByTenant(ctx context.Context, tenantStack string) ([]backup.BackupRun, error)
}

// lastSuccessAt returns when this tenant last completed a backup, and
// whether it ever has.
//
// Reads the RUN HISTORY rather than a `lastRunAt` field on the tenant
// record. Three reasons: the history is already written and already the
// thing the UI shows, a separate marker is a second source of truth that
// can disagree with it, and adding a field would need a migration across
// records that already exist in operators' buckets.
//
// Only SUCCEEDED runs count. A tenant whose backups are failing is due
// every tick, which is the behaviour you want — a failing backup must
// keep being retried, not be marked done by having been attempted.
func lastSuccessAt(ctx context.Context, runs runLister, stack string) (time.Time, bool) {
	if runs == nil {
		return time.Time{}, false
	}
	list, err := runs.ListBackupsByTenant(ctx, stack)
	if err != nil {
		// Unknown history reads as "never backed up", so the tenant is
		// due. Erring toward an extra backup is the safe direction; the
		// opposite would silently skip a tenant whenever the store
		// hiccuped.
		return time.Time{}, false
	}
	var newest time.Time
	for _, r := range list {
		if r.Status != backup.StatusSucceeded {
			continue
		}
		at := r.FinishedAt
		if at.IsZero() {
			at = r.StartedAt
		}
		if at.After(newest) {
			newest = at
		}
	}
	return newest, !newest.IsZero()
}

// tenantIsDue reports whether a tenant should be backed up now.
//
// `intervalMinutes <= 0` falls back to DefaultIntervalMinutes rather
// than meaning "never" — a zero would otherwise be the value a record
// written before this field existed carries, and reading that as
// "disabled" would silently stop backing up every already-registered
// tenant.
func tenantIsDue(ctx context.Context, runs runLister, stack string, intervalMinutes int, now time.Time) (bool, string) {
	if intervalMinutes <= 0 {
		intervalMinutes = DefaultIntervalMinutes
	}
	last, ever := lastSuccessAt(ctx, runs, stack)
	if !ever {
		return true, "never backed up"
	}
	elapsed := now.Sub(last)
	want := time.Duration(intervalMinutes) * time.Minute
	if elapsed >= want {
		return true, "last success " + elapsed.Round(time.Minute).String() + " ago"
	}
	return false, "next due in " + (want - elapsed).Round(time.Minute).String()
}
