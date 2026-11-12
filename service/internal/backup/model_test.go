// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"testing"
	"time"
)

func TestComputeRetentionExpiresAt(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		retentionDays int
		wantZero      bool
		wantHours     int
	}{
		{0, true, 0},
		{-1, true, 0},
		{1, false, 24},
		{7, false, 24 * 7},
		{30, false, 24 * 30},
	}
	for _, c := range cases {
		got := ComputeRetentionExpiresAt(base, c.retentionDays)
		if c.wantZero {
			if !got.IsZero() {
				t.Errorf("retentionDays=%d: want zero, got %v", c.retentionDays, got)
			}
			continue
		}
		delta := int(got.Sub(base) / time.Hour)
		if delta != c.wantHours {
			t.Errorf("retentionDays=%d: want +%dh, got +%dh", c.retentionDays, c.wantHours, delta)
		}
	}
}
