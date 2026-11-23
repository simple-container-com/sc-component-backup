// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"strings"
	"testing"
)

// The absent case has to stay silent. Every operator who upgrades this
// plugin without asking for notifications must keep deploying exactly
// as before — an error here would break every existing stack on the
// release that adds the feature.
func TestNotifyAbsentIsNotAnError(t *testing.T) {
	for _, raw := range []map[string]any{
		nil,
		{},
		{"notify": nil},
		{"storage": "sc-backup-data"},
	} {
		got, err := ValidateNotifyWiring(raw)
		if err != nil {
			t.Fatalf("raw %v: unexpected error: %v", raw, err)
		}
		if got.Declared {
			t.Errorf("raw %v: Declared should be false", raw)
		}
		if got.URL != "" {
			t.Errorf("raw %v: URL should be empty, got %q", raw, got.URL)
		}
	}
}

// Shape 1, the explicit form, in both spellings an operator writes.
func TestNotifyExplicitURL(t *testing.T) {
	for _, raw := range []map[string]any{
		{"notify": map[string]any{"url": "https://notify.simple-forge.com/"}, "notifyToken": "${secret:t}"},
		{"notify": "https://notify.simple-forge.com", "notifyToken": "${secret:t}"},
		{"notify": "  notify.simple-forge.com  ", "notifyToken": "${secret:t}"},
	} {
		got, err := ValidateNotifyWiring(raw)
		if err != nil {
			t.Fatalf("raw %v: unexpected error: %v", raw, err)
		}
		if !got.Declared {
			t.Errorf("raw %v: Declared should be true", raw)
		}
		if want := "https://notify.simple-forge.com"; got.URL != want {
			t.Errorf("raw %v: URL = %q, want %q", raw, got.URL, want)
		}
	}
}

// THE regression test the plan names: an unresolvable `notify:` fails
// rather than deploying quietly.
//
// Each case asserts on the MESSAGE, not just on non-nil. These errors
// are the entire diagnostic surface for "notifications don't work",
// which is otherwise a four-layer debug across plugin, env, token and
// channel — an error that says "invalid config" costs the operator the
// same hour the check was meant to save.
func TestUnresolvableNotifyFailsAndNamesTheFix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   map[string]any
		wants []string
	}{
		{
			// SC leaves a placeholder it did not resolve in place
			// verbatim. Without this guard the deploy is green and
			// every event POST goes to a hostname beginning with a
			// dollar sign — live-proven on the healthcheck tick,
			// 2026-11-19.
			name: "unresolved tpl-extension placeholder",
			raw: map[string]any{
				"notify":      map[string]any{"url": "$sc:${resource:my-notifier.self-url}"},
				"notifyToken": "${secret:t}",
			},
			wants: []string{"unresolved SC placeholder", "self-url", "declared BEFORE"},
		},
		{
			name: "unresolved secret reference",
			raw: map[string]any{
				"notify":      "${secret:notifier-url}",
				"notifyToken": "${secret:t}",
			},
			wants: []string{"unresolved SC placeholder"},
		},
		{
			// Shape 2 — the one word. Honest about why it does not
			// work yet AND prints the shape-3 line to paste instead.
			name:  "bare resource reference",
			raw:   map[string]any{"notify": "my-notifier", "notifyToken": "${secret:t}"},
			wants: []string{"references an SC resource by name", "self-url", "my-notifier"},
		},
		{
			name:  "notify block with no url",
			raw:   map[string]any{"notify": map[string]any{"minSeverity": "warning"}},
			wants: []string{"no `url`", "remove the `notify:` block"},
		},
		{
			name:  "notify names a resource but gives no url",
			raw:   map[string]any{"notify": map[string]any{"name": "my-notifier"}},
			wants: []string{"names a resource but no URL", "my-notifier.self-url"},
		},
		{
			name:  "notify present but blank",
			raw:   map[string]any{"notify": "   "},
			wants: []string{"present but empty", "remove the key"},
		},
		{
			name:  "notify is the wrong type",
			raw:   map[string]any{"notify": 42},
			wants: []string{"must be a URL string or a map"},
		},
		{
			// A publisher that can reach the notifier but cannot
			// authenticate gets a 401 on its first real alert, at the
			// exact moment the operator is relying on it.
			name:  "url without a token",
			raw:   map[string]any{"notify": "https://notify.simple-forge.com"},
			wants: []string{"notifyToken", "401", "sc secrets add"},
		},
		{
			name:  "token without a url",
			raw:   map[string]any{"notifyToken": "${secret:t}"},
			wants: []string{"notifyToken", "no `notify:` block"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateNotifyWiring(tc.raw)
			if err == nil {
				t.Fatalf("expected an error, got target %+v", got)
			}
			if got.Declared {
				t.Error("a failed resolution must not return a Declared target")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message does not mention %q:\n  %v", want, err)
				}
			}
		})
	}
}

// The by-reference shape in map form resolves once the operator writes
// the placeholder themselves and SC substitutes it — and the resource
// name is carried through so the log line can name it.
func TestNotifyResourceNameIsCarriedThrough(t *testing.T) {
	got, err := ValidateNotifyWiring(map[string]any{
		"notify": map[string]any{
			"name": "my-notifier",
			"url":  "https://abc123.lambda-url.eu-central-1.on.aws",
		},
		"notifyToken": "${secret:t}",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Resource != "my-notifier" {
		t.Errorf("Resource = %q, want %q", got.Resource, "my-notifier")
	}
	if !strings.Contains(NotifyEnvDescription(got), `via resource "my-notifier"`) {
		t.Errorf("description should name the resource: %q", NotifyEnvDescription(got))
	}
	if strings.Contains(NotifyEnvDescription(got), "secret") {
		t.Error("the description must never carry the credential")
	}
}

func TestNotifyEnvDescriptionOffCase(t *testing.T) {
	if d := NotifyEnvDescription(NotifyTarget{}); !strings.Contains(d, "notifications off") {
		t.Errorf("got %q", d)
	}
}

// A non-string `notifyToken` must not read as "declared". An operator
// who writes `notifyToken: 12345` (unquoted, so YAML makes it an int)
// would otherwise pass the pair check and deploy with no credential.
func TestNotifyTokenMustBeAString(t *testing.T) {
	if NotifyTokenDeclared(map[string]any{"notifyToken": 12345}) {
		t.Error("a non-string token must not count as declared")
	}
	if NotifyTokenDeclared(map[string]any{"notifyToken": "  "}) {
		t.Error("a whitespace-only token must not count as declared")
	}
	if !NotifyTokenDeclared(map[string]any{"notifyToken": "${secret:t}"}) {
		t.Error("a secret reference is a declared token")
	}
}
