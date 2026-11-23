// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package scheduler — per-tenant cron tick + run execution.
package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/driver"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/notify"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/storage"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// RunStore persists BackupRun / RestoreRun rows. Separate from tenant
// store because the row lifecycle (append + mutate status) is
// different from tenant upsert.
type RunStore interface {
	PutBackup(ctx context.Context, run backup.BackupRun) error
	UpdateBackup(ctx context.Context, run backup.BackupRun) error
	GetBackup(ctx context.Context, id string) (backup.BackupRun, error)
	ListBackupsByTenant(ctx context.Context, tenantStack string) ([]backup.BackupRun, error)
	// ListAllBackups returns runs across every tenant, newest first.
	// `limit <= 0` means "no cap"; callers that page through the
	// result SHOULD pass a cap so a growing collection doesn't push a
	// Lambda past its memory budget. The SPA's global /history view is
	// the primary caller.
	ListAllBackups(ctx context.Context, limit int) ([]backup.BackupRun, error)
	// DeleteBackupsByTenant physically drops every backup-run row for
	// the given tenant. Returns the count of rows removed. Idempotent
	// — an empty tenant returns 0 with no error. Intended for orphan
	// cleanup after a consumer stack is destroyed (no destroy event
	// reaches the backup plugin bus; see pre_destroy.go:22).
	DeleteBackupsByTenant(ctx context.Context, tenantStack string) (int, error)

	PutRestore(ctx context.Context, run backup.RestoreRun) error
	UpdateRestore(ctx context.Context, run backup.RestoreRun) error
	GetRestore(ctx context.Context, id string) (backup.RestoreRun, error)
}

// Config bundles the scheduler's knobs. Operator-sourced via
// server.yaml → sc-backup resource config; defaults chosen to avoid
// surprising ops behaviour on first boot.
type Config struct {
	PathTemplate      string        // default "{tenant}/{yyyy}/{mm}/{dd}/{resource}-{ts-rfc3339}.archive.gz"
	RetentionDays     int           // default 0 (disabled)
	TickInterval      time.Duration // default 60s; operator override for dev
	BackupTimeout     time.Duration // per-run; default 1h
	ShutdownGraceWait time.Duration // graceful SIGTERM wait; default 5m
}

// FillDefaults fills zero-valued fields with the chosen defaults.
func (c *Config) FillDefaults() {
	if c.PathTemplate == "" {
		c.PathTemplate = "{tenant}/{yyyy}/{mm}/{dd}/{resource}-{ts-rfc3339}.archive.gz"
	}
	if c.TickInterval == 0 {
		c.TickInterval = 60 * time.Second
	}
	if c.BackupTimeout == 0 {
		c.BackupTimeout = time.Hour
	}
	if c.ShutdownGraceWait == 0 {
		c.ShutdownGraceWait = 5 * time.Minute
	}
}

// Scheduler ties tenant store + run store + s3 store + driver registry
// together. RunOnce executes a single backup for one tenant+resource
// pair; the scheduler's loop fires RunOnce on a tick (deferred — v1
// does no cron parsing, operator's `schedule.default` is treated as a
// simple interval string via `ParseInterval`).
type Scheduler struct {
	cfg      Config
	tenants  tenant.Store
	runs     RunStore
	s3       storage.Store
	log      *slog.Logger
	driverBy func(string) (driver.Driver, error)

	// events publishes terminal run outcomes to sc-component-notifier.
	// Never nil — New installs notify.Disabled{} when the operator
	// declared no `notify:` block, so no call site needs a guard.
	events notify.Publisher

	inflightMu sync.Mutex
	inflight   map[string]struct{} // key: tenantStack+"\x00"+resourceName
}

// New constructs a Scheduler. driverFactory defaults to driver.ByName
// if nil; tests swap for a fake.
func New(cfg Config, tenants tenant.Store, runs RunStore, s3 storage.Store, log *slog.Logger, driverFactory func(string) (driver.Driver, error)) *Scheduler {
	cfg.FillDefaults()
	if driverFactory == nil {
		driverFactory = driver.ByName
	}
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		cfg:      cfg,
		tenants:  tenants,
		runs:     runs,
		s3:       s3,
		log:      log,
		driverBy: driverFactory,
		inflight: map[string]struct{}{},
		events:   notify.Disabled{},
	}
}

// SetPublisher installs the notifier publisher.
//
// A setter rather than a seventh positional argument to New: this is
// optional wiring, and the one existing call site should not have to
// pass a sentinel. Tests install a fake to assert what would have been
// published.
func (s *Scheduler) SetPublisher(p notify.Publisher) {
	if p == nil {
		p = notify.Disabled{}
	}
	s.events = p
}

// Start runs the scheduler loop until ctx is cancelled. Graceful
// shutdown: on ctx cancel, wait up to cfg.ShutdownGraceWait for
// in-flight runs; the rest are marked `cancelled` by in-flight
// runners when their own ctx fires.
func (s *Scheduler) Start(ctx context.Context) {
	tick := time.NewTicker(s.cfg.TickInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.waitForInflight(s.cfg.ShutdownGraceWait)
			return
		case <-tick.C:
			// v1: no cron parsing; the scheduler loop surfaces the tick
			// as an event, and operator-triggered backups via the HTTP
			// handler use the same RunOnce path. The schedule-aware
			// enrichment (cron parsing + next-run computation) is a
			// follow-up — not in slice-1-first-component scope.
		}
	}
}

func (s *Scheduler) waitForInflight(max time.Duration) {
	deadline := time.Now().Add(max)
	for {
		s.inflightMu.Lock()
		n := len(s.inflight)
		s.inflightMu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			s.log.Warn("shutdown grace exhausted; leaving runs in flight", "count", n)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// claim reserves a tenant+resource slot. Returns false if a run is
// already in flight for that pair — the caller drops the tick.
func (s *Scheduler) claim(tenantStack, resourceName string) bool {
	key := tenantStack + "\x00" + resourceName
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if _, exists := s.inflight[key]; exists {
		return false
	}
	s.inflight[key] = struct{}{}
	return true
}

func (s *Scheduler) release(tenantStack, resourceName string) {
	key := tenantStack + "\x00" + resourceName
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	delete(s.inflight, key)
}

// RunOnce executes one backup of one tenant+resource. Returns the
// created BackupRun row (persisted). The caller (HTTP handler or tick
// loop) is responsible for choosing WHICH resource of the tenant to
// back up — default policy is "first resource whose driver is
// supported."
func (s *Scheduler) RunOnce(ctx context.Context, tenantStack, resourceName, triggerSource string) (backup.BackupRun, error) {
	t, found, err := s.tenants.Get(ctx, tenantStack)
	if err != nil {
		return backup.BackupRun{}, fmt.Errorf("tenant %q lookup: %w", tenantStack, err)
	}
	if !found {
		return backup.BackupRun{}, fmt.Errorf("tenant %q not found", tenantStack)
	}
	resource, ok := findResource(t.Resources, resourceName)
	if !ok {
		return backup.BackupRun{}, fmt.Errorf("tenant %q has no resource named %q", tenantStack, resourceName)
	}
	driverName := strings.TrimSpace(resource.Connection["driver"])
	if driverName == "" {
		return backup.BackupRun{}, fmt.Errorf("tenant %q resource %q: connection.driver is empty", tenantStack, resourceName)
	}
	d, err := s.driverBy(driverName)
	if err != nil {
		return backup.BackupRun{}, err
	}

	if !s.claim(tenantStack, resourceName) {
		return backup.BackupRun{}, fmt.Errorf("tenant %q resource %q: a backup is already in flight", tenantStack, resourceName)
	}
	defer s.release(tenantStack, resourceName)

	runID := uuid.NewString()
	now := time.Now().UTC()
	run := backup.BackupRun{
		ID:            runID,
		TenantStack:   tenantStack,
		ResourceName:  resourceName,
		Driver:        d.Name(),
		StartedAt:     now,
		Status:        backup.StatusRunning,
		TriggerSource: triggerSource,
	}
	if err := s.runs.PutBackup(ctx, run); err != nil {
		return backup.BackupRun{}, fmt.Errorf("persist backup-run: %w", err)
	}

	conn := specFromConnection(resource.Connection)
	s3Path := storage.RenderPath(s.cfg.PathTemplate, tenantStack, resourceName, runID, now)

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.BackupTimeout)
	defer cancel()

	// Buffer-then-upload. The streaming io.Pipe shape (mongodump stdout
	// → io.Pipe → s3 PutObject body) deadlocked under aws-sdk-go-v2's
	// chunked-upload path against seaweedfs's S3 gateway in the first
	// smoke: s3's reader never drained the pipe, so exec.Cmd's stdout
	// copier blocked on write, so mongodump never exited. Buffering the
	// full dump into memory before the Put is the safe-by-construction
	// shape for v1; multi-GB dumps will need either a seekable file
	// backing or a streaming-aware SDK call, both follow-ups.
	var buf bytes.Buffer
	dumpErr := d.Dump(runCtx, conn, &buf)
	var sizeBytes int64
	var putErr error
	if dumpErr == nil {
		sizeBytes, putErr = s.s3.Put(runCtx, s3Path, bytes.NewReader(buf.Bytes()))
	}

	run.FinishedAt = time.Now().UTC()
	switch {
	case runCtx.Err() == context.Canceled:
		run.Status = backup.StatusCancelled
	case dumpErr != nil:
		run.Status = backup.StatusFailed
		run.Error = truncate(dumpErr.Error(), 512)
	case putErr != nil:
		run.Status = backup.StatusFailed
		run.Error = "s3: " + truncate(putErr.Error(), 512)
	default:
		run.Status = backup.StatusSucceeded
		run.S3Path = s3Path
		run.SizeBytes = sizeBytes
		run.RetentionExpiresAt = backup.ComputeRetentionExpiresAt(run.FinishedAt, s.cfg.RetentionDays)
	}
	if err := s.runs.UpdateBackup(ctx, run); err != nil {
		s.log.Error("persist final backup-run state", "err", err, "runId", runID)
	}
	s.publishBackupOutcome(ctx, run)
	return run, nil
}

// publishBackupOutcome emits the terminal status to the notifier.
//
// This is the hook, and the terminal status write is where it belongs:
// the outcome is known exactly once here, which makes the event an EDGE
// by construction. The alerts.Synthesizer is NOT the hook — it is
// level-triggered and only runs on a SPA GET, so it can describe a
// failure but can never be the thing that announces one.
//
// Errors are logged, never returned. A backup that succeeded must not
// be reported as failed because Telegram was slow, and a backup that
// failed has already failed — turning a notification error into a
// second failure would only obscure the first. The log line is the
// operator's trail, and the notifier's own delivery rows are the
// durable record of what happened after this POST.
func (s *Scheduler) publishBackupOutcome(ctx context.Context, run backup.BackupRun) {
	if !s.events.Enabled() {
		return
	}

	var kind, severity, title string
	switch run.Status {
	case backup.StatusFailed:
		kind, severity = notify.KindBackupFailed, notify.SeverityError
		title = fmt.Sprintf("Backup failed: %s/%s", run.TenantStack, run.ResourceName)
	case backup.StatusCancelled:
		// A cancellation is usually deliberate (an operator pressed
		// cancel, or the Lambda hit its deadline), so it is a warning
		// rather than an error — but it is still actionable, which is
		// why it is not info. See the severity note in notify/events.go.
		kind, severity = notify.KindBackupCancelled, notify.SeverityWarning
		title = fmt.Sprintf("Backup cancelled: %s/%s", run.TenantStack, run.ResourceName)
	case backup.StatusSucceeded:
		// Emitted at info, which every channel's default minSeverity
		// (warning) drops. That is intended: an operator who wants a
		// green heartbeat lowers one channel's minimum, and everyone
		// else is not paged about success.
		kind, severity = notify.KindBackupSucceeded, notify.SeverityInfo
		title = fmt.Sprintf("Backup succeeded: %s/%s", run.TenantStack, run.ResourceName)
	default:
		// Not a terminal status — nothing to announce.
		return
	}

	ev := notify.Event{
		ID:       notify.BackupRunEventID(run.ID, kind),
		Source:   run.TenantStack,
		Kind:     kind,
		Severity: severity,
		Subject:  run.TenantStack + "/" + run.ResourceName,
		Title:    title,
		Body:     backupEventBody(run),
		URL:      s.events.RunLink("/#/runs/" + run.ID),
		At:       run.FinishedAt,
		Labels: map[string]string{
			"tenant":   run.TenantStack,
			"resource": run.ResourceName,
			"driver":   run.Driver,
			"runId":    run.ID,
		},
	}
	if err := s.events.Publish(ctx, ev); err != nil {
		s.log.Error("publish backup outcome", "err", err, "runId", run.ID, "kind", kind)
	}
}

// backupEventBody renders the human-readable half of the message.
func backupEventBody(run backup.BackupRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "driver: %s\n", run.Driver)
	if !run.StartedAt.IsZero() && !run.FinishedAt.IsZero() {
		fmt.Fprintf(&b, "duration: %s\n", run.FinishedAt.Sub(run.StartedAt).Round(time.Second))
	}
	if run.TriggerSource != "" {
		fmt.Fprintf(&b, "trigger: %s\n", run.TriggerSource)
	}
	if run.SizeBytes > 0 {
		fmt.Fprintf(&b, "size: %d bytes\n", run.SizeBytes)
	}
	if run.Error != "" {
		// Already truncated to 512 at the status write; repeated here
		// because the operator reading a Telegram message has no other
		// view of it.
		fmt.Fprintf(&b, "error: %s\n", run.Error)
	}
	return b.String()
}

// RunRestore streams a stored archive through the driver's Restore
// back into the originating DB. Executes synchronously relative to
// the goroutine that calls it; the HTTP handler calls this in a
// background goroutine + returns a RestoreRun id immediately.
func (s *Scheduler) RunRestore(ctx context.Context, runID string, triggeredBy string) (backup.RestoreRun, error) {
	backupRun, err := s.runs.GetBackup(ctx, runID)
	if err != nil {
		return backup.RestoreRun{}, fmt.Errorf("backup-run %q: %w", runID, err)
	}
	if backupRun.Status != backup.StatusSucceeded {
		return backup.RestoreRun{}, fmt.Errorf("backup-run %q status %q; can only restore from a succeeded run", runID, backupRun.Status)
	}

	t, found, err := s.tenants.Get(ctx, backupRun.TenantStack)
	if err != nil {
		return backup.RestoreRun{}, fmt.Errorf("tenant %q: %w", backupRun.TenantStack, err)
	}
	if !found {
		return backup.RestoreRun{}, fmt.Errorf("tenant %q: not found at restore time", backupRun.TenantStack)
	}
	resource, ok := findResource(t.Resources, backupRun.ResourceName)
	if !ok {
		return backup.RestoreRun{}, fmt.Errorf("tenant %q resource %q: not found at restore time", backupRun.TenantStack, backupRun.ResourceName)
	}
	d, err := s.driverBy(backupRun.Driver)
	if err != nil {
		return backup.RestoreRun{}, err
	}

	restore := backup.RestoreRun{
		ID:           uuid.NewString(),
		BackupRunID:  runID,
		TenantStack:  backupRun.TenantStack,
		ResourceName: backupRun.ResourceName,
		Driver:       backupRun.Driver,
		StartedAt:    time.Now().UTC(),
		Status:       backup.StatusRunning,
		TriggeredBy:  triggeredBy,
	}
	if err := s.runs.PutRestore(ctx, restore); err != nil {
		return backup.RestoreRun{}, fmt.Errorf("persist restore-run: %w", err)
	}

	body, err := s.s3.Get(ctx, backupRun.S3Path)
	if err != nil {
		restore.Status = backup.StatusFailed
		restore.Error = truncate(err.Error(), 512)
		restore.FinishedAt = time.Now().UTC()
		_ = s.runs.UpdateRestore(ctx, restore)
		return restore, err
	}
	defer body.Close()

	conn := specFromConnection(resource.Connection)
	if err := d.Restore(ctx, conn, body); err != nil {
		restore.Status = backup.StatusFailed
		restore.Error = truncate(err.Error(), 512)
	} else {
		restore.Status = backup.StatusSucceeded
	}
	restore.FinishedAt = time.Now().UTC()
	if err := s.runs.UpdateRestore(ctx, restore); err != nil {
		s.log.Error("persist final restore-run state", "err", err, "runId", restore.ID)
	}
	s.publishRestoreOutcome(ctx, restore)
	return restore, nil
}

// publishRestoreOutcome emits a FAILED restore to the notifier.
//
// Only the failure. A restore is always operator-initiated — somebody
// is watching the SPA while it runs — so a success notification is
// noise, while a failure means the data is not where they just asked it
// to be.
func (s *Scheduler) publishRestoreOutcome(ctx context.Context, restore backup.RestoreRun) {
	if !s.events.Enabled() || restore.Status != backup.StatusFailed {
		return
	}
	ev := notify.Event{
		ID:       notify.BackupRunEventID(restore.ID, notify.KindRestoreFailed),
		Source:   restore.TenantStack,
		Kind:     notify.KindRestoreFailed,
		Severity: notify.SeverityError,
		Subject:  restore.TenantStack + "/" + restore.ResourceName,
		Title:    fmt.Sprintf("Restore failed: %s/%s", restore.TenantStack, restore.ResourceName),
		Body:     fmt.Sprintf("from backup-run: %s\nerror: %s\n", restore.BackupRunID, restore.Error),
		URL:      s.events.RunLink("/#/restores/" + restore.ID),
		At:       restore.FinishedAt,
		Labels: map[string]string{
			"tenant":      restore.TenantStack,
			"resource":    restore.ResourceName,
			"restoreId":   restore.ID,
			"backupRunId": restore.BackupRunID,
		},
	}
	if err := s.events.Publish(ctx, ev); err != nil {
		s.log.Error("publish restore outcome", "err", err, "restoreId", restore.ID, "kind", notify.KindRestoreFailed)
	}
}

func findResource(resources []tenant.Resource, name string) (tenant.Resource, bool) {
	for _, r := range resources {
		if r.Name == name {
			return r, true
		}
	}
	return tenant.Resource{}, false
}

// specFromConnection reads the canonical `connection.*` fields out of
// a Resource.Connection map. Mirrors the canonical credential output
// spec in sc-components-platform §P3.
func specFromConnection(conn map[string]string) driver.ConnectionSpec {
	port := 0
	if p, ok := conn["port"]; ok {
		_, _ = fmt.Sscanf(p, "%d", &port)
	}
	return driver.ConnectionSpec{
		URI:      conn["uri"],
		Host:     conn["host"],
		Port:     port,
		User:     conn["user"],
		Password: conn["password"],
		Database: conn["database"],
		TLSCAPem: conn["tls_ca_pem"],
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
