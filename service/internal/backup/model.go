// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package backup — core shapes for the Backup component's business
// logic. The tenant row is defined in sibling package `tenant`; a
// BackupRun is one scheduled or operator-triggered dump of one
// tenant's DB-flavoured dependency.
package backup

import "time"

// Status of one BackupRun or RestoreRun. Narrow so the scheduler can
// write a tight switch and the SPA can render each as a chip.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// BackupRun is one scheduled or operator-triggered backup of one
// tenant's DB. Persisted; retention sweep deletes rows whose
// `RetentionExpiresAt` has passed AND whose S3 object has been
// removed (two-phase so a crash between phases is resumable).
type BackupRun struct {
	ID                 string    `bson:"_id" json:"id"`
	TenantStack        string    `bson:"tenantStack" json:"tenantStack"`
	ResourceName       string    `bson:"resourceName" json:"resourceName"`
	Driver             string    `bson:"driver" json:"driver"` // "mongodb" / "postgres" / "mysql"
	StartedAt          time.Time `bson:"startedAt" json:"startedAt"`
	FinishedAt         time.Time `bson:"finishedAt,omitempty" json:"finishedAt,omitempty"`
	Status             Status    `bson:"status" json:"status"`
	S3Path             string    `bson:"s3Path,omitempty" json:"s3Path,omitempty"`
	SizeBytes          int64     `bson:"sizeBytes,omitempty" json:"sizeBytes,omitempty"`
	Error              string    `bson:"error,omitempty" json:"error,omitempty"`
	RetentionExpiresAt time.Time `bson:"retentionExpiresAt,omitempty" json:"retentionExpiresAt,omitempty"`
	TriggerSource      string    `bson:"triggerSource" json:"triggerSource"` // "schedule" / "operator"
}

// RestoreRun is one operator-triggered restore of a BackupRun's
// archive back into the originating DB. Persisted; operator polls
// via GET /api/v1/restores/{id}.
type RestoreRun struct {
	ID           string    `bson:"_id" json:"id"`
	BackupRunID  string    `bson:"backupRunId" json:"backupRunId"`
	TenantStack  string    `bson:"tenantStack" json:"tenantStack"`
	ResourceName string    `bson:"resourceName" json:"resourceName"`
	Driver       string    `bson:"driver" json:"driver"`
	StartedAt    time.Time `bson:"startedAt" json:"startedAt"`
	FinishedAt   time.Time `bson:"finishedAt,omitempty" json:"finishedAt,omitempty"`
	Status       Status    `bson:"status" json:"status"`
	Error        string    `bson:"error,omitempty" json:"error,omitempty"`
	TriggeredBy  string    `bson:"triggeredBy,omitempty" json:"triggeredBy,omitempty"`
}

// ComputeRetentionExpiresAt returns the absolute expiry time for a
// run that completed at `finishedAt` under a `retentionDays` policy.
// Zero `retentionDays` disables retention (zero return → never expires).
func ComputeRetentionExpiresAt(finishedAt time.Time, retentionDays int) time.Time {
	if retentionDays <= 0 {
		return time.Time{}
	}
	return finishedAt.Add(time.Duration(retentionDays) * 24 * time.Hour)
}
