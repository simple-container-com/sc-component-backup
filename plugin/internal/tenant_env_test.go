// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import "testing"

// TestEnvMayRegister_EmptyAllowsAll — adding the key must be opt-in.
// An operator who never sets it keeps the behaviour they have.
func TestEnvMayRegister_EmptyAllowsAll(t *testing.T) {
	for _, env := range []string{"staging", "3ec321", ""} {
		if !EnvMayRegister(nil, env) {
			t.Errorf("nil allowlist refused %q", env)
		}
		if !EnvMayRegister([]string{}, env) {
			t.Errorf("empty allowlist refused %q", env)
		}
	}
}

// TestEnvMayRegister_ExcludesIsolatedEnvs is the case this exists for.
//
// An isolated stack's environment is a workflow id, and it inherits
// `uses:` through a YAML merge key that cannot delete entries — so the
// component is the only place it can be refused.
//
// Here the cost of admitting one is real money: the tenant row carries
// a live Atlas URI, so the scheduler dumps a throwaway database every
// interval, one Fargate task and one S3 archive at a time.
func TestEnvMayRegister_ExcludesIsolatedEnvs(t *testing.T) {
	allowed := []string{"staging"}

	if !EnvMayRegister(allowed, "staging") {
		t.Error("staging was refused")
	}
	for _, wf := range []string{"3ec321", "99171c", "1c25e6ad"} {
		if EnvMayRegister(allowed, wf) {
			t.Errorf("isolated env %q was admitted", wf)
		}
	}
}

// TestEnvMayRegister_NoPrefixMatching — `staging` must not admit
// `staging-ru`, which is a real and separate environment in this fleet
// (forge-atrium deploys to both). A prefix match would silently merge
// two environments' deploy history into one row.
func TestEnvMayRegister_NoPrefixMatching(t *testing.T) {
	if EnvMayRegister([]string{"staging"}, "staging-ru") {
		t.Error("staging-ru was admitted by a staging allowlist")
	}
	// Both listed → both admitted.
	both := []string{"staging", "staging-ru"}
	for _, e := range both {
		if !EnvMayRegister(both, e) {
			t.Errorf("%q refused when explicitly listed", e)
		}
	}
}

// TestEnvMayRegister_EmptyEnvIsRefused — an empty environment against a
// non-empty allowlist is a resolution failure, not a match. Admitting
// it would let a dispatch shape that loses the env register anyway, as
// a tenant with no environment.
func TestEnvMayRegister_EmptyEnvIsRefused(t *testing.T) {
	if EnvMayRegister([]string{"staging"}, "") {
		t.Error("empty env was admitted")
	}
	if EnvMayRegister([]string{"staging"}, "   ") {
		t.Error("whitespace env was admitted")
	}
}

// TestTenantEnvironmentsFromConfig covers the shapes that reach a
// plugin. []any is the one it ACTUALLY arrives in, after the JSON
// round-trip over the bus; the others are defensive.
func TestTenantEnvironmentsFromConfig(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want int
	}{
		{"absent", map[string]any{}, 0},
		{"nil map", nil, 0},
		{"[]any from the bus", map[string]any{TenantEnvironmentsKey: []any{"staging", "staging-ru"}}, 2},
		{"[]string", map[string]any{TenantEnvironmentsKey: []string{"staging"}}, 1},
		{"single string", map[string]any{TenantEnvironmentsKey: "staging"}, 1},
		{"blank string", map[string]any{TenantEnvironmentsKey: "  "}, 0},
		// A wrong type must read as "unset" (allow all) rather than
		// "empty list" — failing closed here would stop every
		// registration in the fleet over a config typo.
		{"wrong type", map[string]any{TenantEnvironmentsKey: 42}, 0},
		{"mixed list drops non-strings", map[string]any{TenantEnvironmentsKey: []any{"staging", 7, ""}}, 1},
	}
	for _, tc := range cases {
		if got := TenantEnvironmentsFromConfig(tc.raw); len(got) != tc.want {
			t.Errorf("%s: got %v (len %d), want len %d", tc.name, got, len(got), tc.want)
		}
	}
}
