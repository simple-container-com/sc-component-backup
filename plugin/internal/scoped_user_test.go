// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"strings"
	"testing"

	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
)

func TestScopedUsername(t *testing.T) {
	cases := map[string]string{
		"forge-conductor":       "backup-forge-conductor",
		"":                      "backup-",
		strings.Repeat("x", 80): "backup-" + strings.Repeat("x", 57), // 64 total
	}
	for in, want := range cases {
		if got := ScopedUsername(in); got != want {
			t.Errorf("ScopedUsername(%q) = %q, want %q", in, got, want)
		}
		if len(ScopedUsername(in)) > 64 {
			t.Errorf("exceeded 64-char Atlas cap: %q", ScopedUsername(in))
		}
	}
}

func TestPopulateDatabasesFromRaw_Empty(t *testing.T) {
	for _, raw := range []map[string]any{nil, {}} {
		dbs, admin, err := populateDatabasesFromRaw(raw)
		if err != nil {
			t.Fatalf("unexpected err on empty raw: %v", err)
		}
		if dbs != nil || admin != nil {
			t.Errorf("empty raw: got dbs=%v admin=%v", dbs, admin)
		}
	}
}

func TestPopulateDatabasesFromRaw_HappyPath(t *testing.T) {
	raw := map[string]any{
		"databases": []any{
			map[string]any{"resource": "the-mongo", "driver": "mongodb"},
			map[string]any{"resource": "the-rds", "driver": "postgres"},
		},
		"atlasAdmin": map[string]any{
			"privateKey": "priv",
			"publicKey":  "pub",
			"projectId":  "proj1",
		},
	}
	dbs, admin, err := populateDatabasesFromRaw(raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(dbs) != 2 {
		t.Fatalf("dbs count: %d", len(dbs))
	}
	if dbs[0].Resource != "the-mongo" || dbs[0].Driver != "mongodb" {
		t.Errorf("db0: %+v", dbs[0])
	}
	if admin == nil || admin.PrivateKey != "priv" || admin.PublicKey != "pub" || admin.ProjectID != "proj1" {
		t.Errorf("admin: %+v", admin)
	}
}

func TestPopulateDatabasesFromRaw_MongoWithoutAdminFails(t *testing.T) {
	raw := map[string]any{
		"databases": []any{
			map[string]any{"resource": "the-mongo", "driver": "mongodb"},
		},
	}
	_, _, err := populateDatabasesFromRaw(raw)
	if err == nil || !strings.Contains(err.Error(), "atlasAdmin not set") {
		t.Fatalf("expected admin-required err, got %v", err)
	}
}

func TestPopulateDatabasesFromRaw_MissingFields(t *testing.T) {
	raw := map[string]any{
		"databases": []any{
			map[string]any{"driver": "mongodb"},
		},
	}
	_, _, err := populateDatabasesFromRaw(raw)
	if err == nil || !strings.Contains(err.Error(), "resource + driver required") {
		t.Fatalf("expected missing-resource err, got %v", err)
	}
}

// mkDeps builds a `ResourcesOutputs` fixture — the shape plugin bus
// unmarshals a `ComputeProcessRequest.DependencyOutputsJson` into.
func mkDeps(t *testing.T, m map[string]map[string]any) pApi.ResourcesOutputs {
	t.Helper()
	out := make(pApi.ResourcesOutputs, len(m))
	for name, kv := range m {
		pro := &api.PluginResourceOutput{
			PluginName: "test",
			Urn:        "urn:pulumi:test::test::aws:foo::" + name,
			Outputs:    make(map[string]api.PluginOutputValue, len(kv)),
		}
		for k, v := range kv {
			pro.Outputs[k] = api.PluginOutputValue{Value: v}
		}
		out[name] = &api.ResourceOutput{Ref: pro}
	}
	return out
}

// Slice 1d.7: populateAtlasAdminFromRaw extracts just the atlasAdmin
// block. Replaces the three-field return of the old
// populateDatabasesFromRaw for callers that only need the admin creds.
func TestPopulateAtlasAdminFromRaw(t *testing.T) {
	cases := map[string]struct {
		raw  map[string]any
		want *AtlasAdminRef
	}{
		"absent":  {map[string]any{}, nil},
		"nil-map": {nil, nil},
		"present": {
			map[string]any{"atlasAdmin": map[string]any{
				"privateKey": "p", "publicKey": "pub", "projectId": "x",
			}},
			&AtlasAdminRef{PrivateKey: "p", PublicKey: "pub", ProjectID: "x"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := populateAtlasAdminFromRaw(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Errorf("got=%+v want=%+v", got, tc.want)
			}
		})
	}
}

// Keep mkDeps even though MatchConsumerToDatabase is gone — may be used
// by future tests for StackReference-based readers.
var _ = mkDeps
