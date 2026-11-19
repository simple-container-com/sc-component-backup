// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package project

import (
	"fmt"
	"strings"
	"time"
)

// DefaultInterval is how often a project is backed up when it declares
// nothing.
//
// ONE constant, used by both the scheduler's due-check and the API's
// display default. They used to be separate values that disagreed: the
// API answered a hardcoded "1h" for any project without a stored
// config, while the scheduler used its own 4h default — so the SPA
// promised hourly backups for every tenant and the engine did something
// else entirely. An operator reading "every hour" next to "last success
// 4 h ago" has been told two different things, and neither was
// configured by anyone.
//
// 4h matches the cadence the fleet's timer is set to.
const DefaultInterval = "4h"

// DefaultIntervalMinutes is DefaultInterval in minutes, for callers
// that work in minutes.
const DefaultIntervalMinutes = 240

// ParseInterval turns an operator's interval string into a duration.
//
// Accepts Go duration syntax ("30m", "4h", "1h30m") plus the bare-hour
// shorthand operators tend to write ("4" → 4h), because the field is
// labelled "interval" in a UI full of hour values and a bare number is
// the obvious thing to type.
//
// A value that cannot be parsed is an ERROR rather than a silent
// fallback. Falling back would mean a typo in a backup schedule reads
// as "working, on some other cadence" — and the cadence a backup runs
// at is exactly the thing nobody re-checks once it looks configured.
func ParseInterval(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		d, _ := time.ParseDuration(DefaultInterval)
		return d, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("interval %q must be positive", s)
		}
		return d, nil
	}
	// Bare number → hours.
	var hours float64
	if _, err := fmt.Sscanf(s, "%g", &hours); err == nil && hours > 0 {
		return time.Duration(hours * float64(time.Hour)), nil
	}
	return 0, fmt.Errorf("interval %q is not a duration (want e.g. 30m, 4h, 1h30m)", s)
}

// IntervalOrDefault is ParseInterval with the default substituted on
// error, for read paths that must render something rather than fail.
//
// Returns ok=false when the stored value was unparseable, so a caller
// that CAN surface the problem does instead of hiding it.
func IntervalOrDefault(s string) (time.Duration, bool) {
	d, err := ParseInterval(s)
	if err != nil {
		fallback, _ := time.ParseDuration(DefaultInterval)
		return fallback, false
	}
	return d, true
}
