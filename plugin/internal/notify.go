// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
)

// The `notify:` block tells this component where to publish operator-
// relevant events — a service going down, a backup failing — so that
// sc-component-notifier can turn them into a Telegram message or a
// signed webhook POST.
//
// It is OPTIONAL by design. A component with no `notify:` block deploys
// and runs exactly as before; nothing is emitted and nothing warns.
// That keeps a zero-config deploy working and is why the absent case
// below returns a zero value and a nil error rather than an error.
//
// Everything else fails the provision. A publisher that deploys with
// notifications silently off is the worst available outcome: the
// operator believes they are covered, the SPA shows a channel, and the
// first thing they learn is that nobody was paged. So an unresolvable
// reference, a blank value, or a URL still carrying an unresolved SC
// placeholder all stop the deploy with a message naming the exact key
// to fix.

// notifyTokenField is the operator config field holding the publisher's
// event credential. It is a SEPARATE field from `notify:` and not part
// of it, because the transport differs: `notify.url` rides a plain tpl
// extension into plain env, while the token must reach the service as a
// SecretsManager-backed secret — which only happens for a TOP-LEVEL
// field registered in the plugin's ComponentSecretEnvMap
// (api/pkg/clouds/pulumi/component_secret_env.go reads `merged[field]`
// and nothing nested).
const notifyTokenField = "notifyToken"

// unresolvedMarkers are the SC placeholder prefixes that must never
// survive into a value this plugin hands onward.
//
// SC resolves `${secret:…}` and the `$sc:${resource:…}` tpl-extension
// form at well-defined points in the deploy, and NOT everywhere: a
// placeholder inside an EventBridge schedule's request payload is
// passed through literally, which on 2026-11-19 made the observatory
// healthcheck tick answer 401 twice with `Bearer
// $sc:${resource:${name}.scheduler-token}` in the rule's input. The
// same class of silence applied to a notifier URL would mean events
// POSTed to a hostname that is a literal dollar sign.
var unresolvedMarkers = []string{"${secret:", "${resource:", "$sc:", "${auth:"}

// NotifyTarget is the resolved `notify:` declaration.
type NotifyTarget struct {
	// Declared is true when the operator wrote a `notify:` block at
	// all. Distinguishes "notifications off" from "notifications on
	// and the URL happens to be empty", which must never happen.
	Declared bool

	// URL is the notifier service's base URL, scheme-qualified and
	// without a trailing slash. Empty only when Declared is false.
	URL string

	// Resource is the SC resource name the operator referenced, when
	// they used the by-reference shape. Informational — recorded so a
	// log line and the error message can name it.
	Resource string
}

// NotifyFromConfig resolves the operator's `notify:` declaration into a
// base URL this component's service can POST events to.
//
// Accepted shapes, in the preference order the ladder documents:
//
//  1. `notify: {url: "https://notify.simple-forge.com"}` — explicit.
//     Always works, and is what a smoke uses on step one.
//  2. `notify: my-notifier` — the resource reference, one word. Needs
//     SC to resolve a `${resource:…}` reference that a PLUGIN emitted
//     rather than one the operator wrote, which is unproven; until the
//     wiring spike settles it this shape fails with a message naming
//     shape 3 as the thing to write instead.
//  3. `notify: {url: "$$sc:${resource:my-notifier.self-url}"}` — the
//     reference written by the operator, so it rides the placeholder
//     resolution pass that definitely runs. One line instead of one
//     word, and still no hardcoded hostname.
//
// A bare string is read as shape 1 when it looks like a URL or a
// hostname (it contains `://` or a dot) and as shape 2 otherwise. SC
// resource names in this fleet are hyphenated identifiers, so the two
// do not overlap in practice; the rule is stated here because it is the
// kind of heuristic that is invisible until it misfires.
//
// Ordering is load-bearing and documented, not fixed: the notifier
// resource must be declared BEFORE any publisher in server.yaml, the
// same class of trap as `uses:` ordering.
func NotifyFromConfig(raw map[string]any) (NotifyTarget, error) {
	v, present := raw["notify"]
	if !present || v == nil {
		return NotifyTarget{}, nil
	}

	switch n := v.(type) {
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			// A key written with no value is a half-finished edit, not
			// a decision to disable notifications. Deleting the key is
			// how you do that, and the message says so.
			return NotifyTarget{}, errors.Errorf(
				"`notify:` is present but empty — give it a notifier URL (`notify: {url: \"https://…\"}`) or remove the key entirely to leave notifications off",
			)
		}
		// The placeholder check runs BEFORE the shape discrimination.
		// `${secret:notifier-url}` contains neither `://` nor a dot, so
		// looksLikeURL rejects it and the resource-reference branch
		// below would answer an unresolved-placeholder config with a
		// message about SC resource names — telling the operator to
		// write `${resource:${secret:notifier-url}.self-url}`. Caught by
		// TestUnresolvableNotifyFailsAndNamesTheFix on first run.
		if err := rejectUnresolved("notify", s); err != nil {
			return NotifyTarget{}, err
		}
		if looksLikeURL(s) {
			return targetFromURL(s, "")
		}
		return NotifyTarget{}, errors.Errorf(
			"`notify: %[1]s` references an SC resource by name, which this version cannot resolve: a plugin cannot read a sibling resource's declaration, and a `${resource:…}` reference emitted by a plugin is not re-resolved by SC. Write the reference yourself instead — `notify: {url: \"$$sc:${resource:%[1]s.self-url}\"}` — or give the URL explicitly: `notify: {url: \"https://…\"}`",
			s,
		)

	case map[string]any:
		resource := stringField(n, "name")
		u := stringField(n, "url")
		if u == "" {
			if resource != "" {
				return NotifyTarget{}, errors.Errorf(
					"`notify.name: %[1]s` names a resource but no URL: add `notify.url: \"$$sc:${resource:%[1]s.self-url}\"`, which SC resolves because you wrote it",
					resource,
				)
			}
			return NotifyTarget{}, errors.New(
				"`notify:` is declared with no `url` — add `notify.url: \"https://…\"` (or `\"$$sc:${resource:<notifier-resource>.self-url}\"`), or remove the `notify:` block to leave notifications off",
			)
		}
		return targetFromURL(u, resource)

	default:
		return NotifyTarget{}, errors.Errorf(
			"`notify:` must be a URL string or a map with a `url` key, got %T", v,
		)
	}
}

// targetFromURL validates a URL that reached us from operator config
// and normalises it.
//
// The unresolved-placeholder check is the loud-failure guard: SC leaves
// a placeholder it did not resolve in place verbatim, so without this
// the deploy succeeds and every event POST goes to a hostname that
// starts with a dollar sign. The error names the key and the literal
// value, because the two shapes that produce it — a misspelled export
// key and a context where SC does not substitute at all — look
// identical from the outside and the operator needs the raw string to
// tell them apart.
func targetFromURL(u, resource string) (NotifyTarget, error) {
	u = strings.TrimSpace(u)
	if err := rejectUnresolved("notify.url", u); err != nil {
		return NotifyTarget{}, err
	}
	normalized := normalizeURL(u)
	if normalized == "" {
		return NotifyTarget{}, errors.Errorf("`notify.url` is not a usable URL: %q", u)
	}
	return NotifyTarget{Declared: true, URL: normalized, Resource: resource}, nil
}

// rejectUnresolved fails when a value SC was supposed to substitute
// still carries a placeholder.
//
// `key` names the config key in the message because the same value
// arrives from two shapes (`notify:` and `notify.url:`) and an operator
// grepping their server.yaml for the wrong one finds nothing.
func rejectUnresolved(key, value string) error {
	for _, marker := range unresolvedMarkers {
		if strings.Contains(value, marker) {
			return errors.Errorf(
				"`%s` still holds an unresolved SC placeholder (%q) — SC did not substitute it, so the notifier URL would be a literal. Check the export key exists on the notifier resource (`self-url`), and that the resource is declared BEFORE this component in server.yaml",
				key, value,
			)
		}
	}
	return nil
}

// NotifyTokenDeclared reports whether the operator supplied the
// publisher's event credential.
//
// Checked separately from the URL and enforced together with it: a
// publisher that can reach the notifier but cannot authenticate gets a
// 401 on its first real alert, at the exact moment the operator is
// relying on it. Failing the provision instead is the whole point of
// the loud-failure rule.
//
// The value is NOT validated beyond being non-blank. At this point in
// the deploy it may be the resolved JWT or still be a `${secret:…}`
// reference depending on where SC's resolution pass has reached, and
// guessing wrong would reject a correct config.
func NotifyTokenDeclared(raw map[string]any) bool {
	return strings.TrimSpace(stringField(raw, notifyTokenField)) != ""
}

// ValidateNotifyWiring enforces the url+token pair.
//
// Returns the resolved target. The error names which half is missing
// and the config key that supplies it, because "notifications don't
// work" is otherwise a four-layer debug: plugin, env, token, channel.
func ValidateNotifyWiring(raw map[string]any) (NotifyTarget, error) {
	target, err := NotifyFromConfig(raw)
	if err != nil {
		return NotifyTarget{}, err
	}
	if !target.Declared {
		// No `notify:` and no token: notifications off, deliberately.
		// A token WITHOUT a notify block is a half-wired config and
		// worth saying so, because the operator clearly intended to
		// turn this on.
		if NotifyTokenDeclared(raw) {
			return NotifyTarget{}, errors.Errorf(
				"`%s` is set but there is no `notify:` block — add `notify: {url: \"https://…\"}` so the token has somewhere to be presented",
				notifyTokenField,
			)
		}
		return NotifyTarget{}, nil
	}
	if !NotifyTokenDeclared(raw) {
		return NotifyTarget{}, errors.Errorf(
			"`notify.url` is set to %s but `%s` is not — the publisher would reach the notifier and be refused with a 401 on its first alert. Mint a publisher token on the notifier, register it with `sc secrets add`, and set `%s: \"${secret:<name>}\"`",
			target.URL, notifyTokenField, notifyTokenField,
		)
	}
	return target, nil
}

// normalizeURL accepts a bare hostname or a full URL and returns a
// scheme-qualified base with no trailing slash.
//
// Bare hostnames get https:// — an SC Component always sits behind TLS,
// and defaulting to http would produce a redirect-or-fail on every
// event POST.
//
// Whitespace comes off FIRST: trimming slashes from a value that still
// has a trailing space is a no-op, so "https://host/ " would keep its
// slash and every POST would go to //api/v1/events. That exact bug
// shipped in sc-component-observatory until 2026-11-23.
func normalizeURL(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://" + s
}

// looksLikeURL distinguishes shape 1 from shape 2 for a bare string.
func looksLikeURL(s string) bool {
	return strings.Contains(s, "://") || strings.Contains(s, ".")
}

// stringField reads a string off an untyped config map, tolerating a
// missing key and a non-string value.
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// NotifyEnvDescription renders the resolved wiring for a progress log
// line. Never includes the token.
func NotifyEnvDescription(t NotifyTarget) string {
	if !t.Declared {
		return "notifications off (no `notify:` block)"
	}
	if t.Resource != "" {
		return fmt.Sprintf("notify → %s (via resource %q)", t.URL, t.Resource)
	}
	return fmt.Sprintf("notify → %s", t.URL)
}
