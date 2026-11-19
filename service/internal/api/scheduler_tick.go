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
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

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
	// Runs is the backup-run history, read to decide whether a tenant
	// is due. Nil means every tenant reads as never-backed-up and so
	// always due — safe for a smoke, wrong for production, which is why
	// main.go wires it.
	Runs runLister
	// Ecs is injected for tests; when nil in production, the handler
	// builds one from the AWS SDK's default config chain on first tick.
	Ecs EcsClient
	// Now is injected for tests; defaults to time.Now.
	Now func() time.Time
}

// NewSchedulerTickHandler returns the chi handler. Reads config knobs
// from env on every tick (cheap; the knobs don't change between warm
// container ticks).
func NewSchedulerTickHandler(cfg SchedulerTickConfig) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	ecsCli := cfg.Ecs
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

		// Lazy-init the ECS client (only when Ecs wasn't injected). Done
		// once per warm container.
		if ecsCli == nil {
			region := envOr("AWS_REGION", envOr("BACKUP_S3_REGION", "eu-central-1"))
			awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
			if err != nil {
				tickError(w, "load aws config", err)
				return
			}
			if ak := strings.TrimSpace(os.Getenv("BACKUP_S3_ACCESS_KEY")); ak != "" {
				sk := os.Getenv("BACKUP_S3_SECRET_KEY")
				awsCfg.Credentials = credentials.NewStaticCredentialsProvider(ak, sk, "")
			}
			ecsCli = ecs.NewFromConfig(awsCfg)
		}

		cluster := mustTickEnv(w, "BACKUP_ECS_CLUSTER_NAME")
		if cluster == "" {
			return
		}
		taskFamily := mustTickEnv(w, "BACKUP_ECS_TASK_FAMILY")
		if taskFamily == "" {
			return
		}
		subnets := splitCSV(mustTickEnv(w, "BACKUP_ECS_SUBNETS"))
		if len(subnets) == 0 {
			return
		}
		securityGroups := splitCSV(os.Getenv("BACKUP_ECS_SECURITY_GROUPS"))
		assignPublicIP := ecstypes.AssignPublicIpDisabled
		if strings.EqualFold(os.Getenv("BACKUP_ECS_ASSIGN_PUBLIC_IP"), "true") {
			assignPublicIP = ecstypes.AssignPublicIpEnabled
		}

		// force=true fires every tenant regardless of when it was last
		// backed up — the operator's "do it now" path.
		//
		// WITHOUT force, the tick used to `continue` on every tenant and
		// do nothing at all. That is why no scheduled backup had ever
		// run: the timer was missing AND the handler was a no-op, so
		// wiring the timer alone would have changed nothing. Every
		// archive in the bucket on 2026-11-19 was from a manual Run Now
		// the day before, while the SPA displayed "every hour" from a
		// stored value nothing read.
		force := r.URL.Query().Get("force") == "true"

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

		now := cfg.Now()
		skipped := []map[string]string{}
		for _, t := range tenants {
			if !force {
				due, why := tenantIsDue(ctx, cfg.Runs, t.StackName, t.IntervalMinutes, now)
				if !due {
					// Reported, not silent. A tenant that is skipped for
					// a good reason and a tenant that is skipped because
					// the scheduler is broken look identical from the
					// outside, and the second case is what went
					// unnoticed for a day.
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
				overrides := buildTaskOverrides(t, res, now)
				out, err := ecsCli.RunTask(ctx, &ecs.RunTaskInput{
					Cluster:        aws.String(cluster),
					TaskDefinition: aws.String(taskFamily),
					LaunchType:     ecstypes.LaunchTypeFargate,
					Count:          aws.Int32(1),
					PropagateTags:  ecstypes.PropagateTagsTaskDefinition,
					StartedBy:      aws.String("scheduler-tick:" + t.StackName),
					NetworkConfiguration: &ecstypes.NetworkConfiguration{AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
						Subnets:        subnets,
						SecurityGroups: securityGroups,
						AssignPublicIp: assignPublicIP,
					}},
					Overrides: overrides,
				})
				ft := firedTask{TenantStack: t.StackName, Resource: res.Name}
				if err != nil {
					ft.Error = err.Error()
				} else if len(out.Tasks) > 0 {
					ft.TaskArn = aws.ToString(out.Tasks[0].TaskArn)
				} else if len(out.Failures) > 0 {
					ft.Error = fmt.Sprintf("RunTask failure: %s: %s", aws.ToString(out.Failures[0].Reason), aws.ToString(out.Failures[0].Detail))
				}
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

// buildTaskOverrides builds the ecs.TaskOverride that injects the
// per-tenant env the worker (BACKUP_MODE=worker) reads at startup. The
// container name MUST match what the task definition declares — the
// plugin's Provision sets this deterministically to "worker".
func buildTaskOverrides(t tenant.Record, res tenant.Resource, now time.Time) *ecstypes.TaskOverride {
	s3Prefix := strings.ReplaceAll(t.StackName, "/", "-") + "/" + res.Name
	// S3 key with colons replaced by hyphens so curl / shell handling
	// stays friendly.
	ts := strings.ReplaceAll(now.UTC().Format(time.RFC3339), ":", "-")
	env := []ecstypes.KeyValuePair{
		kv("BACKUP_MODE", "worker"),
		kv("BACKUP_DRIVER", res.Connection["driver"]),
		kv("BACKUP_TENANT_URI", res.Connection["uri"]),
		kv("BACKUP_TENANT_DATABASE", res.Connection["database"]),
		kv("BACKUP_TENANT_TLS_CA_PEM", res.Connection["tls_ca_pem"]),
		// The worker demands BACKUP_S3_*, the platform emits STORAGE_*.
		//
		// sc-plugin-aws's generic storage block puts STORAGE_BUCKET /
		// STORAGE_REGION / STORAGE_TYPE on the deployed service, and the
		// SERVICE was taught to read those — but this override builder
		// still reached for the old BACKUP_S3_* names via raw os.Getenv,
		// so every scheduled run handed the worker an empty bucket and
		// it refused to start:
		//
		//	ERROR required worker env missing name=BACKUP_S3_BUCKET
		//	container worker exit 2
		//
		// Latent until 2026-11-19 because ONLY the scheduled path goes
		// through here — Run Now builds its task from the service's
		// already-resolved storage config — and the scheduled path had
		// never executed. Exactly the class of bug a dead scheduler
		// hides.
		kv("BACKUP_S3_BUCKET", firstEnv("BACKUP_S3_BUCKET", "STORAGE_BUCKET")),
		kv("BACKUP_S3_REGION", firstEnv("BACKUP_S3_REGION", "STORAGE_REGION", "AWS_REGION")),
		kv("BACKUP_S3_KEY", s3Prefix+"/"+ts+".archive.gz"),
	}
	if ak := firstEnv("BACKUP_S3_ACCESS_KEY", "STORAGE_ACCESS_KEY"); ak != "" {
		env = append(env, kv("BACKUP_S3_ACCESS_KEY", ak))
	}
	if sk := firstEnv("BACKUP_S3_SECRET_KEY", "STORAGE_SECRET_KEY"); sk != "" {
		env = append(env, kv("BACKUP_S3_SECRET_KEY", sk))
	}
	return &ecstypes.TaskOverride{
		ContainerOverrides: []ecstypes.ContainerOverride{
			{
				Name:        aws.String("worker"),
				Environment: env,
			},
		},
	}
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

// firstEnv returns the first non-empty value among names.
//
// Exists because the same fact has two spellings: the component-native
// BACKUP_S3_* that predates the platform's generic storage block, and
// the STORAGE_* that sc-plugin-aws emits today. Preferring the explicit
// BACKUP_S3_* keeps an operator override working; falling back to
// STORAGE_* is what makes a default deploy work at all.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}
