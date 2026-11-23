// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/notify"
)

// fakePublisher records what would have been published, so the mapping
// from terminal status to kind+severity can be asserted without an HTTP
// server. `err` makes Publish fail, which must never change the run's
// outcome.
type fakePublisher struct {
	enabled bool
	err     error
	sent    []notify.Event
	linked  []string
}

func (f *fakePublisher) Publish(_ context.Context, ev notify.Event) error {
	f.sent = append(f.sent, ev)
	return f.err
}

func (f *fakePublisher) Enabled() bool { return f.enabled }

func (f *fakePublisher) RunLink(path string) string {
	f.linked = append(f.linked, path)
	return "https://backup.example.com" + path
}

func schedulerWithPublisher(p notify.Publisher) *Scheduler {
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetPublisher(p)
	return s
}

func finishedRun(status backup.Status) backup.BackupRun {
	start := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	return backup.BackupRun{
		ID:            "run-1",
		TenantStack:   "forge-baas",
		ResourceName:  "mongo",
		Driver:        "mongodb",
		StartedAt:     start,
		FinishedAt:    start.Add(90 * time.Second),
		Status:        status,
		TriggerSource: "schedule",
		SizeBytes:     4096,
	}
}

// The severity mapping, which is the thing most likely to be quietly
// wrong. `warning` is every channel's default minimum, so a status
// mapped one notch too low is a notification nobody ever receives —
// in forge-notifier every Telegram subscription was `severityAtLeast:
// warning` and every `info` notification silently vanished for months.
func TestPublishBackupOutcomeSeverityMapping(t *testing.T) {
	for _, tc := range []struct {
		status        backup.Status
		kind          string
		severity      string
		titlePrefix   string
		wantPublished bool
	}{
		{backup.StatusFailed, notify.KindBackupFailed, notify.SeverityError, "Backup failed", true},
		{backup.StatusCancelled, notify.KindBackupCancelled, notify.SeverityWarning, "Backup cancelled", true},
		{backup.StatusSucceeded, notify.KindBackupSucceeded, notify.SeverityInfo, "Backup succeeded", true},
		// Non-terminal: there is no outcome to announce yet. Emitting
		// here would page the operator at the START of every backup.
		{backup.StatusRunning, "", "", "", false},
		{backup.StatusPending, "", "", "", false},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			fake := &fakePublisher{enabled: true}
			schedulerWithPublisher(fake).publishBackupOutcome(context.Background(), finishedRun(tc.status))

			if !tc.wantPublished {
				if len(fake.sent) != 0 {
					t.Fatalf("status %q must publish nothing, sent %+v", tc.status, fake.sent)
				}
				return
			}
			if len(fake.sent) != 1 {
				t.Fatalf("want exactly 1 event, got %d", len(fake.sent))
			}
			ev := fake.sent[0]
			if ev.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", ev.Kind, tc.kind)
			}
			if ev.Severity != tc.severity {
				t.Errorf("Severity = %q, want %q", ev.Severity, tc.severity)
			}
			if !strings.HasPrefix(ev.Title, tc.titlePrefix) {
				t.Errorf("Title = %q, want prefix %q", ev.Title, tc.titlePrefix)
			}
			if ev.Subject != "forge-baas/mongo" {
				t.Errorf("Subject = %q", ev.Subject)
			}
			if ev.ID != notify.BackupRunEventID("run-1", tc.kind) {
				t.Errorf("ID = %q — must be the run-derived idempotency key", ev.ID)
			}
			if !ev.At.Equal(finishedRun(tc.status).FinishedAt) {
				t.Errorf("At = %v, want the run's FinishedAt", ev.At)
			}
			if ev.URL != "https://backup.example.com/#/runs/run-1" {
				t.Errorf("URL = %q — the operator taps this from the message", ev.URL)
			}
			if ev.Labels["runId"] != "run-1" || ev.Labels["tenant"] != "forge-baas" {
				t.Errorf("Labels = %v", ev.Labels)
			}
		})
	}
}

// The body is what the operator actually reads in Telegram; they have
// no other view of the error text from there.
func TestBackupEventBodyCarriesTheDiagnosis(t *testing.T) {
	run := finishedRun(backup.StatusFailed)
	run.Error = "mongodump: connection refused"
	body := backupEventBody(run)

	for _, want := range []string{"driver: mongodb", "duration: 1m30s", "trigger: schedule", "size: 4096", "connection refused"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not mention %q:\n%s", want, body)
		}
	}
}

// A publish failure must not touch the run's outcome. A backup that
// succeeded cannot be reported as failed because Telegram was slow, and
// a backup that failed has already failed — turning a notification
// error into a second failure would only obscure the first.
func TestPublishErrorDoesNotAffectTheRun(t *testing.T) {
	fake := &fakePublisher{enabled: true, err: errors.New("notifier down")}
	run := finishedRun(backup.StatusSucceeded)

	schedulerWithPublisher(fake).publishBackupOutcome(context.Background(), run)

	if run.Status != backup.StatusSucceeded {
		t.Errorf("run status changed to %q", run.Status)
	}
	if len(fake.sent) != 1 {
		t.Errorf("the attempt must still have been made, sent %d", len(fake.sent))
	}
}

// With no notifier configured the hook is a no-op, and must not even
// build an event — RunLink included, since that is observable.
func TestDisabledPublisherIsNotCalled(t *testing.T) {
	fake := &fakePublisher{enabled: false}
	s := schedulerWithPublisher(fake)

	s.publishBackupOutcome(context.Background(), finishedRun(backup.StatusFailed))
	s.publishRestoreOutcome(context.Background(), backup.RestoreRun{ID: "r-1", Status: backup.StatusFailed})

	if len(fake.sent) != 0 || len(fake.linked) != 0 {
		t.Errorf("a disabled publisher must not be used: sent=%d linked=%v", len(fake.sent), fake.linked)
	}
}

// New must leave a usable publisher behind even when nobody calls
// SetPublisher. The one existing call site in main.go predates this
// hook; a nil here would panic on the first terminal status.
func TestSchedulerPublisherDefaultsToDisabled(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if s.events == nil {
		t.Fatal("events must never be nil")
	}
	if s.events.Enabled() {
		t.Error("the default publisher must be disabled")
	}
	// Must not panic.
	s.publishBackupOutcome(context.Background(), finishedRun(backup.StatusFailed))

	s.SetPublisher(nil)
	if s.events == nil || s.events.Enabled() {
		t.Error("SetPublisher(nil) must install the disabled publisher, not nil")
	}
}

// A restore emits only on failure. A restore is always
// operator-initiated — somebody is watching the SPA while it runs — so
// a success notification is noise, while a failure means the data is
// not where they just asked it to be.
func TestPublishRestoreOutcomeOnlyOnFailure(t *testing.T) {
	for _, tc := range []struct {
		status backup.Status
		want   int
	}{
		{backup.StatusFailed, 1},
		{backup.StatusSucceeded, 0},
		{backup.StatusRunning, 0},
		{backup.StatusCancelled, 0},
	} {
		fake := &fakePublisher{enabled: true}
		restore := backup.RestoreRun{
			ID: "restore-1", BackupRunID: "run-1", TenantStack: "forge-baas",
			ResourceName: "mongo", Status: tc.status, Error: "mongorestore: auth failed",
			FinishedAt: time.Date(2026, 11, 23, 10, 5, 0, 0, time.UTC),
		}
		schedulerWithPublisher(fake).publishRestoreOutcome(context.Background(), restore)

		if len(fake.sent) != tc.want {
			t.Fatalf("status %q: sent %d events, want %d", tc.status, len(fake.sent), tc.want)
		}
		if tc.want == 0 {
			continue
		}
		ev := fake.sent[0]
		if ev.Kind != notify.KindRestoreFailed || ev.Severity != notify.SeverityError {
			t.Errorf("kind/severity = %q/%q", ev.Kind, ev.Severity)
		}
		if ev.ID != notify.BackupRunEventID("restore-1", notify.KindRestoreFailed) {
			t.Errorf("ID = %q", ev.ID)
		}
		if !strings.Contains(ev.Body, "run-1") || !strings.Contains(ev.Body, "auth failed") {
			t.Errorf("body should name the source backup and the error:\n%s", ev.Body)
		}
		if ev.Labels["restoreId"] != "restore-1" {
			t.Errorf("Labels = %v", ev.Labels)
		}
	}
}
