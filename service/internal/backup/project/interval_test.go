// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package project

import (
	"testing"
	"time"
)

// TestParseInterval covers the shapes an operator actually types.
func TestParseInterval(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30m":    30 * time.Minute,
		"4h":     4 * time.Hour,
		"1h30m":  90 * time.Minute,
		"1":      time.Hour,     // bare number → hours
		"4":      4 * time.Hour, // the obvious thing to type
		"":       4 * time.Hour, // unset → DefaultInterval
		"  4h  ": 4 * time.Hour,
	} {
		got, err := ParseInterval(in)
		if err != nil {
			t.Errorf("ParseInterval(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseInterval(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestParseInterval_RejectsGarbage — a typo must be an ERROR, not a
// silent fallback.
//
// Falling back would mean a mistyped backup schedule reads as "working,
// on some other cadence", and the cadence a backup runs at is exactly
// the thing nobody re-checks once it looks configured. That is how the
// SPA came to display "every hour" for services that were never backed
// up at all.
func TestParseInterval_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"hourly", "every hour", "-1h", "0", "0h", "abc"} {
		if d, err := ParseInterval(in); err == nil {
			t.Errorf("ParseInterval(%q) = %v, want an error", in, d)
		}
	}
}

// TestDefaultsAgree is the guard for the bug this file exists to fix:
// the display default and the engine default were different values.
func TestDefaultsAgree(t *testing.T) {
	d, err := ParseInterval(DefaultInterval)
	if err != nil {
		t.Fatalf("DefaultInterval %q does not parse: %v", DefaultInterval, err)
	}
	if int(d/time.Minute) != DefaultIntervalMinutes {
		t.Errorf("DefaultInterval=%q is %v but DefaultIntervalMinutes=%d — the SPA and the scheduler would disagree again",
			DefaultInterval, d, DefaultIntervalMinutes)
	}
}
