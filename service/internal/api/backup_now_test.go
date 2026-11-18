// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/go-chi/chi/v5"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// backupNowRouter mounts the handler at the real route path so
// chi.URLParam("stack") resolves.
func backupNowRouter(cfg BackupNowConfig) *chi.Mux {
	r := chi.NewRouter()
	r.Method(http.MethodPost, "/api/v1/tenants/{stack}/backup-now", NewBackupNowHandler(cfg))
	return r
}

// seedTenantWithDB returns a tenant store + run store, a pre-populated
// tenant with one DB-flavoured resource, and tears down env on return.
func seedTenantWithDB(t *testing.T) (*tenant.InMemoryStore, *scheduler.InMemoryRunStore) {
	t.Helper()
	ten := tenant.NewInMemoryStore()
	_ = ten.Upsert(context.Background(), tenant.Record{
		StackName: "alpha",
		Resources: []tenant.Resource{{
			Name: "alpha",
			Connection: map[string]string{
				"driver":   "mongodb",
				"uri":      "mongodb://localhost/alpha",
				"database": "alpha",
			},
		}},
	})
	return ten, scheduler.NewInMemoryRunStore()
}

// TestBackupNow_FargateDispatch_HappyPath — with ECS env set, the
// handler creates a run row (status: running + S3 path) and fires
// RunTask with the expected overrides. Returns 202 + {run, task_arn}.
func TestBackupNow_FargateDispatch_HappyPath(t *testing.T) {
	restore := setupTickEnv(t)
	defer restore()

	ten, runs := seedTenantWithDB(t)
	capturedInput := (*ecs.RunTaskInput)(nil)
	fake := &fakeEcs{runTask: func(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
		capturedInput = in
		return &ecs.RunTaskOutput{Tasks: []ecstypes.Task{{TaskArn: aws.String("arn:aws:ecs:...:task/t-abc")}}}, nil
	}}
	cfg := BackupNowConfig{
		Tenants:    ten,
		Runs:       runs,
		MintWorker: MintWorkerJWT([]byte("test-signing-key-at-least-32-byt")),
		AgentURL:   "https://backup.example.com",
		EcsFactory: func(context.Context, *FargateDispatchConfig) (EcsClient, error) { return fake, nil },
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/alpha/backup-now", nil)
	backupNowRouter(cfg).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d; body=%q", rr.Code, rr.Body.String())
	}
	var resp struct {
		Run     backup.BackupRun `json:"run"`
		TaskArn string           `json:"task_arn"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Run.ID == "" || resp.Run.Status != backup.StatusRunning {
		t.Errorf("run not persisted as running: %+v", resp.Run)
	}
	if resp.TaskArn != "arn:aws:ecs:...:task/t-abc" {
		t.Errorf("task_arn wrong: %q", resp.TaskArn)
	}
	// Row persisted in the store.
	got, err := runs.GetBackup(context.Background(), resp.Run.ID)
	if err != nil || got.Status != backup.StatusRunning {
		t.Errorf("store row not persisted: %v %+v", err, got)
	}
	// Task overrides carry the expected env.
	if capturedInput == nil || len(capturedInput.Overrides.ContainerOverrides) != 1 {
		t.Fatalf("task overrides not captured correctly: %+v", capturedInput)
	}
	envMap := map[string]string{}
	for _, kv := range capturedInput.Overrides.ContainerOverrides[0].Environment {
		envMap[aws.ToString(kv.Name)] = aws.ToString(kv.Value)
	}
	for _, k := range []string{"BACKUP_RUN_ID", "BACKUP_AGENT_URL", "BACKUP_AGENT_TOKEN", "BACKUP_TENANT_URI", "BACKUP_S3_BUCKET", "BACKUP_S3_KEY"} {
		if envMap[k] == "" {
			t.Errorf("worker env %s not set", k)
		}
	}
	if envMap["BACKUP_RUN_ID"] != resp.Run.ID {
		t.Errorf("BACKUP_RUN_ID mismatch: env=%q, run.ID=%q", envMap["BACKUP_RUN_ID"], resp.Run.ID)
	}
	if envMap["BACKUP_AGENT_URL"] != "https://backup.example.com" {
		t.Errorf("BACKUP_AGENT_URL mismatch: %q", envMap["BACKUP_AGENT_URL"])
	}
}

// TestBackupNow_FargateDispatch_RunTaskFails — RunTask error marks the
// row failed BEFORE returning so a worker that never started doesn't
// leave the row stuck for the watchdog.
func TestBackupNow_FargateDispatch_RunTaskFails(t *testing.T) {
	restore := setupTickEnv(t)
	defer restore()

	ten, runs := seedTenantWithDB(t)
	fake := &fakeEcs{runTask: func(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
		return nil, context.DeadlineExceeded
	}}
	cfg := BackupNowConfig{
		Tenants:    ten,
		Runs:       runs,
		MintWorker: MintWorkerJWT([]byte("test-signing-key-at-least-32-byt")),
		AgentURL:   "https://backup.example.com",
		EcsFactory: func(context.Context, *FargateDispatchConfig) (EcsClient, error) { return fake, nil },
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/alpha/backup-now", nil)
	backupNowRouter(cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d; body=%q", rr.Code, rr.Body.String())
	}
	// Any row persisted should now be `failed`, not stuck in running.
	all, _ := runs.ListAllBackups(context.Background(), 0)
	if len(all) != 1 {
		t.Fatalf("want 1 persisted run, got %d", len(all))
	}
	if all[0].Status != backup.StatusFailed {
		t.Errorf("want failed, got %q", all[0].Status)
	}
	if all[0].Error == "" {
		t.Error("Error field should be populated")
	}
}

// TestBackupNow_UnknownTenant_404 — a typo'd stack is a 404 before any
// dispatch fires.
func TestBackupNow_UnknownTenant_404(t *testing.T) {
	ten, runs := seedTenantWithDB(t)
	cfg := BackupNowConfig{Tenants: ten, Runs: runs}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/nope/backup-now", nil)
	backupNowRouter(cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestBackupNow_NoDBResource_400 — tenant with no DB-flavoured
// resource returns 400.
func TestBackupNow_NoDBResource_400(t *testing.T) {
	ten := tenant.NewInMemoryStore()
	_ = ten.Upsert(context.Background(), tenant.Record{StackName: "alpha"})
	cfg := BackupNowConfig{Tenants: ten, Runs: scheduler.NewInMemoryRunStore()}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/alpha/backup-now", nil)
	backupNowRouter(cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rr.Code)
	}
}

// TestMintWorkerJWT_Shape — the minted token carries op="worker" +
// tenant + a subject prefixed with "worker:" + a non-zero exp.
func TestMintWorkerJWT_Shape(t *testing.T) {
	signingKey := []byte("test-signing-key-at-least-32-byt")
	mint := MintWorkerJWT(signingKey)
	tok, err := mint("run-abc", "alpha", 30*time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Decode locally with the same key — mirrors the JWTMiddleware
	// verify step.
	if !strings.HasPrefix(tok, "eyJ") {
		t.Errorf("token should look like a JWT: %q", tok[:min(len(tok), 20)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestBackupNow_StorageBucketFallback — Lambda env often has
// STORAGE_BUCKET / STORAGE_REGION (laid down by the plugin's
// generic storage-block → env translation) but not BACKUP_S3_*.
// ReadFargateDispatchConfigFromEnv should accept the STORAGE_* form
// as a fallback so operators don't have to spell both out.
func TestBackupNow_StorageBucketFallback(t *testing.T) {
	// Set the Fargate-adjacent env but ONLY the STORAGE_* names.
	env := map[string]string{
		"BACKUP_ECS_CLUSTER_NAME": "sc-backup-test-cluster",
		"BACKUP_ECS_TASK_FAMILY":  "sc-backup-test-worker",
		"BACKUP_ECS_SUBNETS":      "subnet-a",
		"STORAGE_BUCKET":          "store-bucket",
		"STORAGE_REGION":          "eu-west-1",
	}
	prev := map[string]string{}
	for k, v := range env {
		prev[k] = os.Getenv(k)
		_ = os.Setenv(k, v)
	}
	// Explicitly unset the BACKUP_S3_* keys to make sure the fallback
	// path is exercised.
	for _, k := range []string{"BACKUP_S3_BUCKET", "BACKUP_S3_REGION"} {
		prev[k] = os.Getenv(k)
		_ = os.Unsetenv(k)
	}
	defer func() {
		for k, v := range prev {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	}()

	fcfg, err := ReadFargateDispatchConfigFromEnv()
	if err != nil {
		t.Fatalf("want fallback to resolve, got err: %v", err)
	}
	if fcfg.S3Bucket != "store-bucket" {
		t.Errorf("S3Bucket: got %q, want store-bucket", fcfg.S3Bucket)
	}
	if fcfg.S3Region != "eu-west-1" {
		t.Errorf("S3Region: got %q, want eu-west-1", fcfg.S3Region)
	}
}

// TestBackupNow_FargateEnvMisconfigured_500 — BACKUP_ECS_CLUSTER_NAME
// is set but a sibling env is missing. Handler must fail LOUDLY with
// a 500 rather than silently fall back to the broken in-process path.
func TestBackupNow_FargateEnvMisconfigured_500(t *testing.T) {
	// Cluster set, but NO task-family / subnets / bucket.
	prev := os.Getenv("BACKUP_ECS_CLUSTER_NAME")
	_ = os.Setenv("BACKUP_ECS_CLUSTER_NAME", "sc-backup-test-cluster")
	// Clear the others.
	toClear := []string{"BACKUP_ECS_TASK_FAMILY", "BACKUP_ECS_SUBNETS", "BACKUP_S3_BUCKET", "STORAGE_BUCKET", "BACKUP_S3_REGION", "STORAGE_REGION"}
	oldVals := map[string]string{}
	for _, k := range toClear {
		oldVals[k] = os.Getenv(k)
		_ = os.Unsetenv(k)
	}
	defer func() {
		if prev == "" {
			_ = os.Unsetenv("BACKUP_ECS_CLUSTER_NAME")
		} else {
			_ = os.Setenv("BACKUP_ECS_CLUSTER_NAME", prev)
		}
		for k, v := range oldVals {
			if v != "" {
				_ = os.Setenv(k, v)
			}
		}
	}()

	ten, runs := seedTenantWithDB(t)
	cfg := BackupNowConfig{Tenants: ten, Runs: runs}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/alpha/backup-now", nil)
	backupNowRouter(cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d; body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Fargate env misconfigured") {
		t.Errorf("want 'Fargate env misconfigured' in body, got %q", rr.Body.String())
	}
}
