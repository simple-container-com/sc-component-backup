// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
)

// decodeDefaults is the plugin's advertised defaults as a map, the way
// SC core sees them before the operator's raw is merged on top.
func decodeDefaults(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(componentDefaultsJSON, &m); err != nil {
		t.Fatalf("unmarshal componentDefaultsJSON: %v", err)
	}

	return m
}

// scheduleRequestOf digs out the one lambda schedule's request payload.
func scheduleRequestOf(t *testing.T, defaults map[string]any) string {
	t.Helper()
	extras, ok := defaults["cloudExtras"].(map[string]any)
	if !ok {
		t.Fatalf("defaults have no cloudExtras map: %#v", defaults["cloudExtras"])
	}
	schedules, ok := extras["lambdaSchedules"].([]any)
	if !ok || len(schedules) != 1 {
		t.Fatalf("want exactly one lambdaSchedule, got %#v", extras["lambdaSchedules"])
	}
	sched, ok := schedules[0].(map[string]any)
	if !ok {
		t.Fatalf("schedule is not a map: %#v", schedules[0])
	}
	req, ok := sched["request"].(string)
	if !ok {
		t.Fatalf("schedule request is not a string: %#v", sched["request"])
	}

	return req
}

// THE regression test for the sentinel position in a schedule payload.
//
// The scheduler tick cannot be a goroutine (Lambda freezes background
// work when the handler returns), so it is an HTTP endpoint driven by
// an EventBridge rule, and that rule's input has to carry a bearer
// token. The obvious way to write it — the `$$sc:` sentinel next to the
// token it guards, `"Bearer $$sc:${resource:${name}.scheduler-token}"` —
// does not substitute: `substituteString` returns the string unchanged
// unless it BEGINS with the sentinel, and says nothing when it doesn't.
// That shipped to sc-component-observatory on 2026-11-19; the rule
// fired on time and the tick answered 401 twice, holding the literal
// placeholder in its input. It was then misread as a hard limitation of
// SC ("`${secret:}` resolves in a schedule payload but the
// tpl-extension form does not") and a ~25-line comment saying so went
// into all three components, which is why every operator still writes
// this block by hand.
//
// So this test runs the real pipeline — the plugin's own defaults
// through InterpolateDefaults and then through a Collector carrying the
// tpl extension PreProvision lands — and asserts the token actually
// arrives. Invert it by moving `$$sc:` from the front of the payload to
// in front of the token and it fails with the literal placeholder,
// which is precisely the live 401.
func TestComponentDefaults_SchedulerTokenResolvesInScheduleRequest(t *testing.T) {
	defaults := decodeDefaults(t)

	if raw := scheduleRequestOf(t, defaults); !strings.HasPrefix(raw, pApi.DefaultsSentinel) {
		t.Fatalf("schedule request must START with %q or no token in it ever substitutes; got %q",
			pApi.DefaultsSentinel, raw)
	}

	interpolated := pApi.InterpolateDefaults(defaults, pApi.DefaultsContext{
		Name:  "my-backup",
		Env:   "staging",
		Stack: "infra",
	})

	collector := pApi.NewComputeContextCollector(context.Background(), logger.New(), "infra", "staging")
	collector.AddResourceTplExtension("my-backup", map[string]string{
		"scheduler-token": "tok-from-preprovision",
		"storage-bucket":  "backup-archives--staging",
	})
	if err := collector.ResolvePlaceholders(&interpolated); err != nil {
		t.Fatalf("ResolvePlaceholders: %v", err)
	}

	got := scheduleRequestOf(t, interpolated)
	if strings.Contains(got, "${") || strings.Contains(got, pApi.DefaultsSentinel) {
		t.Fatalf("schedule request still holds an unresolved placeholder — this is the live 401: %s", got)
	}

	// Decode it: the rule's input is the Lambda event, so a payload
	// that resolved but no longer parses is just as dead.
	var event struct {
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal([]byte(got), &event); err != nil {
		t.Fatalf("resolved schedule request is not valid JSON: %v\n%s", err, got)
	}
	if event.Path != "/api/v1/scheduler/tick" {
		t.Fatalf("schedule path = %q, want /api/v1/scheduler/tick", event.Path)
	}
	if event.Headers["Authorization"] != "Bearer tok-from-preprovision" {
		t.Fatalf("Authorization = %q, want %q", event.Headers["Authorization"], "Bearer tok-from-preprovision")
	}
}

// The go-embedb bucket default must resolve through the SAME pass, and
// to the operator's effective bucket rather than the plugin's
// convention name. An unresolved value here is worse than a missing
// one: the service would take the literal `${resource:…}` string as a
// bucket name and every metadata write would fail against a bucket that
// cannot exist.
func TestComponentDefaults_GoEmbedbBucketResolves(t *testing.T) {
	interpolated := pApi.InterpolateDefaults(decodeDefaults(t), pApi.DefaultsContext{
		Name: "my-backup", Env: "staging", Stack: "infra",
	})
	collector := pApi.NewComputeContextCollector(context.Background(), logger.New(), "infra", "staging")
	collector.AddResourceTplExtension("my-backup", map[string]string{
		"scheduler-token": "t",
		"storage-bucket":  "backup-archives--staging",
	})
	if err := collector.ResolvePlaceholders(&interpolated); err != nil {
		t.Fatalf("ResolvePlaceholders: %v", err)
	}

	env, ok := interpolated["env"].(map[string]any)
	if !ok {
		t.Fatalf("defaults have no env map")
	}
	if got := env["GOEMBEDB_S3_BUCKET"]; got != "backup-archives--staging" {
		t.Fatalf("GOEMBEDB_S3_BUCKET = %#v, want the operator's effective bucket", got)
	}
	if got := env["USE_GOEMBEDB"]; got != "1" {
		t.Fatalf("USE_GOEMBEDB = %#v, want \"1\" — the colocated engine refuses RMW upserts from every warm Lambda but the lease owner", got)
	}
}

// The image default is what makes the plugin/image lockstep structural
// instead of a convention two pins have to honour. Both halves matter:
// a stamped build must name its own tag, and an unstamped one must name
// nothing at all (`:dev` was never pushed, and SC's own "image
// required" is a far better error than a pull failure).
func TestDefaultImage_TracksTheStampedVersion(t *testing.T) {
	for _, tc := range []struct {
		version, want string
	}{
		{"v2026.11.25-56a1457", imageRepo + ":v2026.11.25-56a1457"},
		{"dev", ""},
		{"", ""},
	} {
		t.Run(tc.version, func(t *testing.T) {
			orig := version
			version = tc.version
			defer func() { version = orig }()

			if got := defaultImage(); got != tc.want {
				t.Fatalf("defaultImage() with version %q = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

// A schedule `name` over 14 characters is rejected at APPLY, after
// preview has passed — the worst place to learn it. Assert it here
// instead, where a one-character rename is free.
func TestComponentDefaults_ScheduleNameFitsTheLimit(t *testing.T) {
	extras := decodeDefaults(t)["cloudExtras"].(map[string]any)
	for _, s := range extras["lambdaSchedules"].([]any) {
		name, _ := s.(map[string]any)["name"].(string)
		if name == "" {
			t.Fatalf("schedule has no name: %#v", s)
		}
		if len(name) > 14 {
			t.Fatalf("schedule name %q is %d chars; >14 is rejected at apply, after preview passes", name, len(name))
		}
	}
}

// The `bk-tick` cadence is a DATA-DURABILITY constraint, not a
// scheduling preference, and nothing else in the build would catch a
// regression on it — a coarser rule deploys green, fires on time, and
// loses acked writes silently.
//
// The tick is the only thing that calls `Handler.TickNow`, which is the
// only thing that drives go-embedb's reader catch-up on a Lambda (a
// frozen container does not tick). So the tick period IS the window in
// which a peer replays another replica's log. A write that is not
// replayed before its writer's slot is reclaimed is epoch-fenced or
// GC'd and becomes permanently unreadable — durable in S3, acked to the
// client, and gone.
//
// Live-caught 2026-11-26 at the old `rate(30 minutes)`: two
// `tenants/register` writes lost (leaving a rotated-away Atlas password
// in forge-baas's tenant row, so every mongodump failed
// `AuthenticationFailed`) and four `runs/{id}/complete` callbacks lost
// (leaving finished 1.9 GB backups on `running`). Every write that
// landed AT a tick survived; every write between ticks did not.
//
// The bound is deliberately a RANGE, not equality: re-timing 2m -> 1m
// is fine and should not fail a test, while 2m -> 15m must. 5 minutes
// is the ceiling because that is the coarsest cadence `TickNow`'s own
// measured table records as healthy.
func TestComponentDefaults_TickCadenceIsFineEnoughToReplicate(t *testing.T) {
	const maxCadence = 5 * time.Minute

	extras := decodeDefaults(t)["cloudExtras"].(map[string]any)

	schedules, ok := extras["lambdaSchedules"].([]any)
	if !ok || len(schedules) != 1 {
		t.Fatalf("want exactly one lambdaSchedule, got %#v", extras["lambdaSchedules"])
	}

	expr, _ := schedules[0].(map[string]any)["expression"].(string)

	cadence, err := parseRateExpression(expr)
	if err != nil {
		t.Fatalf("bk-tick expression %q: %v — the cadence must stay machine-checkable, "+
			"because it bounds how long an acked write can sit unreplayed", expr, err)
	}

	if cadence > maxCadence {
		t.Fatalf("bk-tick fires every %s; anything over %s loses acked writes.\n"+
			"The tick is the ONLY driver of go-embedb reader catch-up on Lambda, so this\n"+
			"period is the window in which a peer replays another replica's log. A write\n"+
			"not replayed before its writer's slot is reclaimed is epoch-fenced or GC'd —\n"+
			"durable in S3, acked, unreadable. Live-caught 2026-11-26 at rate(30 minutes).",
			cadence, maxCadence)
	}
}

// parseRateExpression reads the `rate(N unit)` form EventBridge accepts.
// Only what this component actually emits; a cron() expression is a
// deliberate error rather than a silent pass, because a cron cadence
// cannot be bounded by reading one number.
func parseRateExpression(expr string) (time.Duration, error) {
	inner, ok := strings.CutPrefix(strings.TrimSpace(expr), "rate(")
	if !ok {
		return 0, fmt.Errorf("not a rate() expression")
	}

	inner, ok = strings.CutSuffix(inner, ")")
	if !ok {
		return 0, fmt.Errorf("unterminated rate() expression")
	}

	fields := strings.Fields(inner)
	if len(fields) != 2 {
		return 0, fmt.Errorf("want `rate(N unit)`, got %q", inner)
	}

	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("rate value %q is not an integer", fields[0])
	}

	unit, ok := map[string]time.Duration{
		"minute": time.Minute, "minutes": time.Minute,
		"hour": time.Hour, "hours": time.Hour,
		"day": 24 * time.Hour, "days": 24 * time.Hour,
	}[fields[1]]
	if !ok {
		return 0, fmt.Errorf("unknown rate unit %q", fields[1])
	}

	return time.Duration(n) * unit, nil
}
