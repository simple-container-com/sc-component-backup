// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package tenant

import "time"

// Record is one tenant row — one `(consumer-stack, version)` pair.
//
// The plugin's push-register call produces the StackName / Version /
// Resources fields; the service fills in CreatedAt / LastSeenAt.
//
// Real components (Backup, Monitoring, …) would add per-type fields:
// Backup.ScheduleHint, Monitoring.MetricsEndpoint, etc. The backup just
// records the shape.
type Record struct {
	StackName   string     `bson:"stackName" json:"stackName"`
	Version     string     `bson:"version" json:"version"`
	Resources   []Resource `bson:"resources" json:"resources"`
	OrgID       string     `bson:"orgId,omitempty" json:"orgId,omitempty"`
	CreatedAt   time.Time  `bson:"createdAt" json:"createdAt"`
	LastSeenAt  time.Time  `bson:"lastSeenAt" json:"lastSeenAt"`
	SoftDeleted bool       `bson:"softDeleted,omitempty" json:"softDeleted,omitempty"`
}

// Resource describes one sibling resource the consumer has (DB, S3, …).
// For the backup, this is a pass-through — the service doesn't act on it.
// Real components (Backup) would parse the `connection.*` family to
// reach the DB.
type Resource struct {
	Type       string            `bson:"type" json:"type"`
	Name       string            `bson:"name" json:"name"`
	Connection map[string]string `bson:"connection,omitempty" json:"connection,omitempty"`
}
