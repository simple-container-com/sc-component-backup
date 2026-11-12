// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"context"
	"fmt"
	"strings"

	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// PreDestroy is the backup's pre-destroy hook.
//
// Fires when the plugin's OWN resource (`my-backup`) is being destroyed —
// NOT when consumers that `uses: [my-backup]` are being destroyed. The
// plugin bus has no proto-level "consumer removed" event; per-consumer
// stale scoped users are handled either by the service's last-seen TTL
// GC (reaps tenant rows after 7 days) or by an explicit operator-side
// cleanup pass on this resource's teardown.
//
// Slice 1d scoped-user cleanup (sc-components-platform design doc §5.2):
// this hook best-effort enumerates scoped Atlas users this component
// minted (the service's tenant list, resources where connection.driver
// == "mongodb") and calls Atlas Delete on each. Errors are LOGGED,
// NEVER returned — a Lambda destroy may race the Atlas calls, and the
// teardown should never block on a credential cleanup. Stale users
// remaining is operationally benign (they only have readWrite on the
// consumer's DB, which is also being torn down).
func PreDestroy(ctx context.Context, publisher *scPlugin.Publisher, req *scpluginv1.PreDestroyRequest) error {
	publisher.Info("pre-destroy", fmt.Sprintf("backup component resource_type=%q stack=%q env=%q",
		req.GetResourceType(), req.GetHeader().GetStackName(), req.GetHeader().GetEnvironment()))

	outputs := req.GetStackOutputs()
	publisher.Info("pre-destroy", fmt.Sprintf("stack outputs: %d key(s)", len(outputs)))

	// Attempt to reach the service's tenant list + delete each scoped
	// Atlas user. Short-circuit if we don't have what we need.
	cleanupScopedUsers(ctx, publisher, req)

	return nil
}

// cleanupScopedUsers is the best-effort PreDestroy helper. All errors
// log + swallow — the plugin destroy continues regardless.
func cleanupScopedUsers(_ context.Context, publisher *scPlugin.Publisher, req *scpluginv1.PreDestroyRequest) {
	// PreDestroyRequest doesn't carry resource config today — scoped-user
	// cleanup needs admin creds that are only visible at Provision /
	// ComputeProcess time. Follow-up: SC plugin bus to carry a resource
	// config JSON on PreDestroy too.
	var cfg map[string]any
	_, admin, err := populateDatabasesFromRaw(cfg)
	if err != nil || admin == nil {
		publisher.Info("pre-destroy", fmt.Sprintf("no atlasAdmin on config (err=%v) — skipping cleanup", err))
		return
	}
	// Walk the stack outputs for tenant markers emitted by past
	// ComputeProcess runs. Pre-slice-1a-wired, we don't yet have a
	// durable list of minted usernames in stack outputs; this hook is
	// prepared for that signal and no-ops today. A future enhancement
	// adds a `my-backup-scoped-users` export on each ComputeProcess →
	// this hook reads it and calls DeleteScopedMongoUser.
	if names := collectScopedUsernames(req.GetStackOutputs()); len(names) > 0 {
		for _, username := range names {
			if err := DeleteScopedMongoUser(context.Background(), *admin, username); err != nil {
				publisher.Warn("pre-destroy", fmt.Sprintf("delete scoped user %q: %v — swallowed", username, err))
				continue
			}
			publisher.Info("pre-destroy", fmt.Sprintf("deleted scoped Atlas user %q", username))
		}
	} else {
		publisher.Info("pre-destroy", "no scoped-user markers in stack outputs — nothing to clean up")
	}
}

// collectScopedUsernames reads stack outputs for `backup-*` scoped-user
// markers. Expected shape: outputs["my-backup-scoped-users"] = "name1,name2,...".
// StackOutputs carries bytes values (JSON-serialised Pulumi outputs);
// strings arrive wrapped in quotes.
func collectScopedUsernames(outputs map[string][]byte) []string {
	// Look through any key whose suffix is `-scoped-users` (SC core's
	// output key composition varies across templates).
	var all []string
	for k, v := range outputs {
		if !strings.HasSuffix(k, "-scoped-users") || len(v) == 0 {
			continue
		}
		// Pulumi outputs are JSON-serialised; strip quotes if present.
		s := strings.TrimSpace(string(v))
		s = strings.Trim(s, `"`)
		for _, name := range strings.Split(s, ",") {
			name = strings.TrimSpace(name)
			if strings.HasPrefix(name, "backup-") {
				all = append(all, name)
			}
		}
	}
	return all
}
