// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

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

func TestSchedulerTick_NoForce_FiresNoTasks(t *testing.T) {
	restore := setupTickEnv(t)
	defer restore()

	store := tenant.NewInMemoryStore()
	_ = store.Upsert(context.Background(), tenant.Record{
		StackName: "smoke-tenant",
		Resources: []tenant.Resource{
			{Name: "mongo", Connection: map[string]string{"driver": "mongodb", "uri": "mongodb://h/", "database": "db1"}},
		},
	})

	var fired int
	h := NewSchedulerTickHandler(SchedulerTickConfig{
		Tenants: store,
		Ecs: &fakeEcs{runTask: func(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
			fired++
			return &ecs.RunTaskOutput{}, nil
		}},
		Now: func() time.Time { return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC) },
	})

	req := httptest.NewRequest("POST", "/api/v1/scheduler/tick", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code %d, body %s", rec.Code, rec.Body.String())
	}
	if fired != 0 {
		t.Errorf("no-force should fire zero tasks; fired %d", fired)
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["firedCount"].(float64) != 0 {
		t.Errorf("firedCount: %v", resp["firedCount"])
	}
	if resp["tenants"].(float64) != 1 {
		t.Errorf("tenants: %v", resp["tenants"])
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
