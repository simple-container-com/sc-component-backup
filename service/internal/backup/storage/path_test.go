// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"strings"
	"testing"
	"time"
)

func TestRenderPath(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 30, 15, 0, time.UTC)
	cases := []struct {
		tmpl, tenant, resource, uuid, want string
	}{
		{
			tmpl:     "{tenant}/{yyyy}/{mm}/{dd}/{resource}-{ts-rfc3339}.archive.gz",
			tenant:   "smoke-client",
			resource: "mongodb",
			uuid:     "x",
			want:     "smoke-client/2026/11/11/mongodb-2026-11-11T09-30-15Z.archive.gz",
		},
		{
			tmpl:     "{tenant}/{uuid}",
			tenant:   "bad/slash/in/name",
			resource: "",
			uuid:     "run-abc",
			want:     "bad-slash-in-name/run-abc",
		},
		{
			tmpl:     "{yyyy}-{mm}-{dd}T{hh}-{mi}-{ss}Z",
			tenant:   "",
			resource: "",
			uuid:     "",
			want:     "2026-11-11T09-30-15Z",
		},
		{
			// Unknown placeholder left intact
			tmpl:   "{tenant}/{unknown}/x",
			tenant: "a", want: "a/{unknown}/x",
		},
	}
	for _, c := range cases {
		t.Run(c.tmpl, func(t *testing.T) {
			got := RenderPath(c.tmpl, c.tenant, c.resource, c.uuid, now)
			if got != c.want {
				t.Errorf("RenderPath:\n got:  %s\n want: %s", got, c.want)
			}
		})
	}
}

func TestSafeSegment(t *testing.T) {
	if got := safeSegment("a/b\\c"); got != "a-b-c" {
		t.Errorf("slash replace: got %q", got)
	}
	ctrls := "a\x00b\x01c\x7fd"
	if got := safeSegment(ctrls); got != "abcd" {
		t.Errorf("control-char drop: got %q", got)
	}
	if got := safeSegment("ok-under_score.dot"); got != "ok-under_score.dot" {
		t.Errorf("safe chars dropped: got %q", got)
	}
	// Ensure no accidental lowercasing
	if got := safeSegment("ABCxyz"); !strings.EqualFold(got, "abcxyz") || got != "ABCxyz" {
		t.Errorf("case: got %q", got)
	}
}
