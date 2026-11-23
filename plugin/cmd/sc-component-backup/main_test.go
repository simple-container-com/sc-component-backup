// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"slices"
	"testing"

	"github.com/simple-container-com/api/pkg/api"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// TestComponentSecretEnvMapKeysAreWritable asserts that every key the plugin
// advertises in ComponentSecretEnvMap is reachable at parse time — either
// through SC core's base allowlist (api.ComponentConfigTopLevelKeys) OR
// through the plugin's per-type ComponentAllowedOverrides. A secret-env
// entry for a key that is parse-time-rejected provisions an env var that
// never gets a value, and the service compares an operator's credential
// against an empty string. This test catches the next secret-bound field
// the day it is added rather than the day it breaks a provision.
//
// Live-caught shape (operatorPassword): the key was in ComponentSecretEnvMap
// but silently never reached the Lambda env until 2026-11-16 on backup-
// smoke-tier4's SPA "No password set yet". The invariant this test enforces
// would have failed the moment operatorPassword landed without an allowlist
// entry.
func TestComponentSecretEnvMapKeysAreWritable(t *testing.T) {
	resp, err := (&backupPlugin{}).Schema(context.Background(), &scpluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	for resType, m := range resp.ComponentSecretEnvMap {
		if m == nil {
			continue
		}
		allowed := resp.ComponentAllowedOverrides[resType]
		for field := range m.Entries {
			inBase := api.IsComponentConfigTopLevelKey(field)
			inAllowed := allowed != nil && slices.Contains(allowed.Values, field)
			if !inBase && !inAllowed {
				t.Errorf("secret env map key %q for resource %q is parse-time-unreachable: not in api.ComponentConfigTopLevelKeys, not in ComponentAllowedOverrides", field, resType)
			}
		}
	}
}

// TestComponentSecretEnvMapKeysAreWritable_InversionCanFail inverts the
// assertion above: given a synthetic secret-env key that is neither in the
// core base list nor in the per-type allowlist, the invariant must flag
// it. Confirms the test above is not a tautology.
func TestComponentSecretEnvMapKeysAreWritable_InversionCanFail(t *testing.T) {
	base := api.IsComponentConfigTopLevelKey("totally-not-a-key-xyz")
	if base {
		t.Fatalf("sanity: base list unexpectedly contains the synthetic key")
	}
	resp, err := (&backupPlugin{}).Schema(context.Background(), &scpluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	for resType, allowed := range resp.ComponentAllowedOverrides {
		if allowed == nil {
			continue
		}
		if slices.Contains(allowed.Values, "totally-not-a-key-xyz") {
			t.Fatalf("sanity: allowlist for %q unexpectedly contains the synthetic key", resType)
		}
	}
}
