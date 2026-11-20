// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import "strings"

// TenantEnvironmentsKey is the operator config key holding the
// environments whose deploys may register as tenants.
const TenantEnvironmentsKey = "tenantEnvironments"

// EnvMayRegister reports whether a consumer deploying to `env` should
// become a tenant.
//
// WHY THIS EXISTS. An SC parent stack can be cloned into short-lived
// ISOLATED stacks — the Forge fleet spins one per agent-workflow run,
// and each service gets a stack like `forge-conductor--3ec321`. Those
// blocks inherit the base config with a YAML merge key
// (`<<: *staging-cfg`), and a merge key CANNOT delete an inherited
// entry. So an isolated deploy inherits `uses: [… my-observatory]` and
// there is no way to opt out from the YAML side.
//
// It registers, and the consequence here is worse than a cluttered
// dashboard: each isolated tenant carries a live Atlas connection URI,
// so the scheduler dumps a throwaway database every interval — a
// Fargate task and an S3 archive per run, for a stack that was
// destroyed hours ago. Six of them were registered on 2026-11-21
// alongside the six real services.
//
// The filter is an ALLOWLIST rather than a deny-pattern because the
// thing being excluded has no stable shape: a workflow id is a random
// hex string, so any pattern matching it would also match a legitimate
// environment someone names later. The set of environments an operator
// actually monitors is small, known, and theirs to state.
//
// An EMPTY list allows everything — the pre-filter behaviour, so adding
// this key is opt-in and no existing deployment changes.
//
// Comparison is exact after trimming. Deliberately NOT prefix-matching:
// `staging` must not admit `staging-ru`, which is a real and separate
// environment in this fleet, and an operator who wants both says both.
func EnvMayRegister(allowed []string, env string) bool {
	if len(allowed) == 0 {
		return true
	}
	env = strings.TrimSpace(env)
	if env == "" {
		return false
	}
	for _, a := range allowed {
		if strings.TrimSpace(a) == env {
			return true
		}
	}
	return false
}

// TenantEnvironmentsFromConfig reads the allowlist off the component's
// operator config.
//
// Tolerates the shapes that reach a plugin: a []string, a single string
// (one environment), and the []any a list becomes after the JSON
// round-trip over the plugin bus — which is the shape it ACTUALLY
// arrives in, the others being defensive.
func TenantEnvironmentsFromConfig(raw map[string]any) []string {
	if raw == nil {
		return nil
	}
	switch v := raw[TenantEnvironmentsKey].(type) {
	case []string:
		return v
	case string:
		if s := strings.TrimSpace(v); s != "" {
			return []string{s}
		}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}
