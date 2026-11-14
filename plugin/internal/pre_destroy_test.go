// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"testing"

	. "github.com/onsi/gomega"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// PreDestroy for the backup is a fire-and-forget log + return. A real
// component's test would assert on the cleanup side effects — for backup,
// we just prove the hook is invokable with the Phase 1e v2 request shape.
func TestPreDestroy_FireAndForget(t *testing.T) {
	RegisterTestingT(t)

	pub := scPlugin.NewPublisher(0)
	req := &scpluginv1.PreDestroyRequest{
		Header:       &scpluginv1.RequestHeader{StackName: "smoke-backup", Environment: "smoke"},
		ResourceType: "sc-backup",
	}

	err := PreDestroy(t.Context(), pub, req)
	Expect(err).ToNot(HaveOccurred())
}

// Slice 1d.5: discoverComponentPrefixes walks stack outputs for every
// `<prefix>-atlas-admin-public-key` key. Covers single + multi-component
// stacks + the no-opt-in degraded path.
func TestDiscoverComponentPrefixes(t *testing.T) {
	cases := []struct {
		name string
		in   map[string][]byte
		want int
	}{
		{"empty", map[string][]byte{}, 0},
		{"non-slice-1d stack (no atlasAdmin export)", map[string][]byte{
			"my-backup-service-url": []byte(`"https://x"`),
		}, 0},
		{"single component", map[string][]byte{
			"my-backup-atlas-admin-public-key":  []byte(`"pub"`),
			"my-backup-atlas-admin-private-key": []byte(`"priv"`),
			"my-backup-service-url":             []byte(`"https://x"`),
		}, 1},
		{"two components", map[string][]byte{
			"my-backup-atlas-admin-public-key": []byte(`"pub"`),
			"my-other-atlas-admin-public-key":  []byte(`"pub2"`),
			"my-backup-service-url":            []byte(`"https://x"`),
		}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := discoverComponentPrefixes(tc.in); len(got) != tc.want {
				t.Errorf("got %v (len=%d), want len=%d", got, len(got), tc.want)
			}
		})
	}
}

// extractUsernameFromMongoURI is the one pre-slice-1d.5 regression risk
// — the scoped-user name is parsed out of the service's stored URI, and
// a subtle URI shape change would silently miss users. Pin the shapes.
func TestExtractUsernameFromMongoURI(t *testing.T) {
	cases := map[string]string{
		"mongodb://backup-forge:pw@h:27017/":         "backup-forge",
		"mongodb+srv://backup-x:pw@h/?replicaSet=rs": "backup-x",
		"mongodb://no-at-sign/":                      "",
		"":                                           "",
		"postgres://a:b@c":                           "", // not a mongo URI
	}
	for uri, want := range cases {
		if got := extractUsernameFromMongoURI(uri); got != want {
			t.Errorf("uri=%q: got %q, want %q", uri, got, want)
		}
	}
}

// readStackOutputString handles both the auto.OutputValue wire shape and
// the older plain-JSON-quoted-string fallback. The second path is live
// on cores that pre-date the structured wire.
func TestReadStackOutputString(t *testing.T) {
	cases := map[string]struct {
		raw  []byte
		want string
	}{
		"structured":   {[]byte(`{"value":"hello","secret":false}`), "hello"},
		"secret-wrap":  {[]byte(`{"value":"shh","secret":true}`), "shh"},
		"plain-quoted": {[]byte(`"old-core-shape"`), "old-core-shape"},
		"missing":      {nil, ""},
		"non-string":   {[]byte(`{"value":42,"secret":false}`), ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			outputs := map[string][]byte{}
			if tc.raw != nil {
				outputs["k"] = tc.raw
			}
			if got := readStackOutputString(outputs, "k"); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
