// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

// Scheduler tick — the Lambda-side half of the aws-lambda compute split
// (design doc §5.3). EventBridge fires `POST /api/v1/scheduler/tick`
// every minute. Handler enumerates the tenant store, finds backups
// that are due, and fires an ECS Fargate one-shot task per (tenant,
// resource) pair via ecs.RunTask. Lambda returns ~immediately with
// the list of fired task ARNs; the Fargate tasks run mongodump + S3
// upload out-of-band.
//
// "Due" semantics in v1 are intentionally thin — no cron rules per
// tenant yet, just an optional `?force=true` operator override that
// fires every tenant. Full scheduling logic is slice 1e. This
// handler proves the Lambda → Fargate wire works.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/simple-container-com/sc-component-backup/service/internal/backup/project"
	"github.com/simple-container-com/sc-component-backup/service/internal/backup/scheduler"
	"github.com/simple-container-com/sc-component-backup/service/internal/tenant"
)

// Env the scheduler tick reads (set at Lambda deploy time, operator-declared
// on BackupConfig.env per design doc §5.3):
//
//   - BACKUP_ECS_CLUSTER_NAME        deterministic: sc-backup-<component>-cluster
//   - BACKUP_ECS_TASK_FAMILY         deterministic: sc-backup-<component>-worker
//   - BACKUP_ECS_SUBNETS             comma-separated subnet IDs
//   - BACKUP_ECS_SECURITY_GROUPS     comma-separated SG IDs
//   - BACKUP_S3_BUCKET               passed through to the worker task
//   - BACKUP_S3_REGION               passed through to the worker task
//   - AWS_REGION                     Lambda runtime default; used for ECS client
//
// Optional:
//   - BACKUP_ECS_ASSIGN_PUBLIC_IP    "true" iff tasks need outbound without NAT
//     (default "false" — Atlas private endpoint via VPC)

// EcsClient is the subset of the AWS ECS API this handler needs. Isolated
// as an interface so tests can supply a fake without any AWS network.
type EcsClient interface {
	RunTask(ctx context.Context, params *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
}

// SchedulerTickConfig captures the handler's dependencies. In production
// NewFromEnv builds one with a lazy-constructed ECS client; tests pass
// their own ECS/time overrides.
type SchedulerTickConfig struct {
	Tenants tenant.Store
	// Runs is the backup-run history: read to decide whether a tenant
	// is due, and written when a run is dispatched. Nil means every
	// tenant reads as never-backed-up and so always due — safe for a
	// smoke, wrong for production, which is why main.go wires it.
	Runs scheduler.RunStore
	// Projects is the operator-edited per-project config, read for the
	// backup INTERVAL. Without it every tenant falls back to the
	// default, which is how the SPA came to display "every hour" (its
	// own hardcoded default) while the engine ran four-hourly.
	Projects project.Store
	// MintWorker mints the worker's callback credential. Without it the
	// worker cannot report completion, no run is ever recorded as
	// succeeded, and every tenant stays due forever — which at a
	// 30-minute tick means dumping the whole fleet 48 times a day.
	MintWorker MintWorkerJWTFn
	// AgentURL is where the worker POSTs that callback.
	AgentURL string
	// PathTemplate keeps scheduled archives at the same S3 key shape as
	// operator-triggered ones.
	PathTemplate string
	// EcsFactory lazy-builds the ECS client. Nil → NewEcsClientFromEnv.
	EcsFactory func(ctx context.Context, cfg *FargateDispatchConfig) (EcsClient, error)
	// Ecs is injected for tests. Dispatch goes through EcsFactory now
	// (the shared path), so this is wrapped into one when set.
	Ecs EcsClient
	// Now is injected for tests; defaults to time.Now.
	Now func() time.Time

	// ReplicationTick drives go-embedb's background replication loops
	// once, synchronously. Wired to [ferretdb.FerretDB.TickNow] when
	// USE_GOEMBEDB=1; nil otherwise (and on a non-goembedb stack there
	// is nothing to drive).
	//
	// This is here, on the EventBridge-driven tick, because it is the
	// only thing in this service that runs on a real invocation that
	// stays awake until its work finishes. go-embedb's five loops
	// (slot heartbeat, lease renewal, reader reconcile, snapshot, GC)
	// are wall-clock tickers, and AWS Lambda freezes the container the
	// instant the response flushes — so a loop fires only if ONE
	// invocation outlives its period. This service is awake ~21 s per
	// 30-minute tick, a ~1.2% duty cycle, and the measured result was
	// that NONE of them ever fired: all six slots sat at
	// `heartbeat == joinedAt` to the nanosecond, the replica was three
	// TTLs dead by its own record while serving traffic, and no
	// snapshot was committed for ~6 h.
	//
	// See `forge/docs/roadmap/items/go-embedb-snapshot-ticker-starves-on-lambda.md`.
	ReplicationTick func(context.Context) error
}

// NewSchedulerTickHandler returns the chi handler. Reads config knobs
// from env on every tick (cheap; the knobs don't change between warm
// container ticks).
func NewSchedulerTickHandler(cfg SchedulerTickConfig) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// A test that injects Ecs gets it used by the shared dispatch path.
	if cfg.EcsFactory == nil && cfg.Ecs != nil {
		injected := cfg.Ecs
		cfg.EcsFactory = func(context.Context, *FargateDispatchConfig) (EcsClient, error) {
			return injected, nil
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Scheduler or operator only.
		//
		// Validity alone is not enough: every tenant's plugin holds a
		// `register` token so it can push-register itself, and this
		// route spawns an ECS Fargate task per due tenant. Accepting any
		// valid token would let any single consumer stack spend money
		// across the whole fleet on demand — and with ?force=true, dump
		// every database at once.
		//
		// It also bounds the credential sitting in the EventBridge
		// rule's input, readable by anyone with events:DescribeRule in
		// the account: a scheduler token can fire the tick and nothing
		// else.
		if claims := ClaimsFromContext(ctx); claims == nil ||
			(claims.Op != OpScheduler && claims.Op != OpOperator) {
			http.Error(w, `{"error":"token op must be 'scheduler' or 'operator'"}`, http.StatusForbidden)
			return
		}

		// Drive go-embedb's replication loops FIRST, before any backup
		// work. Ordering is deliberate: this renews the slot heartbeat
		// and the tenant leases, so it must happen even when the
		// backup dispatch below fails — otherwise a stack with a
		// broken Fargate config would also silently stop replicating.
		//
		// Never fatal to the tick. A replication tick that fails is
		// retried on the next one; a backup tick that 500s because
		// replication hiccuped would stop dumping databases, which is
		// strictly worse. Logged so it is not silent.
		if cfg.ReplicationTick != nil {
			// Bounded independently of the tick's own budget: a
			// snapshot plus a GC sweep is several sequential S3
			// round-trips, and it must not be able to eat the whole
			// invocation and starve the backup dispatch.
			repCtx, repCancel := context.WithTimeout(ctx, 20*time.Second)
			if err := cfg.ReplicationTick(repCtx); err != nil {
				slog.WarnContext(repCtx, "scheduler tick: replication tick failed",
					"err", err)
			}
			repCancel()
		}

		// One dispatch path, shared with Run Now.
		//
		// This used to read env and build its own task override. That
		// duplicate drifted and was broken in two ways the Run Now path
		// was not — it read BACKUP_S3_BUCKET where the platform emits
		// STORAGE_BUCKET, and it injected none of RUN_ID / AGENT_URL /
		// AGENT_TOKEN, so the worker could never report completion.
		// Both were invisible because only the scheduled path went
		// through it and the scheduled path had never run.
		fcfg, err := ReadFargateDispatchConfigFromEnv()
		if err != nil {
			tickError(w, "fargate dispatch config", err)
			return
		}

		tenants, err := cfg.Tenants.List(ctx)
		if err != nil {
			tickError(w, "list tenants", err)
			return
		}

		type firedTask struct {
			TenantStack string `json:"tenantStack"`
			Resource    string `json:"resource"`
			TaskArn     string `json:"taskArn"`
			Error       string `json:"error,omitempty"`
		}
		fired := []firedTask{}

		// force=true fires every tenant regardless of when it was last
		// backed up — the operator's "do it now" path.
		force := r.URL.Query().Get("force") == "true"

		now := cfg.Now()
		skipped := []map[string]string{}
		for _, t := range tenants {
			if !force {
				due, why := tenantIsDue(ctx, cfg.Runs, t.StackName, resolveIntervalMinutes(ctx, cfg.Projects, t), now)
				if !due {
					// Reported, not silent. A tenant skipped for a good
					// reason and a tenant skipped because the scheduler
					// is broken look identical otherwise, and the second
					// is what went unnoticed for a day.
					skipped = append(skipped, map[string]string{
						"tenantStack": t.StackName,
						"reason":      why,
					})
					continue
				}
			}
			for _, res := range t.Resources {
				if res.Connection["driver"] == "" {
					continue
				}
				run, taskArn, derr := dispatchViaFargate(ctx, BackupNowConfig{
					Runs:         cfg.Runs,
					MintWorker:   cfg.MintWorker,
					AgentURL:     cfg.AgentURL,
					PathTemplate: cfg.PathTemplate,
					EcsFactory:   cfg.EcsFactory,
				}, fcfg, t, res, "schedule")
				ft := firedTask{TenantStack: t.StackName, Resource: res.Name, TaskArn: taskArn}
				if derr != nil {
					ft.Error = derr.Error()
				}
				_ = run
				fired = append(fired, ft)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"force":      force,
			"tenants":    len(tenants),
			"fired":      fired,
			"firedCount": len(fired),
			// Why each tenant was NOT fired. Without this a tick that
			// fires nothing is indistinguishable from a tick that is
			// broken, which is exactly how a dead scheduler stayed
			// invisible.
			"skipped":      skipped,
			"skippedCount": len(skipped),
		})
	})
}

func kv(k, v string) ecstypes.KeyValuePair {
	return ecstypes.KeyValuePair{Name: aws.String(k), Value: aws.String(v)}
}

func mustTickEnv(w http.ResponseWriter, name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		http.Error(w, fmt.Sprintf(`{"error":"env %s required"}`, name), http.StatusInternalServerError)
	}
	return v
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func tickError(w http.ResponseWriter, what string, err error) {
	http.Error(w, fmt.Sprintf(`{"error":"%s: %v"}`, what, err), http.StatusInternalServerError)
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
