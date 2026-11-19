// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// BackupNowConfig bundles backup-now's dependencies. Sch is required
// for the non-Lambda (in-process) path; EcsCli + MintWorker + Runs are
// required for the Lambda/Fargate path. When Fargate env is present,
// the handler dispatches through RunTask; otherwise it falls back to
// Sch.RunOnce — non-Lambda deploys (ecs-fargate/vm-compose/k8s)
// run long-lived and can dump in-process safely.
type BackupNowConfig struct {
	Sch        *scheduler.Scheduler
	Tenants    tenant.Store
	Runs       scheduler.RunStore
	MintWorker MintWorkerJWTFn
	// AgentURL is the publicly reachable base URL of the deployed
	// service — the Fargate worker POSTs its completion callback here.
	// Operator sets via plugin-emitted BACKUP_AGENT_URL (self-URL
	// published under ComponentEndpointURLKey); env override honored.
	AgentURL string
	// EcsFactory lazy-builds the ECS client once per warm container.
	// Nil → NewEcsClientFromEnv.
	EcsFactory func(ctx context.Context, cfg *FargateDispatchConfig) (EcsClient, error)
	// PathTemplate mirrors scheduler.Config.PathTemplate so Fargate
	// runs land at the same S3 key shape as in-process runs.
	PathTemplate string
}

// NewBackupNowHandler serves `POST /api/v1/tenants/{stack}/backup-now`.
// Picks the first DB-flavoured resource (connection.driver present)
// unless the request body overrides with {"resource": "<name>"}.
//
// Dispatch path:
//
//   - If BACKUP_ECS_CLUSTER_NAME is set in env → Fargate. Creates the
//     BackupRun row locally (status: running + the S3 key the worker
//     will write), fires ecs.RunTask with env carrying BACKUP_RUN_ID +
//     BACKUP_AGENT_URL + BACKUP_AGENT_TOKEN, returns 202 + {run_id,
//     task_arn}. Worker POSTs /api/v1/internal/runs/{id}/complete on
//     exit (ok/failed/bytes/s3_path/error).
//   - Otherwise → in-process Sch.RunOnce (legacy path, kept for non-
//     Lambda deploys).
//
// The sync shape is the same in both cases: 202 is the fire-and-forget
// contract, the SPA polls /runs for status; the row lifecycle is
// different under the hood.
func NewBackupNowHandler(cfg BackupNowConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stack := chi.URLParam(r, "stack")
		if stack == "" {
			http.Error(w, `{"error":"stack path param required"}`, http.StatusBadRequest)
			return
		}
		var body struct {
			Resource string `json:"resource,omitempty"`
		}
		if r.ContentLength > 0 {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}

		t, found, err := cfg.Tenants.Get(r.Context(), stack)
		if err != nil || !found {
			http.Error(w, `{"error":"tenant not found"}`, http.StatusNotFound)
			return
		}
		resourceName := body.Resource
		if resourceName == "" {
			resourceName = firstDBResource(t.Resources)
			if resourceName == "" {
				http.Error(w, `{"error":"tenant has no DB-flavoured resource"}`, http.StatusBadRequest)
				return
			}
		}
		resource, ok := findTenantResource(t.Resources, resourceName)
		if !ok {
			http.Error(w, `{"error":"tenant has no resource of that name"}`, http.StatusNotFound)
			return
		}

		// Fargate path — when BACKUP_ECS_CLUSTER_NAME is set we're in
		// Lambda mode and MUST dispatch via RunTask (the in-process
		// scheduler.RunOnce path OOMs at 128MB default and Lambda freezes
		// async goroutines). A missing sibling env is a configuration
		// bug — fail loud rather than silently fall back to the broken
		// in-process shape.
		if strings.TrimSpace(os.Getenv("BACKUP_ECS_CLUSTER_NAME")) != "" {
			fcfg, err := ReadFargateDispatchConfigFromEnv()
			if err != nil {
				http.Error(w, `{"error":"Fargate env misconfigured: `+jsonEscape(err.Error())+`"}`, http.StatusInternalServerError)
				return
			}
			run, taskArn, dispatchErr := dispatchViaFargate(r.Context(), cfg, fcfg, t, resource, "operator")
			if dispatchErr != nil {
				http.Error(w, `{"error":"`+jsonEscape(dispatchErr.Error())+`"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"run":      run,
				"task_arn": taskArn,
			})
			return
		}

		// Legacy in-process path — for non-Lambda deploys (ecs-fargate /
		// vm-compose / k8s). Lambda deploys with the OOM-prone shape fall
		// through here only when the operator deliberately removes the
		// Fargate env, which is intentional in a test harness. Otherwise
		// the shape above wins.
		runCh := make(chan backup.BackupRun, 1)
		errCh := make(chan error, 1)
		go func() {
			bg := context.Background()
			run, err := cfg.Sch.RunOnce(bg, stack, resourceName, "operator")
			if err != nil {
				errCh <- err
				return
			}
			runCh <- run
		}()
		select {
		case run := <-runCh:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(run)
		case err := <-errCh:
			if strings.Contains(err.Error(), "already in flight") {
				http.Error(w, `{"error":"`+jsonEscape(err.Error())+`"}`, http.StatusConflict)
				return
			}
			http.Error(w, `{"error":"`+jsonEscape(err.Error())+`"}`, http.StatusInternalServerError)
		}
	})
}

// dispatchViaFargate builds the BackupRun row, mints a worker JWT,
// and fires RunTask. The row is persisted BEFORE RunTask so a worker
// that dies before its first callback still appears in /runs — the
// watchdog later reclassifies it as cancelled.
// dispatchViaFargate creates the run row, mints the worker's callback
// credential and fires the one-shot task.
//
// `trigger` is recorded on the row and prefixes StartedBy: "operator"
// for Run Now, "schedule" for the scheduler tick. Both go through HERE,
// deliberately — the tick used to build its own task override, and that
// duplicate drifted until it was broken in two ways the working path
// was not: it read BACKUP_S3_BUCKET where the platform emits
// STORAGE_BUCKET, and it injected none of RUN_ID / AGENT_URL /
// AGENT_TOKEN, so the worker could never report completion and every
// tenant stayed permanently "never backed up".
func dispatchViaFargate(ctx context.Context, cfg BackupNowConfig, fcfg *FargateDispatchConfig, t tenant.Record, resource tenant.Resource, trigger string) (backup.BackupRun, string, error) {
	if cfg.Runs == nil {
		return backup.BackupRun{}, "", fmt.Errorf("Fargate dispatch: run store not wired")
	}
	if cfg.MintWorker == nil {
		return backup.BackupRun{}, "", fmt.Errorf("Fargate dispatch: worker JWT minter not wired")
	}
	if cfg.AgentURL == "" {
		return backup.BackupRun{}, "", fmt.Errorf("Fargate dispatch: AgentURL not wired (need plugin-emitted BACKUP_AGENT_URL)")
	}

	driverName := resource.Connection["driver"]
	if driverName == "" {
		return backup.BackupRun{}, "", fmt.Errorf("tenant %q resource %q: connection.driver empty", t.StackName, resource.Name)
	}
	runID := uuid.NewString()
	now := time.Now().UTC()
	s3Key := renderPath(cfg.PathTemplate, t.StackName, resource.Name, runID, now)

	run := backup.BackupRun{
		ID:            runID,
		TenantStack:   t.StackName,
		ResourceName:  resource.Name,
		Driver:        driverName,
		StartedAt:     now,
		Status:        backup.StatusRunning,
		TriggerSource: trigger,
		S3Path:        s3Key, // provisional; worker confirms on complete
	}
	if err := cfg.Runs.PutBackup(ctx, run); err != nil {
		return backup.BackupRun{}, "", fmt.Errorf("persist run: %w", err)
	}

	workerJWT, err := cfg.MintWorker(runID, t.StackName, 90*time.Minute)
	if err != nil {
		return backup.BackupRun{}, "", fmt.Errorf("mint worker JWT: %w", err)
	}

	ecsFactory := cfg.EcsFactory
	if ecsFactory == nil {
		ecsFactory = NewEcsClientFromEnv
	}
	ecsCli, err := ecsFactory(ctx, fcfg)
	if err != nil {
		return backup.BackupRun{}, "", fmt.Errorf("ecs client: %w", err)
	}

	overrides := BuildWorkerTaskOverrides(fcfg, WorkerTaskSpec{
		RunID:      runID,
		AgentURL:   cfg.AgentURL,
		AgentToken: workerJWT,
		Tenant:     t,
		Resource:   resource,
		S3Key:      s3Key,
	})
	taskArn, err := RunWorkerTask(ctx, ecsCli, fcfg, overrides, trigger+":"+t.StackName)
	if err != nil {
		// Mark the run failed so a worker that never started doesn't
		// leave the row stuck for the watchdog.
		run.Status = backup.StatusFailed
		run.FinishedAt = time.Now().UTC()
		run.Error = "RunTask failed: " + truncateError(err.Error(), 400)
		_ = cfg.Runs.UpdateBackup(ctx, run)
		return backup.BackupRun{}, "", err
	}
	return run, taskArn, nil
}

// findTenantResource returns the resource with the given name, or
// ok=false. firstDBResource stays as-is from the previous impl.
func findTenantResource(resources []tenant.Resource, name string) (tenant.Resource, bool) {
	for _, r := range resources {
		if r.Name == name {
			return r, true
		}
	}
	return tenant.Resource{}, false
}

func firstDBResource(resources []tenant.Resource) string {
	for _, r := range resources {
		if strings.TrimSpace(r.Connection["driver"]) != "" {
			return r.Name
		}
	}
	return ""
}

// renderPath mirrors scheduler/storage.RenderPath. Duplicated here to
// avoid a circular-import temptation; the template is small and
// operator-chosen (see scheduler.Config).
func renderPath(tmpl, tenantStack, resourceName, runID string, when time.Time) string {
	if tmpl == "" {
		tmpl = "{tenant}/{yyyy}/{mm}/{dd}/{resource}-{ts-rfc3339}.archive.gz"
	}
	ts := strings.ReplaceAll(when.UTC().Format(time.RFC3339), ":", "-")
	out := tmpl
	out = strings.ReplaceAll(out, "{tenant}", tenantStack)
	out = strings.ReplaceAll(out, "{resource}", resourceName)
	out = strings.ReplaceAll(out, "{runId}", runID)
	out = strings.ReplaceAll(out, "{ts-rfc3339}", ts)
	out = strings.ReplaceAll(out, "{yyyy}", when.UTC().Format("2006"))
	out = strings.ReplaceAll(out, "{mm}", when.UTC().Format("01"))
	out = strings.ReplaceAll(out, "{dd}", when.UTC().Format("02"))
	return out
}

// MintWorkerJWT returns a MintWorkerJWTFn bound to a signing key.
// Produces an HS256 JWT with the standard Claims shape (op="worker",
// tenant=<stackName>) so the existing JWTMiddleware accepts the
// worker's callback.
func MintWorkerJWT(signingKey []byte) MintWorkerJWTFn {
	return func(runID, tenantStack string, ttl time.Duration) (string, error) {
		now := time.Now()
		claims := &Claims{
			Op:     "worker",
			Tenant: tenantStack,
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    "sc-component-backup",
				Subject:   "worker:" + runID,
				IssuedAt:  jwt.NewNumericDate(now),
				NotBefore: jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			},
		}
		t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		return t.SignedString(signingKey)
	}
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}
