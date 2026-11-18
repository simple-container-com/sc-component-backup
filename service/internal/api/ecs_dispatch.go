// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"fmt"
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

// Shared Fargate dispatch helpers used by both scheduler-tick (the
// Lambda's 1-minute cron-driven sweep) and backup-now (the SPA's
// Run Now button). Lifted out of scheduler_tick.go so a single
// place owns the task-override shape + the lazy ECS client init.
//
// The deployed Lambda reads the ECS wire from env (all set by the
// sc-component-backup plugin's auto-emission — forge/infra operator
// only sets `maxMemory`):
//
//   - BACKUP_ECS_CLUSTER_NAME       sc-backup-<component>-cluster
//   - BACKUP_ECS_TASK_FAMILY        sc-backup-<component>-worker
//   - BACKUP_ECS_SUBNETS            comma-separated subnet IDs
//   - BACKUP_ECS_SECURITY_GROUPS    comma-separated SG IDs
//   - BACKUP_S3_BUCKET              archive target bucket
//   - BACKUP_S3_REGION              archive target region
//   - BACKUP_ECS_ASSIGN_PUBLIC_IP   "true" iff tasks need outbound
//                                   without NAT (default "false")
//   - AWS_REGION                    Lambda runtime default; used for
//                                   the ECS client itself
//
// Optional (override the Fargate task role when the environment
// doesn't carry one):
//
//   - BACKUP_S3_ACCESS_KEY / _SECRET_KEY

// FargateDispatchConfig is the resolved env snapshot the handler
// builds per request. Immutable across one dispatch; next request
// re-reads env (knobs don't change between warm-container invokes
// but a Lambda environment update does flip them).
type FargateDispatchConfig struct {
	Cluster        string
	TaskFamily     string
	Subnets        []string
	SecurityGroups []string
	AssignPublicIP ecstypes.AssignPublicIp
	S3Bucket       string
	S3Region       string
	AWSRegion      string

	// Static AWS creds — empty strings mean "use the Lambda's execution
	// role via the SDK default credential chain".
	AWSAccessKey string
	AWSSecretKey string
}

// ReadFargateDispatchConfigFromEnv builds a config from the current
// process environment. Returns a typed error naming the missing key
// when any required env var is empty — handlers can surface this to
// the operator rather than silently 500ing.
func ReadFargateDispatchConfigFromEnv() (*FargateDispatchConfig, error) {
	required := func(name string) (string, error) {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return "", fmt.Errorf("env %s required", name)
		}
		return v, nil
	}
	cluster, err := required("BACKUP_ECS_CLUSTER_NAME")
	if err != nil {
		return nil, err
	}
	taskFamily, err := required("BACKUP_ECS_TASK_FAMILY")
	if err != nil {
		return nil, err
	}
	subnets, err := required("BACKUP_ECS_SUBNETS")
	if err != nil {
		return nil, err
	}
	// BACKUP_S3_BUCKET / BACKUP_S3_REGION fall back to STORAGE_BUCKET /
	// STORAGE_REGION — the plugin's generic storage-block → env
	// translation (sc-plugin-aws component_raw.go) lays those two down
	// automatically from `storage.bucket` + `storage.region`, so an
	// operator who declared the storage block gets the Fargate worker
	// env for free. Explicit BACKUP_S3_* wins for tests + non-Lambda
	// shapes where the storage block isn't used.
	s3Bucket := strings.TrimSpace(os.Getenv("BACKUP_S3_BUCKET"))
	if s3Bucket == "" {
		s3Bucket = strings.TrimSpace(os.Getenv("STORAGE_BUCKET"))
	}
	if s3Bucket == "" {
		return nil, fmt.Errorf("env BACKUP_S3_BUCKET or STORAGE_BUCKET required")
	}
	s3Region := strings.TrimSpace(os.Getenv("BACKUP_S3_REGION"))
	if s3Region == "" {
		s3Region = strings.TrimSpace(os.Getenv("STORAGE_REGION"))
	}
	if s3Region == "" {
		return nil, fmt.Errorf("env BACKUP_S3_REGION or STORAGE_REGION required")
	}
	assignIP := ecstypes.AssignPublicIpDisabled
	if strings.EqualFold(os.Getenv("BACKUP_ECS_ASSIGN_PUBLIC_IP"), "true") {
		assignIP = ecstypes.AssignPublicIpEnabled
	}
	return &FargateDispatchConfig{
		Cluster:        cluster,
		TaskFamily:     taskFamily,
		Subnets:        splitCSV(subnets),
		SecurityGroups: splitCSV(os.Getenv("BACKUP_ECS_SECURITY_GROUPS")),
		AssignPublicIP: assignIP,
		S3Bucket:       s3Bucket,
		S3Region:       s3Region,
		AWSRegion:      envOr("AWS_REGION", envOr("BACKUP_S3_REGION", "eu-central-1")),
		AWSAccessKey:   os.Getenv("BACKUP_S3_ACCESS_KEY"),
		AWSSecretKey:   os.Getenv("BACKUP_S3_SECRET_KEY"),
	}, nil
}

// NewEcsClientFromEnv builds a default ECS client from the SDK's
// default credential chain. Isolated for testability — handlers that
// take an EcsClient interface pass a fake in unit tests.
func NewEcsClientFromEnv(ctx context.Context, cfg *FargateDispatchConfig) (EcsClient, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	if cfg.AWSAccessKey != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(cfg.AWSAccessKey, cfg.AWSSecretKey, "")
	}
	return ecs.NewFromConfig(awsCfg), nil
}

// WorkerTaskSpec is the per-run input to BuildWorkerTaskOverrides.
// RunID + AgentURL + AgentToken let the worker POST back to the
// Lambda's internal callback when it exits — closes the
// SPA/overview visibility loop that in-process dumps had implicitly.
// S3Key is the full object path; callers compute it from the
// scheduler's PathTemplate.
type WorkerTaskSpec struct {
	RunID      string
	AgentURL   string
	AgentToken string
	Tenant     tenant.Record
	Resource   tenant.Resource
	S3Key      string
}

// BuildWorkerTaskOverrides builds the ECS task override that injects
// the per-run env the worker reads at startup. The container name
// MUST match the task definition's declared name ("worker") — the
// plugin's Provision sets this deterministically.
func BuildWorkerTaskOverrides(cfg *FargateDispatchConfig, spec WorkerTaskSpec) *ecstypes.TaskOverride {
	env := []ecstypes.KeyValuePair{
		kv("BACKUP_MODE", "worker"),
		kv("BACKUP_DRIVER", spec.Resource.Connection["driver"]),
		kv("BACKUP_TENANT_URI", spec.Resource.Connection["uri"]),
		kv("BACKUP_TENANT_DATABASE", spec.Resource.Connection["database"]),
		kv("BACKUP_TENANT_TLS_CA_PEM", spec.Resource.Connection["tls_ca_pem"]),
		kv("BACKUP_S3_BUCKET", cfg.S3Bucket),
		kv("BACKUP_S3_REGION", cfg.S3Region),
		kv("BACKUP_S3_KEY", spec.S3Key),
		kv("BACKUP_RUN_ID", spec.RunID),
		kv("BACKUP_AGENT_URL", spec.AgentURL),
		kv("BACKUP_AGENT_TOKEN", spec.AgentToken),
	}
	if cfg.AWSAccessKey != "" {
		env = append(env, kv("BACKUP_S3_ACCESS_KEY", cfg.AWSAccessKey))
	}
	if cfg.AWSSecretKey != "" {
		env = append(env, kv("BACKUP_S3_SECRET_KEY", cfg.AWSSecretKey))
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

// RunWorkerTask fires a single Fargate one-shot task with the given
// overrides. Returns the task ARN on success, a typed error on any
// RunTask failure or empty Tasks slice.
func RunWorkerTask(ctx context.Context, ecsCli EcsClient, cfg *FargateDispatchConfig, overrides *ecstypes.TaskOverride, startedBy string) (string, error) {
	out, err := ecsCli.RunTask(ctx, &ecs.RunTaskInput{
		Cluster:        aws.String(cfg.Cluster),
		TaskDefinition: aws.String(cfg.TaskFamily),
		LaunchType:     ecstypes.LaunchTypeFargate,
		Count:          aws.Int32(1),
		PropagateTags:  ecstypes.PropagateTagsTaskDefinition,
		StartedBy:      aws.String(startedBy),
		NetworkConfiguration: &ecstypes.NetworkConfiguration{AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
			Subnets:        cfg.Subnets,
			SecurityGroups: cfg.SecurityGroups,
			AssignPublicIp: cfg.AssignPublicIP,
		}},
		Overrides: overrides,
	})
	if err != nil {
		return "", fmt.Errorf("RunTask: %w", err)
	}
	if len(out.Tasks) > 0 {
		return aws.ToString(out.Tasks[0].TaskArn), nil
	}
	if len(out.Failures) > 0 {
		return "", fmt.Errorf("RunTask failure: %s: %s", aws.ToString(out.Failures[0].Reason), aws.ToString(out.Failures[0].Detail))
	}
	return "", fmt.Errorf("RunTask returned no tasks and no failures (unexpected)")
}

// MintWorkerJWTFn is the signature backup-now + scheduler-tick depend
// on. Lets tests inject a stub without wiring the full signing path.
// Returns an HS256 JWT valid for enough wall-clock time to cover a
// long dump (default 90m); claims identify the run + tenant so the
// internal complete handler can trust the row the worker reports on.
type MintWorkerJWTFn func(runID, tenantStack string, ttl time.Duration) (string, error)
