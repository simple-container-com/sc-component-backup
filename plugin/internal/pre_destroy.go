// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"context"
	"fmt"

	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// PreDestroy is the backup's stub pre-destroy hook. Fire-and-forget.
//
// Fires when the plugin's OWN resource (`my-backup`) is being destroyed —
// NOT when consumers that `uses: [my-backup]` are being destroyed. The
// plugin bus has no proto-level "consumer removed" event; stale tenant
// rows are reaped via the service's own last-seen TTL GC (see
// sc-components-platform design doc §P3).
//
// A real component's PreDestroy stops the cron scheduler, flushes pending
// backups to the sibling S3, drains in-flight restores. The backup has
// nothing to clean up; log and return.
func PreDestroy(_ context.Context, publisher *scPlugin.Publisher, req *scpluginv1.PreDestroyRequest) error {
	publisher.Info("pre-destroy", fmt.Sprintf("backup component resource_type=%q stack=%q env=%q",
		req.GetResourceType(), req.GetHeader().GetStackName(), req.GetHeader().GetEnvironment()))

	outputs := req.GetStackOutputs()
	publisher.Info("pre-destroy", fmt.Sprintf("stack outputs: %d key(s) — nothing to drain for a backup component", len(outputs)))

	return nil
}
