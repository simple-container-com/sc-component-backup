// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

type fakeEcs struct {
	runTask func(ctx context.Context, params *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
}

func (f *fakeEcs) RunTask(ctx context.Context, p *ecs.RunTaskInput, opts ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	return f.runTask(ctx, p, opts...)
}

func setupTickEnv(t *testing.T) func() {
	t.Helper()
	env := map[string]string{
		"BACKUP_ECS_CLUSTER_NAME":    "sc-backup-test-cluster",
		"BACKUP_ECS_TASK_FAMILY":     "sc-backup-test-worker",
		"BACKUP_ECS_SUBNETS":         "subnet-a,subnet-b",
		"BACKUP_ECS_SECURITY_GROUPS": "sg-1",
		"BACKUP_S3_BUCKET":           "bucket-x",
		"BACKUP_S3_REGION":           "eu-central-1",
	}
	prev := map[string]string{}
	for k, v := range env {
		prev[k] = os.Getenv(k)
		_ = os.Setenv(k, v)
	}
	return func() {
		for k, v := range prev {
			_ = os.Setenv(k, v)
		}
	}
}

// runsWith returns a real in-memory RunStore seeded with history.
//
// The real store rather than a hand-rolled fake: scheduler.RunStore is
// eight methods, and the tick now WRITES to it as well as reading —
// a stub that only answers ListBackupsByTenant would pass while the
// dispatch path silently failed to persist its run row.
func runsWith(stack string, runs ...backup.BackupRun) scheduler.RunStore {
	st := scheduler.NewInMemoryRunStore()
	for i, r := range runs {
		if r.ID == "" {
			r.ID = fmt.Sprintf("%s-%d", stack, i)
		}
		if r.TenantStack == "" {
			r.TenantStack = stack
		}
		_ = st.PutBackup(context.Background(), r)
	}
	return st
}

// TestSchedulerTick_NoForce_FiresOnlyDueTenants replaces a test that
// asserted the BUG as the spec.
//
// It used to be TestSchedulerTick_NoForce_FiresNoTasks — "no-force
// should fire zero tasks" — which is exactly what the handler did:
// `if !force { continue }` on every tenant, with a comment saying v1 had
// no cron. So the scheduler could never back anything up unattended, and
// the test locked that in. Every archive in the bucket on 2026-11-19 was
// from a manual Run Now the day before, while the SPA showed "every
// hour" from a stored value nothing read.
//
// The contract now: without force, a tenant fires when it is DUE.
// tickRequest builds a tick request carrying scheduler claims.
//
// The handler requires `op` in {scheduler, operator} — validity alone is
// not enough, because every tenant's plugin holds a `register` token and
// this route spawns a Fargate task per due tenant. Tests go through the
// same gate rather than around it, so a change to that rule fails here
// rather than in production.
func tickRequest(target string) *http.Request {
	req := httptest.NewRequest("POST", target, nil)
	return req.WithContext(context.WithValue(req.Context(), claimsKey{}, &Claims{Op: OpScheduler}))
}

func TestSchedulerTick_NoForce_FiresOnlyDueTenants(t *testing.T) {
	restore := setupTickEnv(t)
	defer restore()

	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	mongo := []tenant.Resource{
		{Name: "mongo", Connection: map[string]string{"driver": "mongodb", "uri": "mongodb://h/", "database": "db1"}},
	}

	for name, tc := range map[string]struct {
		record    tenant.Record
		history   []backup.BackupRun
		wantFired int
	}{
		// Never backed up: due immediately. This is the case that makes
		// an existing fleet start getting backups at all.
		"never backed up": {
			record:    tenant.Record{StackName: "t", Resources: mongo},
			wantFired: 1,
		},
		// Succeeded 5 h ago against a 4 h interval: due.
		"older than interval": {
			record: tenant.Record{StackName: "t", Resources: mongo, IntervalMinutes: 240},
			history: []backup.BackupRun{
				{TenantStack: "t", Status: backup.StatusSucceeded, FinishedAt: now.Add(-5 * time.Hour)},
			},
			wantFired: 1,
		},
		// Succeeded 1 h ago against a 4 h interval: NOT due. The whole
		// point — a 30-minute tick must not dump every tenant every time.
		"within interval": {
			record: tenant.Record{StackName: "t", Resources: mongo, IntervalMinutes: 240},
			history: []backup.BackupRun{
				{TenantStack: "t", Status: backup.StatusSucceeded, FinishedAt: now.Add(-1 * time.Hour)},
			},
			wantFired: 0,
		},
		// Zero interval is "use the default", NOT "never" — it is what
		// every record written before the field existed carries, and
		// reading it as disabled would silently stop backing up the
		// whole already-registered fleet.
		"zero interval uses default": {
			record: tenant.Record{StackName: "t", Resources: mongo},
			history: []backup.BackupRun{
				{TenantStack: "t", Status: backup.StatusSucceeded, FinishedAt: now.Add(-1 * time.Hour)},
			},
			wantFired: 0,
		},
		// A failing tenant stays due. Attempting is not succeeding, and
		// a broken backup must keep being retried rather than be marked
		// done by having been tried.
		"only failures": {
			record: tenant.Record{StackName: "t", Resources: mongo, IntervalMinutes: 240},
			history: []backup.BackupRun{
				{TenantStack: "t", Status: backup.StatusFailed, FinishedAt: now.Add(-1 * time.Minute)},
			},
			wantFired: 1,
		},
	} {
		store := tenant.NewInMemoryStore()
		_ = store.Upsert(context.Background(), tc.record)

		fired := 0
		h := NewSchedulerTickHandler(SchedulerTickConfig{
			Tenants:    store,
			Runs:       runsWith("t", tc.history...),
			MintWorker: func(string, string, time.Duration) (string, error) { return "worker-jwt", nil },
			AgentURL:   "https://backup.example.com",
			Ecs: &fakeEcs{runTask: func(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
				fired++
				return &ecs.RunTaskOutput{}, nil
			}},
			Now: func() time.Time { return now },
		})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, tickRequest("/api/v1/scheduler/tick"))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: code %d, body %s", name, rec.Code, rec.Body.String())
		}
		if fired != tc.wantFired {
			t.Errorf("%s: fired %d, want %d (body %s)", name, fired, tc.wantFired, rec.Body.String())
		}

		// A skipped tenant must say WHY. A tick that fires nothing and a
		// tick that is broken look identical otherwise, which is how the
		// dead scheduler stayed unnoticed.
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if tc.wantFired == 0 {
			if n, _ := resp["skippedCount"].(float64); n != 1 {
				t.Errorf("%s: skippedCount = %v, want 1", name, resp["skippedCount"])
			}
		}
	}
}

// erroringRuns satisfies only the narrow runLister the due-check reads,
// which is the point: an unreadable history must not be able to make a
// tenant look done.
type erroringRuns struct{ err error }

func (e *erroringRuns) ListBackupsByTenant(context.Context, string) ([]backup.BackupRun, error) {
	return nil, e.err
}

// TestSchedulerTick_StoreErrorMeansDue — an unreadable history must err
// toward an extra backup, never toward skipping. The opposite would
// silently stop backups whenever the store hiccuped.
func TestSchedulerTick_StoreErrorMeansDue(t *testing.T) {
	due, _ := tenantIsDue(context.Background(),
		&erroringRuns{err: errors.New("store down")}, "t", 240,
		time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	if !due {
		t.Error("a store error must read as due")
	}
}

func TestSchedulerTick_Force_FiresOnePerTenantResource(t *testing.T) {
	restore := setupTickEnv(t)
	defer restore()

	store := tenant.NewInMemoryStore()
	_ = store.Upsert(context.Background(), tenant.Record{
		StackName: "smoke-tenant",
		Resources: []tenant.Resource{
			{Name: "mongo", Connection: map[string]string{"driver": "mongodb", "uri": "mongodb://h/", "database": "db1"}},
			{Name: "pg", Connection: map[string]string{"driver": "postgres", "uri": "postgres://h/", "database": "db2"}},
			{Name: "no-driver", Connection: map[string]string{"uri": "x"}}, // skipped: no driver
		},
	})

	var inputs []*ecs.RunTaskInput
	// Runs + MintWorker + AgentURL are REQUIRED now: dispatch goes
	// through the shared path, which persists a run row and mints the
	// worker's callback credential before firing. Omitting them is how
	// the scheduled path used to "work" while producing runs nothing
	// could ever mark succeeded.
	h := NewSchedulerTickHandler(SchedulerTickConfig{
		Tenants:    store,
		Runs:       scheduler.NewInMemoryRunStore(),
		MintWorker: func(string, string, time.Duration) (string, error) { return "worker-jwt", nil },
		AgentURL:   "https://backup.example.com",
		Ecs: &fakeEcs{runTask: func(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
			inputs = append(inputs, in)
			return &ecs.RunTaskOutput{Tasks: []ecstypes.Task{{TaskArn: aws.String("arn:task:" + aws.ToString(in.StartedBy))}}}, nil
		}},
		Now: func() time.Time { return time.Date(2026, 11, 13, 10, 20, 30, 0, time.UTC) },
	})

	req := tickRequest("/api/v1/scheduler/tick?force=true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code %d, body %s", rec.Code, rec.Body.String())
	}
	if len(inputs) != 2 {
		t.Fatalf("expected 2 fired tasks (mongo + pg; no-driver skipped), got %d", len(inputs))
	}

	// Mongo task should carry the right env overrides.
	mongoIn := inputs[0]
	if aws.ToString(mongoIn.Cluster) != "sc-backup-test-cluster" {
		t.Errorf("cluster: %q", aws.ToString(mongoIn.Cluster))
	}
	if aws.ToString(mongoIn.TaskDefinition) != "sc-backup-test-worker" {
		t.Errorf("taskdef: %q", aws.ToString(mongoIn.TaskDefinition))
	}
	env := map[string]string{}
	for _, e := range mongoIn.Overrides.ContainerOverrides[0].Environment {
		env[aws.ToString(e.Name)] = aws.ToString(e.Value)
	}
	if env["BACKUP_MODE"] != "worker" {
		t.Errorf("BACKUP_MODE: %q", env["BACKUP_MODE"])
	}
	if env["BACKUP_DRIVER"] != "mongodb" {
		t.Errorf("BACKUP_DRIVER: %q", env["BACKUP_DRIVER"])
	}
	if env["BACKUP_TENANT_URI"] != "mongodb://h/" {
		t.Errorf("BACKUP_TENANT_URI: %q", env["BACKUP_TENANT_URI"])
	}
	if env["BACKUP_TENANT_DATABASE"] != "db1" {
		t.Errorf("BACKUP_TENANT_DATABASE: %q", env["BACKUP_TENANT_DATABASE"])
	}
	if env["BACKUP_S3_BUCKET"] != "bucket-x" {
		t.Errorf("BACKUP_S3_BUCKET: %q", env["BACKUP_S3_BUCKET"])
	}
	// S3 key now comes from the SHARED PathTemplate, so a scheduled
	// archive lands at the same key shape as an operator-triggered one.
	// They used to differ — the tick wrote <stack>/<resource>/<ts> while
	// Run Now wrote the date-partitioned form — which would have broken
	// any lifecycle rule keyed on the date path.
	if !strings.HasPrefix(env["BACKUP_S3_KEY"], "smoke-tenant/") ||
		!strings.HasSuffix(env["BACKUP_S3_KEY"], ".archive.gz") ||
		!strings.Contains(env["BACKUP_S3_KEY"], "mongo") {
		t.Errorf("BACKUP_S3_KEY: %q", env["BACKUP_S3_KEY"])
	}

	// The worker's callback wiring — without these three it can never
	// report completion, so no run is recorded succeeded and every
	// tenant stays due forever.
	for _, k := range []string{"BACKUP_RUN_ID", "BACKUP_AGENT_URL", "BACKUP_AGENT_TOKEN"} {
		if env[k] == "" {
			t.Errorf("%s is empty — the worker cannot report completion", k)
		}
	}

	// Started-by carries the trigger + tenant stack, for operator-side
	// triage of failed tasks.
	if aws.ToString(mongoIn.StartedBy) != "schedule:smoke-tenant" {
		t.Errorf("startedBy: %q", aws.ToString(mongoIn.StartedBy))
	}
}

func TestSchedulerTick_MissingEnvReturns500(t *testing.T) {
	// No env set up — mustTickEnv returns early.
	store := tenant.NewInMemoryStore()
	h := NewSchedulerTickHandler(SchedulerTickConfig{
		Tenants: store,
		Ecs: &fakeEcs{runTask: func(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
			return nil, nil
		}},
	})

	// Isolate env.
	for _, k := range []string{"BACKUP_ECS_CLUSTER_NAME", "BACKUP_ECS_TASK_FAMILY", "BACKUP_ECS_SUBNETS"} {
		prev := os.Getenv(k)
		_ = os.Unsetenv(k)
		defer os.Setenv(k, prev)
	}

	req := tickRequest("/api/v1/scheduler/tick")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code %d, body %s", rec.Code, rec.Body.String())
	}
}

// TestSchedulerTick_RejectsNonSchedulerOps is the authorization gate.
//
// This route spawns an ECS Fargate task per due tenant, and with
// ?force=true dumps every database at once. Every tenant's plugin holds
// a `register` token so it can push-register itself — so a tick that
// accepted any valid token would let any single consumer stack spend
// money across the whole fleet on demand.
//
// It also bounds the credential in the EventBridge rule's input, which
// is readable by anyone with events:DescribeRule in the account.
func TestSchedulerTick_RejectsNonSchedulerOps(t *testing.T) {
	restore := setupTickEnv(t)
	defer restore()

	store := tenant.NewInMemoryStore()
	_ = store.Upsert(context.Background(), tenant.Record{
		StackName: "t",
		Resources: []tenant.Resource{
			{Name: "mongo", Connection: map[string]string{"driver": "mongodb", "uri": "mongodb://h/", "database": "db1"}},
		},
	})

	for name, tc := range map[string]struct {
		claims *Claims
		want   int
	}{
		"scheduler": {claims: &Claims{Op: OpScheduler}, want: http.StatusAccepted},
		"operator":  {claims: &Claims{Op: OpOperator}, want: http.StatusAccepted},
		// The one that matters: a tenant's own token must not fire it.
		"register": {claims: &Claims{Op: OpRegister, Tenant: "t"}, want: http.StatusForbidden},
		"unknown":  {claims: &Claims{Op: "nope"}, want: http.StatusForbidden},
		// The handler must not assume the middleware ran.
		"no claims": {claims: nil, want: http.StatusForbidden},
	} {
		fired := 0
		h := NewSchedulerTickHandler(SchedulerTickConfig{
			Tenants:    store,
			Runs:       scheduler.NewInMemoryRunStore(),
			MintWorker: func(string, string, time.Duration) (string, error) { return "worker-jwt", nil },
			AgentURL:   "https://backup.example.com",
			Ecs: &fakeEcs{runTask: func(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
				fired++
				return &ecs.RunTaskOutput{}, nil
			}},
			Now: func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) },
		})

		req := httptest.NewRequest("POST", "/api/v1/scheduler/tick?force=true", nil)
		if tc.claims != nil {
			req = req.WithContext(context.WithValue(req.Context(), claimsKey{}, tc.claims))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != tc.want {
			t.Errorf("%s: code %d, want %d (body %s)", name, rec.Code, tc.want, rec.Body.String())
		}
		// A refused caller must not have spawned anything.
		if tc.want == http.StatusForbidden && fired != 0 {
			t.Errorf("%s: fired %d tasks despite being refused", name, fired)
		}
	}
}

// TestScheduledDispatch_UsesStorageEnvFallback is the regression guard
// for the bug only the scheduled path could hit.
//
// The worker demands BACKUP_S3_BUCKET; sc-plugin-aws's generic storage
// block emits STORAGE_BUCKET. The tick used to build its own task
// override reading the old names via raw os.Getenv, so every scheduled
// run handed the worker an empty bucket and it exited 2 with
// `required worker env missing name=BACKUP_S3_BUCKET`.
//
// The fix was not to add a fallback there but to DELETE that duplicate:
// the shared dispatch config already had this fallback, documented, and
// Run Now had been using it all along. The duplicate existed only on a
// path that had never run, which is why it could drift unnoticed.
//
// This asserts the shared reader is what resolves it.
func TestScheduledDispatch_UsesStorageEnvFallback(t *testing.T) {
	for _, n := range []string{"BACKUP_S3_BUCKET", "BACKUP_S3_REGION"} {
		t.Setenv(n, "")
	}
	t.Setenv("BACKUP_ECS_CLUSTER_NAME", "c")
	t.Setenv("BACKUP_ECS_TASK_FAMILY", "f")
	t.Setenv("BACKUP_ECS_SUBNETS", "subnet-a")
	t.Setenv("STORAGE_BUCKET", "backup-archives--staging")
	t.Setenv("STORAGE_REGION", "eu-central-1")

	cfg, err := ReadFargateDispatchConfigFromEnv()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.S3Bucket != "backup-archives--staging" {
		t.Errorf("S3Bucket = %q, want the STORAGE_BUCKET fallback", cfg.S3Bucket)
	}

	// And the override actually carries the worker's callback wiring —
	// the OTHER half the duplicate was missing. Without these three the
	// worker cannot report completion, no run is recorded as succeeded,
	// and every tenant stays due forever.
	ov := BuildWorkerTaskOverrides(cfg, WorkerTaskSpec{
		RunID:      "run-1",
		AgentURL:   "https://backup.example.com",
		AgentToken: "tok",
		Tenant:     tenant.Record{StackName: "t"},
		Resource:   tenant.Resource{Name: "db", Connection: map[string]string{"driver": "mongodb"}},
		S3Key:      "t/db/ts.archive.gz",
	})
	got := map[string]string{}
	for _, kv := range ov.ContainerOverrides[0].Environment {
		got[*kv.Name] = *kv.Value
	}
	for k, want := range map[string]string{
		"BACKUP_S3_BUCKET":   "backup-archives--staging",
		"BACKUP_RUN_ID":      "run-1",
		"BACKUP_AGENT_URL":   "https://backup.example.com",
		"BACKUP_AGENT_TOKEN": "tok",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}
