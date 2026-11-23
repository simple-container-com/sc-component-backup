// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// This file is the publisher half of sc-component-notifier: it turns a
// terminal backup/restore status into one POST, and does nothing else.
// Routing, channel selection, severity gating, retries, deduplication
// and cooldown all live in the notifier — which is the entire reason
// the notifier exists as a separate component rather than as another
// Telegram client in here (telegram.go is that, wired only to a manual
// "Test notify" button, and is deprecated by this path).
//
// Note what is NOT here: no goroutine, no retry loop, no queue. A
// Lambda freezes background goroutines the instant the handler returns,
// so anything deferred to one silently never happens. The POST is made
// inline on the caller's goroutine or not at all.

const (
	// eventsPath is the notifier's ingest route.
	eventsPath = "/api/v1/events"

	// publishTimeout is deliberately generous. A 3-second timeout on a
	// Lambda→Lambda POST lost real deliveries in forge-notifier because
	// the receiving function was cold; the fix was 15s. The publisher
	// is on the terminal path of a backup that already took minutes, so
	// 15s of worst-case latency costs nothing and a missed alert costs
	// the whole feature.
	publishTimeout = 15 * time.Second

	// userAgent — Cloudflare answers the default Go UA with a 403
	// (error 1010) on some zones, and the notifier may well sit behind
	// one.
	userAgent = "sc-component-backup/notify (+https://github.com/simple-container-com/sc-component-backup)"
)

// Severity levels, matching the notifier's ordinal gate
// (info=1, warning=2, error=3, critical=4; anything unknown sorts to 0
// and therefore passes no minimum).
//
// Anything operator-ACTIONABLE must be emitted at warning or above.
// This is a scar, not a style preference: in forge-notifier every
// Telegram subscription was `severityAtLeast: warning`, so every `info`
// notification ever sent silently vanished for months. A failed backup
// is an error; a cancelled one is a warning; a successful one is info
// and is therefore off by default on every channel.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityError    = "error"
	SeverityCritical = "critical"
)

// Event kinds this component publishes.
const (
	KindBackupFailed    = "backup.failed"
	KindBackupCancelled = "backup.cancelled"
	KindBackupSucceeded = "backup.succeeded"
	KindRestoreFailed   = "restore.failed"
)

// unresolvedMarkers are the SC placeholder prefixes that must never
// appear in a configured URL. SC leaves a placeholder it did not
// resolve in place verbatim, so without this check the service starts
// happily and POSTs every event to a hostname beginning with a dollar
// sign — the exact shape that made the observatory healthcheck tick
// answer 401 twice on 2026-11-19.
var unresolvedMarkers = []string{"${secret:", "${resource:", "$sc:", "${auth:"}

// Event is the wire shape of POST /api/v1/events.
type Event struct {
	// ID is a publisher-supplied idempotency key, deduped by the
	// notifier for 24h. Derive it from the thing that happened, never
	// from the clock: a re-fired scheduler tick must produce the same
	// ID so it cannot double-send. See BackupRunEventID.
	ID string `json:"id"`

	// Source identifies the publisher. The notifier overrides this
	// from the `tenant` claim on the token, so a publisher cannot
	// impersonate another; it is sent for log readability.
	Source string `json:"source"`

	Kind     string            `json:"kind"`
	Severity string            `json:"severity"`
	Subject  string            `json:"subject"`
	Title    string            `json:"title"`
	Body     string            `json:"body,omitempty"`
	URL      string            `json:"url,omitempty"`
	At       time.Time         `json:"at"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// Publisher is the seam the scheduler depends on, so a test can assert
// what would have been published without an HTTP server.
type Publisher interface {
	Publish(ctx context.Context, ev Event) error
	Enabled() bool

	// RunLink builds the deep link an operator taps from the message,
	// or "" when the service does not know its own public URL.
	RunLink(path string) string
}

// EmitterConfig is the service's notify wiring, read from env.
type EmitterConfig struct {
	// BaseURL is NOTIFY_URL. Empty means notifications are off, which
	// is a legitimate configuration and not an error.
	BaseURL string

	// Token is NOTIFY_TOKEN — the notifier-minted `op:"event"` JWT for
	// this tenant.
	Token string

	// SelfURL, when set, is used to build the deep link an operator
	// taps to see the failing run.
	SelfURL string

	HTTPClient *http.Client
	Log        *slog.Logger
}

// Emitter publishes events to a notifier service.
type Emitter struct {
	baseURL string
	token   string
	selfURL string
	http    *http.Client
	log     *slog.Logger
}

// Disabled is the no-op publisher used when no notifier is configured.
// A separate type rather than a nil check at every call site, so
// "notifications off" cannot be confused with "forgot to wire it".
type Disabled struct{}

func (Disabled) Publish(context.Context, Event) error { return nil }
func (Disabled) Enabled() bool                        { return false }
func (Disabled) RunLink(string) string                { return "" }

// NewEmitter builds the publisher from the service's env.
//
// The three outcomes are deliberately distinct:
//
//   - No BaseURL → (Disabled{}, nil). The operator declared no
//     `notify:` block. Every existing stack keeps working on the
//     release that adds this.
//   - BaseURL holds an unresolved SC placeholder, or BaseURL without a
//     Token → error. Both mean the wiring is broken in a way that
//     would only surface at the first real incident: a POST to a
//     literal `$sc:…` host, or a 401. Failing at STARTUP makes it a
//     deploy failure instead of a silent gap, which is the whole point
//     of shipping a notifier at all.
//   - Otherwise → a live Emitter.
func NewEmitter(cfg EmitterConfig) (Publisher, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	token := strings.TrimSpace(cfg.Token)

	if base == "" {
		if token != "" {
			return nil, fmt.Errorf("NOTIFY_TOKEN is set but NOTIFY_URL is empty — the component's `notify:` block is half-wired")
		}
		log.Info("notifications off: NOTIFY_URL is not set")
		return Disabled{}, nil
	}
	for _, marker := range unresolvedMarkers {
		if strings.Contains(base, marker) {
			return nil, fmt.Errorf(
				"NOTIFY_URL holds an unresolved SC placeholder (%q) — the `notify-url` tpl extension did not reach this service; check the component's PreProvision landed it for this deployTo", base,
			)
		}
	}
	if token == "" {
		return nil, fmt.Errorf("NOTIFY_URL is set to %s but NOTIFY_TOKEN is empty — every event would be refused with a 401 at the moment it mattered", base)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: publishTimeout}
	}
	log.Info("notifications on", "notifyUrl", base)
	return &Emitter{
		baseURL: base,
		token:   token,
		selfURL: strings.TrimRight(strings.TrimSpace(cfg.SelfURL), "/"),
		http:    httpClient,
		log:     log,
	}, nil
}

func (e *Emitter) Enabled() bool { return true }

// Publish POSTs one event, inline, once.
//
// It does NOT retry. Retry, backoff and give-up all live in the
// notifier's delivery queue, which has durable rows the operator can
// see; a second retry loop here would produce the duplicate-delivery
// class this architecture exists to avoid (forge-notifier sent 20+
// copies of one notification on 2026-09-19 because two layers each
// thought they owned the retry).
//
// The log line before the send is not decoration. A swallowed failure
// on this path is invisible by construction — nobody is watching a
// notification that was never sent — so the attempt is recorded before
// the outcome can be lost.
func (e *Emitter) Publish(ctx context.Context, ev Event) error {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	url := e.baseURL + eventsPath
	e.log.Info("publishing event", "kind", ev.Kind, "severity", ev.Severity, "subject", ev.Subject, "eventId", ev.ID, "url", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("User-Agent", userAgent)

	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("post event to %s: %w", url, err)
	}
	defer resp.Body.Close()
	// Read a bounded prefix: the notifier's error bodies are small
	// JSON, and an unbounded read on a misrouted URL could pull a whole
	// SPA bundle into a log line.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Named separately because the cause is always the same —
		// the publisher's token was not minted by the key this
		// notifier validates with — and the generic message sends
		// people reading network logs instead.
		return fmt.Errorf("notifier refused the event token (HTTP %d): the NOTIFY_TOKEN was not minted by this notifier's signingKey; re-mint it and update the secret: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notifier returned HTTP %d for %s: %s", resp.StatusCode, ev.Kind, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// BackupRunEventID is the idempotency key for an event about a backup
// run.
//
// Keyed on the run and the kind, never on the clock: the scheduler tick
// can re-fire, a Lambda can be retried by its own invoker, and a
// watchdog can revisit a row. Each must produce the same ID so the
// notifier dedupes rather than paging the operator twice about one
// failure.
func BackupRunEventID(runID, kind string) string {
	return "backup-run:" + runID + ":" + kind
}

// RunLink builds the deep link an operator taps from the message.
// Empty when the service does not know its own public URL, which is
// legitimate — a component with no ingress still backs up.
func (e *Emitter) RunLink(path string) string {
	if e.selfURL == "" {
		return ""
	}
	return e.selfURL + path
}
