// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
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

// fakeRuns is the run-history slice the due-check reads.
type fakeRuns struct {
	byTenant map[string][]backup.BackupRun
	err      error
}

func (f *fakeRuns) ListBackupsByTenant(_ context.Context, stack string) ([]backup.BackupRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byTenant[stack], nil
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
			Tenants: store,
			Runs:    &fakeRuns{byTenant: map[string][]backup.BackupRun{"t": tc.history}},
			Ecs: &fakeEcs{runTask: func(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
				fired++
				return &ecs.RunTaskOutput{}, nil
			}},
			Now: func() time.Time { return now },
		})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/scheduler/tick", nil))
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

// TestSchedulerTick_StoreErrorMeansDue — an unreadable history must err
// toward an extra backup, never toward skipping. The opposite would
// silently stop backups whenever the store hiccuped.
func TestSchedulerTick_StoreErrorMeansDue(t *testing.T) {
	due, _ := tenantIsDue(context.Background(),
		&fakeRuns{err: errors.New("store down")}, "t", 240,
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
	h := NewSchedulerTickHandler(SchedulerTickConfig{
		Tenants: store,
		Ecs: &fakeEcs{runTask: func(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
			inputs = append(inputs, in)
			return &ecs.RunTaskOutput{Tasks: []ecstypes.Task{{TaskArn: aws.String("arn:task:" + aws.ToString(in.StartedBy))}}}, nil
		}},
		Now: func() time.Time { return time.Date(2026, 11, 13, 10, 20, 30, 0, time.UTC) },
	})

	req := httptest.NewRequest("POST", "/api/v1/scheduler/tick?force=true", nil)
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
	// S3 key shape: <stack>/<resource>/<ts>.archive.gz; colons replaced
	// with hyphens.
	wantKey := "smoke-tenant/mongo/2026-11-13T10-20-30Z.archive.gz"
	if env["BACKUP_S3_KEY"] != wantKey {
		t.Errorf("BACKUP_S3_KEY: got %q, want %q", env["BACKUP_S3_KEY"], wantKey)
	}

	// Started-by carries the tenant stack — easier operator-side triage
	// of failed tasks.
	if aws.ToString(mongoIn.StartedBy) != "scheduler-tick:smoke-tenant" {
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

	req := httptest.NewRequest("POST", "/api/v1/scheduler/tick", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code %d, body %s", rec.Code, rec.Body.String())
	}
}
