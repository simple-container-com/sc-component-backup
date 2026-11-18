// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package alerts turns the backup service's recent-run history into the
// `[]Alert` list the SPA's Overview page renders, plus a dismissal
// facility so operators can clear noise after reading it.
//
// # Design
//
// Alerts are derived, not stored. On every /api/v1/overview request the
// handler walks recent runs across tenants, synthesizes one Alert per
// newsworthy run (failed / cancelled / long-running), and filters out
// the ones the operator has already dismissed. There's no alert-writer
// path — if the Run disappears, the Alert disappears with it, which is
// the right UX for a surfaced-from-truth signal.
//
// Dismissals ARE stored. Two shapes:
//   - Per-ID dismissal: the operator clicks the × on a specific alert.
//     Stored as the int64 ID.
//   - Dismiss-all: the operator clicks "dismiss all". Stored as a
//     before-timestamp; any alert whose run finished before this
//     cutoff counts as dismissed. New alerts after the cutoff still
//     appear.
//
// The Alert.ID is a 53-bit FNV-1a hash of the originating run ID. 53
// bits is the integer-safe range the SPA's `id: number` TypeScript
// type can faithfully round-trip (JavaScript numbers are IEEE 754
// doubles; collisions in that space are astronomically unlikely
// across a fleet's run volume).
package alerts

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
)

// Level mirrors internal/ui/src/api/types.ts AlertLevel. The SPA's
// type widens to `string` for forward-compat; we stick to the three
// canonical values.
type Level string

const (
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelInfo    Level = "info"
)

// Alert matches internal/ui/src/api/types.ts Alert. ID is int64 on
// the wire but constrained to 53 bits so the SPA's number type does
// not lose precision.
type Alert struct {
	ID      int64  `json:"id"`
	TS      string `json:"ts"`
	Level   Level  `json:"level"`
	Project string `json:"project"`
	Text    string `json:"text"`
}

// Dismissals is the store contract. Two impls: InMemoryDismissals
// (tests + local-dev MVP) and MongoDismissals (production, when
// BACKUP_PROJECTS_MODE=mongo). The handler reads State() once per
// Overview request + filters locally rather than paying a round-trip
// per alert.
type Dismissals interface {
	// Dismiss marks one alert id dismissed. Idempotent — a second
	// call with the same id is a no-op.
	Dismiss(ctx context.Context, id int64) error
	// DismissAll sets the before-timestamp cutoff. Any alert whose
	// run finished before this value is treated as dismissed.
	// Passing an earlier timestamp than the current cutoff is a
	// no-op (dismissals are a high-water mark; you cannot
	// un-dismiss by moving the cutoff backward).
	DismissAll(ctx context.Context, before time.Time) error
	// State returns the current dismissal snapshot. The returned
	// value is a copy — mutating it does not affect the store.
	State(ctx context.Context) (State, error)
}

// State is the dismissal snapshot the handler filters against.
type State struct {
	IDs    map[int64]struct{}
	Before time.Time
}

// IsDismissed reports whether an alert with (id, ts) should be hidden
// given the current dismissal snapshot.
func (s State) IsDismissed(id int64, ts time.Time) bool {
	if _, ok := s.IDs[id]; ok {
		return true
	}
	if !s.Before.IsZero() && !ts.After(s.Before) {
		return true
	}
	return false
}

// alertIDMask keeps the hash within JavaScript's safe-integer range.
// 2^53 - 1 = 9007199254740991; AND'ing against that mask preserves
// 53 bits of entropy.
const alertIDMask int64 = (1 << 53) - 1

// AlertIDFromRun derives a stable alert id from a run id. FNV-1a 64
// masked to 53 bits. Deterministic across restarts so an operator's
// dismissal survives a service rollout.
func AlertIDFromRun(runID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(runID))
	// Mask high bits so the SPA's JS number can hold the value
	// without loss. The sign bit stays zero — all alert ids are
	// positive.
	return int64(h.Sum64()) & alertIDMask
}

// Synthesizer walks recent runs and emits Alerts. The exported struct
// (vs. a bag of args) gives each knob a name an operator can read in
// the overview-handler wiring at main.go.
type Synthesizer struct {
	// Lookback bounds how far back the synthesizer walks for each
	// tenant. Zero → 7 days.
	Lookback time.Duration
	// MaxPerTenant caps how many alerts one tenant can contribute
	// to the feed — a project in continuous failure should not
	// drown the others. Zero → 10.
	MaxPerTenant int
	// Now is injected for deterministic tests. Zero value →
	// time.Now().
	Now func() time.Time
}

// RunLister is the minimum of scheduler.RunStore the synthesizer
// needs — ListBackupsByTenant per tenant. Taking the narrow
// interface here keeps the alerts package free of the scheduler
// dependency.
type RunLister interface {
	ListBackupsByTenant(ctx context.Context, tenantStack string) ([]backup.BackupRun, error)
}

// Emit synthesizes the Alert list for the operator's overview.
// Caller provides the tenant inventory + a RunLister + the current
// dismissal State; Emit returns the undismissed alerts, newest
// first, trimmed per-tenant.
func (s Synthesizer) Emit(ctx context.Context, tenants []string, runs RunLister, state State) ([]Alert, error) {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	lookback := s.Lookback
	if lookback <= 0 {
		lookback = 7 * 24 * time.Hour
	}
	maxPerTenant := s.MaxPerTenant
	if maxPerTenant <= 0 {
		maxPerTenant = 10
	}
	cutoff := now.Add(-lookback)

	var out []Alert
	for _, tenant := range tenants {
		rows, err := runs.ListBackupsByTenant(ctx, tenant)
		if err != nil {
			// Per-tenant failure should not take down the whole
			// feed — log by returning what we have would be a
			// surprise for an "alerts" feature. Return the error
			// for the caller to log + render the overview without
			// alerts.
			return nil, fmt.Errorf("list runs for %q: %w", tenant, err)
		}
		// Newest finished first, bounded by lookback.
		sort.Slice(rows, func(i, j int) bool {
			return rows[i].FinishedAt.After(rows[j].FinishedAt)
		})
		emitted := 0
		for _, r := range rows {
			if r.FinishedAt.IsZero() || r.FinishedAt.Before(cutoff) {
				continue
			}
			a, ok := alertForRun(r)
			if !ok {
				continue
			}
			if state.IsDismissed(a.ID, r.FinishedAt) {
				continue
			}
			out = append(out, a)
			emitted++
			if emitted >= maxPerTenant {
				break
			}
		}
	}
	// Final sort: newest alert first (by TS parsed back).
	sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out, nil
}

// alertForRun maps a BackupRun → one Alert, or returns false when
// the run is not newsworthy enough to surface. Rules:
//   - StatusFailed    → error-level, text = "backup failed: <err>"
//   - StatusCancelled → warning-level, text = "backup cancelled"
//   - StatusSucceeded → no alert
//   - StatusRunning   → no alert (the Overview's `running` block shows these)
func alertForRun(r backup.BackupRun) (Alert, bool) {
	ts := r.FinishedAt.UTC().Format("2006-01-02T15:04:05Z")
	id := AlertIDFromRun(r.ID)
	switch r.Status {
	case backup.StatusFailed:
		msg := strings.TrimSpace(r.Error)
		if msg == "" {
			msg = "unknown error"
		}
		return Alert{
			ID:      id,
			TS:      ts,
			Level:   LevelError,
			Project: r.TenantStack,
			Text:    truncate("backup failed: "+msg, 512),
		}, true
	case backup.StatusCancelled:
		return Alert{
			ID:      id,
			TS:      ts,
			Level:   LevelWarning,
			Project: r.TenantStack,
			Text:    "backup cancelled",
		}, true
	}
	return Alert{}, false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
