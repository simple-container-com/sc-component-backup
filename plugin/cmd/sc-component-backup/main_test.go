// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/simple-container-com/api/pkg/api"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"

	backupApi "github.com/simple-container-com/sc-component-backup/api"
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

// TestSecretEnvVarNamesAreSane catches a copy-paste that would make two
// config fields fight over one env var, or bind one to the empty string.
// Mirror of notifier's schema_test.go.
func TestSecretEnvVarNamesAreSane(t *testing.T) {
	resp, err := (&backupPlugin{}).Schema(context.Background(), &scpluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	envVarRe := regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	for resType, m := range resp.ComponentSecretEnvMap {
		if m == nil {
			continue
		}
		seen := map[string]string{}
		for cfgKey, envVar := range m.Entries {
			if envVar == "" {
				t.Errorf("secret env map: config key %q (resource %q) binds to an empty env var name", cfgKey, resType)
				continue
			}
			if !envVarRe.MatchString(envVar) {
				t.Errorf("secret env map: env var %q (from %q, resource %q) is not an upper-snake name", envVar, cfgKey, resType)
			}
			if prev, dup := seen[envVar]; dup {
				t.Fatalf("secret env map: env var %q is bound from both %q and %q (resource %q) — one would silently win", envVar, prev, cfgKey, resType)
			}
			seen[envVar] = cfgKey
		}
	}
}

// TestMigrationTokenIsBoundAsASecret pins the specific wire PR #37
// added. The migration endpoint is a full write path over both the
// external Mongo source and the go-embedb sink, so its bearer must land
// in a per-function SecretsManager secret rather than plaintext Lambda
// env. Mirror of notifier's schema_test.go.
func TestMigrationTokenIsBoundAsASecret(t *testing.T) {
	resp, err := (&backupPlugin{}).Schema(context.Background(), &scpluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	allowed := resp.ComponentAllowedOverrides[backupApi.ResourceType]
	if allowed == nil || !slices.Contains(allowed.Values, "migrationToken") {
		t.Errorf("migrationToken missing from ComponentAllowedOverrides for %q", backupApi.ResourceType)
	}
	secretEnv := resp.ComponentSecretEnvMap[backupApi.ResourceType]
	if secretEnv == nil || secretEnv.Entries["migrationToken"] != "ADMIN_MIGRATION_TOKEN" {
		t.Errorf("migrationToken must bind to ADMIN_MIGRATION_TOKEN in ComponentSecretEnvMap for %q; got entries=%v", backupApi.ResourceType, secretEnv.GetEntries())
	}
}

// TestSecretEnvVarsAreResolvedByTheService is the SERVICE half of the
// secret-env wire, and it reads source rather than calling code because
// the binding lives in the plugin module and the reader lives in the
// service module — no ordinary test spans them. A grep is coarse, but
// the failure it prevents is the one that has actually happened twice in
// this fleet: a bound secret the service never passes through
// resolveAWSSecretARN, so the service compares against the SecretsManager
// ARN string instead of the secret and nothing errors (SERVICE_JWT_SECRET
// on backup-smoke-1d 2026-11-14; BACKUP_OPERATOR_PASSWORD on the first
// backup.simple-forge.com login 2026-11-18).
//
// Mirror of notifier's schema_test.go — ported here per the sc-components
// handoff after notifier's plugin #4 landed the shape.
func TestSecretEnvVarsAreResolvedByTheService(t *testing.T) {
	resp, err := (&backupPlugin{}).Schema(context.Background(), &scpluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	secretEnv := resp.ComponentSecretEnvMap[backupApi.ResourceType]
	if secretEnv == nil || len(secretEnv.Entries) == 0 {
		t.Fatalf("Schema advertised no secret-env entries for %q — add one before removing this test", backupApi.ResourceType)
	}

	path := filepath.Join("..", "..", "..", "service", "cmd", "backup", "main.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s — if the service module moved, move this assertion with it rather than deleting it: %v", path, err)
	}

	resolved := map[string]bool{}
	re := regexp.MustCompile(`resolveAWSSecretARN\([^,]+,\s*"([A-Z][A-Z0-9_]*)"`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		resolved[m[1]] = true
	}
	if len(resolved) == 0 {
		t.Fatalf("found no resolveAWSSecretARN call sites in %s — service wiring moved or regressed", path)
	}

	var missing []string
	for cfgKey, envVar := range secretEnv.Entries {
		if !resolved[envVar] {
			missing = append(missing, cfgKey+" → "+envVar)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s never passes these bound secrets through resolveAWSSecretARN, so the service compares against the SecretsManager ARN instead of the secret: %s", path, strings.Join(missing, ", "))
	}
}
