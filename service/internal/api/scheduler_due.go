// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"strings"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// DefaultIntervalMinutes is how often a tenant is backed up when
// nothing is configured.
//
// Aliases project.DefaultIntervalMinutes so the scheduler and the API's
// display default cannot drift. They used to be separate: the API
// answered a hardcoded "1h" for any project without a stored config
// while this was 240, so the SPA promised hourly backups and the engine
// did four-hourly. An operator reading "every hour" above "last success
// 4 h ago" was being told two different things, neither of them chosen
// by anyone.
const DefaultIntervalMinutes = project.DefaultIntervalMinutes

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

// resolveIntervalMinutes decides how often this tenant is backed up.
//
// Order, most operator-explicit first:
//
//  1. the project config's `interval` — the value the SPA displays and
//     an operator edits. THIS is the one that was being ignored.
//  2. the tenant record's IntervalMinutes, set by the plugin.
//  3. the shared default.
//
// An unparseable stored interval falls back rather than failing the
// whole tick: one malformed project config must not stop every other
// tenant's backup. The project endpoint is where a typo should be
// rejected, at write time, with the operator looking at it.
func resolveIntervalMinutes(ctx context.Context, projects project.Store, t tenant.Record) int {
	if projects != nil {
		if cfg, ok, err := projects.Get(ctx, t.StackName); err == nil && ok && strings.TrimSpace(cfg.Interval) != "" {
			if d, parsed := project.IntervalOrDefault(cfg.Interval); parsed {
				if m := int(d / time.Minute); m > 0 {
					return m
				}
			}
		}
	}
	if t.IntervalMinutes > 0 {
		return t.IntervalMinutes
	}
	return DefaultIntervalMinutes
}
