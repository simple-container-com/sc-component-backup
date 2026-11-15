// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// PreDestroy is the backup's pre-destroy hook.
//
// Fires when the plugin's OWN resource (`my-backup`) is being destroyed —
// NOT when consumers that `uses: [my-backup]` are being destroyed. The
// plugin bus has no proto-level "consumer removed" event, so by the time
// this hook runs the consumer stacks are already destroyed. What this
// hook CAN do is reach the backup service (whose tenant.Store still
// carries records for every consumer that ever registered), enumerate
// scoped Atlas users this component minted (`backup-<consumer-stack>`),
// and call Atlas REST DELETE on each.
//
// Slice 1d.5 scoped-user cleanup. Operator-visible symptom pre-slice-1d.5:
// every smoke left behind an Atlas user (visible in the Atlas UI). Not a
// security issue — the scoped user only has readWrite on a destroyed DB,
// which is also being torn down — but visual clutter + per-project user
// cap drift over time.
//
// Errors are LOGGED, NEVER returned. A Lambda destroy may race the Atlas
// calls, and the teardown should never block on a credential cleanup.
func PreDestroy(ctx context.Context, publisher *scPlugin.Publisher, req *scpluginv1.PreDestroyRequest, peerDialer func(context.Context) (*scPlugin.PeerClient, error)) error {
	publisher.Info("pre-destroy", fmt.Sprintf("backup component resource_type=%q stack=%q env=%q",
		req.GetResourceType(), req.GetHeader().GetStackName(), req.GetHeader().GetEnvironment()))

	outputs := req.GetStackOutputs()
	publisher.Info("pre-destroy", fmt.Sprintf("stack outputs: %d key(s)", len(outputs)))

	cleanupScopedUsers(ctx, publisher, req, peerDialer)

	return nil
}

// cleanupScopedUsers is the best-effort PreDestroy helper. All errors
// log + swallow — the plugin destroy continues regardless.
//
// Inputs come from StackOutputs (what Provision exported on this
// component's state). Required keys per component prefix:
//   - `<res>-signing-key`  — minted a short-TTL JWT for the service call.
//   - `<res>-service-url`  — backup service HTTPS endpoint. Falls back to
//     any `*-function-url` key when the primary value is the pre-slice-1a
//     placeholder `unset-pre-slice-1a` (matches `readServiceURL`'s cascade).
//
// Scoped-user delete flows via peer-invoke to the database-owning plugin
// (sc-plugin-mongodb for mongodb, etc.) — NO driver-specific knowledge
// in this plugin. Each tenant.Record.Resources entry carries its driver
// + cluster name; PreDestroy routes delete calls by driver.
//
// LIMITATION: the database-owning plugin's cred-cache is populated at
// ComputeProcess time (consumer deploys), not at PreDestroy time
// (parent-stack destroys). So isolated-backup-destroy (operator removes
// `my-backup` while mongodb cluster stays) will hit cache-miss errors
// from the owning plugin's handler, orphaning scoped users. The cleanup
// loop logs + swallows those errors. Parent-stack destroy is fine —
// mongodb cluster is destroyed concurrently, carrying its users with it.
//
// Full fix tracked under
// sc-components-plugin-peer-invoke.md's "Follow-ups" section:
// mongodb plugin exports its Atlas creds to parent state; its
// PluginInvoke handler reads via StackReference on cache-miss.
func cleanupScopedUsers(ctx context.Context, publisher *scPlugin.Publisher, req *scpluginv1.PreDestroyRequest, peerDialer func(context.Context) (*scPlugin.PeerClient, error)) {
	outputs := req.GetStackOutputs()

	// PreDestroyRequest has no resource_name field so the per-component
	// prefix is discovered from stack-output signing-key keys: any
	// `<prefix>-signing-key` is a backup component Provision exported.
	prefixes := discoverComponentPrefixes(outputs)
	if len(prefixes) == 0 {
		publisher.Info("pre-destroy", "scoped-user cleanup skipped: no `-signing-key` export in stack outputs (expected on non-slice-1d stacks)")
		return
	}
	if peerDialer == nil {
		publisher.Info("pre-destroy", "scoped-user cleanup skipped: PeerDialer not wired on this plugin binary")
		return
	}

	for _, resName := range prefixes {
		publisher.Info("pre-destroy", fmt.Sprintf("scoped-user cleanup for component %q", resName))
		cleanupOneComponent(ctx, publisher, resName, outputs, peerDialer)
	}
}

// discoverComponentPrefixes returns every unique `<prefix>` where
// `<prefix>-signing-key` appears in StackOutputs — one per backup
// component in the stack. Signing-key is always exported by Provision
// (slice 1d.5) regardless of config, so it's a reliable discriminator.
func discoverComponentPrefixes(outputs map[string][]byte) []string {
	const suffix = "-signing-key"
	var out []string
	for k := range outputs {
		if strings.HasSuffix(k, suffix) {
			out = append(out, strings.TrimSuffix(k, suffix))
		}
	}
	return out
}

// cleanupOneComponent runs the peer-invoke scoped-user purge for one
// `<prefix>`. Enumerates tenants via the service, then for each tenant
// resource issues a peer.PluginInvoke(<owning-plugin>, "delete-scoped-user",
// {cluster, username}). All failures swallow; orphan users are a
// documented limitation (see cleanupScopedUsers header).
func cleanupOneComponent(ctx context.Context, publisher *scPlugin.Publisher, resName string, outputs map[string][]byte, peerDialer func(context.Context) (*scPlugin.PeerClient, error)) {
	signingKey := readStackOutputString(outputs, resName+"-signing-key")
	serviceURL := readStackOutputString(outputs, resName+"-service-url")

	if isUnsetServiceURL(serviceURL) {
		serviceURL = scanForFunctionURL(outputs)
	}

	if signingKey == "" || isUnsetServiceURL(serviceURL) {
		publisher.Info("pre-destroy", fmt.Sprintf("component %q cleanup skipped: missing signingKey/serviceURL", resName))
		return
	}

	token, err := mintJWT(signingKey, "pre-destroy-cleanup", jwtLifetime)
	if err != nil {
		publisher.Warn("pre-destroy", fmt.Sprintf("component %q: mint JWT: %v — swallowed", resName, err))
		return
	}

	tenants, err := fetchTenantsList(ctx, serviceURL, token)
	if err != nil {
		publisher.Warn("pre-destroy", fmt.Sprintf("component %q: fetch tenants from %s: %v — swallowed", resName, serviceURL, err))
		return
	}

	peer, err := peerDialer(ctx)
	if err != nil {
		publisher.Warn("pre-destroy", fmt.Sprintf("component %q: peer-dial SCCore: %v — swallowed", resName, err))
		return
	}
	defer peer.Close()

	deleted := 0
	for _, t := range tenants {
		for _, r := range t.Resources {
			driver := r.Connection["driver"]
			cluster := r.Connection["cluster"]
			if _, known := PluginForDriver(driver); !known {
				continue
			}
			username := extractUsernameFromMongoURI(r.Connection["uri"])
			if !strings.HasPrefix(username, "backup-") && !strings.HasPrefix(username, "sc-component-backup-") && !strings.HasPrefix(username, "peer-") {
				// Not one we minted (legacy stacks may have different
				// prefixes); skip.
				continue
			}
			if cluster == "" {
				// Tenants registered pre-Tier-4 don't carry cluster
				// on their connection map; we have no way to route
				// the delete peer-invoke. Log + skip.
				publisher.Warn("pre-destroy", fmt.Sprintf("component %q: tenant %q has no cluster recorded; skipping delete of %q", resName, t.StackName, username))
				continue
			}
			if dErr := DeleteScopedUser(ctx, peer, driver, cluster, username); dErr != nil {
				publisher.Warn("pre-destroy", fmt.Sprintf("component %q: peer-invoke delete %q on %s cluster %q: %v — swallowed (owning plugin's cred-cache likely empty at parent-destroy; orphan user remains for isolated-destroy case — see sc-components-plugin-peer-invoke follow-ups)", resName, username, driver, cluster, dErr))
				continue
			}
			publisher.Info("pre-destroy", fmt.Sprintf("component %q: deleted scoped %s user %q via peer-invoke", resName, driver, username))
			deleted++
		}
	}
	publisher.Info("pre-destroy", fmt.Sprintf("component %q cleanup done: %d deleted out of %d tenant(s)", resName, deleted, len(tenants)))
}

// readStackOutputString parses a Pulumi auto.OutputValue JSON blob
// (`{"value":..., "secret": bool}`) for a single key + returns its
// string value. Missing keys or non-string values return "".
func readStackOutputString(outputs map[string][]byte, key string) string {
	raw, ok := outputs[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var wire struct {
		Value  any  `json:"value"`
		Secret bool `json:"secret"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		// Older cores may serialise as a plain JSON-quoted string.
		s := strings.TrimSpace(string(raw))
		return strings.Trim(s, `"`)
	}
	s, ok := wire.Value.(string)
	if !ok {
		return ""
	}
	return s
}

// scanForFunctionURL walks stack outputs for the aws-lambda template's
// own `*-function-url` export. Used as the fallback when the backup
// plugin's own `-service-url` export is the pre-slice-1a placeholder
// (same cascade as `readServiceURL`).
func scanForFunctionURL(outputs map[string][]byte) string {
	for k := range outputs {
		if !strings.HasSuffix(k, "-function-url") {
			continue
		}
		if s := readStackOutputString(outputs, k); !isUnsetServiceURL(s) {
			return s
		}
	}
	return ""
}

// extractUsernameFromMongoURI reads the `user` portion of a mongodb URI
// `mongodb[+srv]://<user>:<pass>@<host>/...`. Returns "" on any URI
// without an `@` or without the expected scheme.
func extractUsernameFromMongoURI(uri string) string {
	for _, prefix := range []string{"mongodb+srv://", "mongodb://"} {
		if !strings.HasPrefix(uri, prefix) {
			continue
		}
		rest := strings.TrimPrefix(uri, prefix)
		at := strings.Index(rest, "@")
		if at <= 0 {
			return ""
		}
		userPass := rest[:at]
		colon := strings.Index(userPass, ":")
		if colon <= 0 {
			return userPass
		}
		return userPass[:colon]
	}
	return ""
}

// fetchTenantsList issues a single GET /api/v1/tenants against the
// backup service, authed with a bearer JWT. Returns the list or an
// error naming the request shape (so the PreDestroy log says what
// happened rather than hiding the status code behind a generic "fetch
// failed").
type listTenantsResp struct {
	Count   int            `json:"count"`
	Tenants []TenantRecord `json:"tenants"`
}

func fetchTenantsList(ctx context.Context, serviceURL, token string) ([]TenantRecord, error) {
	target := strings.TrimRight(serviceURL, "/") + "/api/v1/tenants"
	// 60s budget: the Lambda service may be stone-cold after a day
	// idle, and `sc destroy` is the first HTTPS hit that warms it.
	// Observed cold-start + TLS + request round-trip: 10-25s. The
	// earlier 15s timeout tripped on the first 1d.5 validation smoke
	// (2026-11-14 11:17:20 — context deadline exceeded) and skipped the
	// scoped-user cleanup entirely. Bigger budget trades a slower
	// teardown for correctness.
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.Errorf("http %d from GET %s: %s", resp.StatusCode, target, truncate(string(body), 200))
	}
	var out listTenantsResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.Wrap(err, "decode tenants list")
	}
	return out.Tenants, nil
}
