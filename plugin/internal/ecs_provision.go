// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

// Slice 1c expansion — template-aware compute backend provisioning for
// deployTo: aws-lambda (sc-components-platform design doc §5.3). This
// file holds the aws-lambda-branch extra resources the backup plugin
// provisions ALONGSIDE the Lambda the SC core sidecar dispatched:
//
//   - aws:ecs/cluster:Cluster            per-component Fargate cluster
//   - aws:iam/role:Role                  × 2 — task execution + task role
//   - aws:iam/rolePolicy:RolePolicy      S3 write for the task role,
//     SecretsManager / logs / ECR read for the execution role
//   - aws:ecs/taskDefinition:TaskDefinition  worker image, mode flag
//
// Operator decision 2026-11-13: α.1 (Pulumi-managed) + β.2 (per-
// component cluster). Lifecycle rides SC state — `sc destroy` cleans
// up automatically via Pulumi's own teardown.
//
// Deterministic names (design doc §5.3 Lambda env wiring):
//
//   - cluster:      sc-backup-<resname>-cluster
//   - task family:  sc-backup-<resname>-worker
//
// Lambda's scheduler tick (service/internal/api/scheduler_tick.go)
// reads BACKUP_ECS_CLUSTER_NAME + BACKUP_ECS_TASK_FAMILY from env
// (operator-declared literals matching the names here) and calls
// ecs.RunTask. No Pulumi-output cross-wiring needed — ECS resolves
// the active task-def revision at RunTask time.

import (
	"encoding/json"
	"fmt"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ecs"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// ComponentRawFromProvisionRequestLocal unmarshals the ComponentRaw bytes
// from a ProvisionRequest into a map[string]any. Returns nil + nil when
// the field is absent (the resource was NOT dispatched as a Component,
// e.g. an operator ran `sc provision` against a plain backup resource).
//
// Mirrors runtime.ComponentRawFromProvisionRequest with the same
// semantics; kept in the plugin's own module so sc-component-backup
// stays decoupled from api's exact runtime package shape.
func ComponentRawFromProvisionRequestLocal(req *scpluginv1.ProvisionRequest) (map[string]any, error) {
	raw := req.GetComponentRaw()
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.Wrap(err, "unmarshal ComponentRaw JSON")
	}
	return m, nil
}

// EcsProvisionInput is the operator-config surface the backup plugin
// consumes for the aws-lambda extras. Built from BackupConfig.Worker
// (api/backup.go) at plugin.Provision time.
type EcsProvisionInput struct {
	// ComponentResourceName is the server.yaml-declared `my-backup` name.
	// Used to derive the deterministic cluster + task-family names.
	ComponentResourceName string

	// Image is the Dockerhub ref (simplecontainer/sc-component-backup:<ver>)
	// — SAME image as the Lambda runs. Mode flag flips behaviour via env.
	Image string

	// CPU / MemoryMB / EphemeralStorageGB — Fargate task sizing. See
	// BackupWorker in api/backup.go for defaults.
	CPU                int
	MemoryMB           int
	EphemeralStorageGB int

	// ArchivesBucket is the shared S3 bucket the backup worker writes
	// archives to. task-role gets s3:PutObject on this.
	ArchivesBucket string

	// Region is the AWS region for the Fargate cluster. Must match the
	// Lambda's region.
	Region string

	// AwsProvider is the plugin-pctx aws provider built from
	// BackupConfig.Credentials. Must be non-nil.
	AwsProvider sdk.ProviderResource
}

// EcsProvisionOutput names what the Provision RPC should export so the
// operator sees the task-def + cluster ARNs in `sc stack outputs`.
type EcsProvisionOutput struct {
	ClusterName    sdk.StringOutput
	ClusterArn     sdk.StringOutput
	TaskFamily     sdk.StringOutput
	TaskRoleArn    sdk.StringOutput
	ExecutionRole  sdk.StringOutput
	TaskDefinition sdk.StringOutput // revision ARN (changes on updates)
}

// ProvisionEcsWorker registers the aws-lambda-branch extras in the
// plugin's pctx. Called from plugin.Provision when deployTo: lambda.
//
// All resources use sdk.Protect(false) — the plugin owns their lifecycle,
// an operator `sc destroy` on the backup resource must take them with it.
// The dispatched Lambda IS protected (by sc-plugin-aws) but the extras
// are owned here.
func ProvisionEcsWorker(ctx *sdk.Context, in EcsProvisionInput) (*EcsProvisionOutput, error) {
	if in.AwsProvider == nil {
		return nil, errors.New("ProvisionEcsWorker: AwsProvider is required")
	}
	if in.ComponentResourceName == "" || in.Image == "" {
		return nil, errors.New("ProvisionEcsWorker: ComponentResourceName + Image required")
	}
	if in.ArchivesBucket == "" {
		return nil, errors.New("ProvisionEcsWorker: ArchivesBucket required")
	}
	if in.CPU == 0 {
		in.CPU = 512
	}
	if in.MemoryMB == 0 {
		in.MemoryMB = 2048
	}
	if in.EphemeralStorageGB == 0 {
		in.EphemeralStorageGB = 50
	}

	clusterName := fmt.Sprintf("sc-backup-%s-cluster", in.ComponentResourceName)
	taskFamily := fmt.Sprintf("sc-backup-%s-worker", in.ComponentResourceName)
	execRoleName := fmt.Sprintf("sc-backup-%s-exec-role", in.ComponentResourceName)
	taskRoleName := fmt.Sprintf("sc-backup-%s-task-role", in.ComponentResourceName)

	cluster, err := ecs.NewCluster(ctx, clusterName, &ecs.ClusterArgs{
		Name: sdk.String(clusterName),
	}, sdk.Provider(in.AwsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "ecs.NewCluster")
	}

	// Task execution role — Fargate agent uses this to pull image + ship
	// logs + fetch SecretsManager values. ECS task-exec policies.
	execRole, err := iam.NewRole(ctx, execRoleName, &iam.RoleArgs{
		Name: sdk.String(execRoleName),
		AssumeRolePolicy: sdk.String(`{
			"Version": "2012-10-17",
			"Statement": [{
				"Effect": "Allow",
				"Principal": {"Service": "ecs-tasks.amazonaws.com"},
				"Action": "sts:AssumeRole"
			}]
		}`),
	}, sdk.Provider(in.AwsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "iam.NewRole exec")
	}
	// Attach AWS-managed ECS task exec policy (ECR pull + CloudWatch Logs).
	_, err = iam.NewRolePolicyAttachment(ctx, execRoleName+"-ecs-task-exec", &iam.RolePolicyAttachmentArgs{
		Role:      execRole.Name,
		PolicyArn: sdk.String("arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"),
	}, sdk.Provider(in.AwsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "iam.NewRolePolicyAttachment exec-managed")
	}
	// Inline SecretsManager read grant — the task may need to read
	// backup-service JWT keys for per-run tokens if the design grows
	// that direction. Scoped by name prefix.
	_, err = iam.NewRolePolicy(ctx, execRoleName+"-secrets", &iam.RolePolicyArgs{
		Role: execRole.Name,
		Policy: sdk.String(`{
			"Version": "2012-10-17",
			"Statement": [{
				"Effect": "Allow",
				"Action": ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"],
				"Resource": "*"
			}]
		}`),
	}, sdk.Provider(in.AwsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "iam.NewRolePolicy exec-secrets")
	}

	// Task role — what the running container can do. S3 write on the
	// archives bucket.
	taskRole, err := iam.NewRole(ctx, taskRoleName, &iam.RoleArgs{
		Name: sdk.String(taskRoleName),
		AssumeRolePolicy: sdk.String(`{
			"Version": "2012-10-17",
			"Statement": [{
				"Effect": "Allow",
				"Principal": {"Service": "ecs-tasks.amazonaws.com"},
				"Action": "sts:AssumeRole"
			}]
		}`),
	}, sdk.Provider(in.AwsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "iam.NewRole task")
	}
	taskPolicyJSON, err := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Effect": "Allow",
				"Action": []string{
					"s3:PutObject",
					"s3:GetObject",
					"s3:ListBucket",
					"s3:AbortMultipartUpload",
					"s3:DeleteObject",
				},
				"Resource": []string{
					fmt.Sprintf("arn:aws:s3:::%s", in.ArchivesBucket),
					fmt.Sprintf("arn:aws:s3:::%s/*", in.ArchivesBucket),
				},
			},
		},
	})
	if err != nil {
		return nil, errors.Wrap(err, "marshal task role policy")
	}
	_, err = iam.NewRolePolicy(ctx, taskRoleName+"-s3", &iam.RolePolicyArgs{
		Role:   taskRole.Name,
		Policy: sdk.String(string(taskPolicyJSON)),
	}, sdk.Provider(in.AwsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "iam.NewRolePolicy task-s3")
	}

	// Container definition for the task. One container named "worker"
	// (matches scheduler_tick.go's ContainerOverride.Name reference).
	// Command is empty (image's ENTRYPOINT is /app/backup); env flips mode.
	containerDef := []map[string]any{
		{
			"name":  "worker",
			"image": in.Image,
			"environment": []map[string]string{
				{"name": "BACKUP_MODE", "value": "worker"},
			},
			"essential": true,
			"logConfiguration": map[string]any{
				"logDriver": "awslogs",
				"options": map[string]string{
					"awslogs-group":         fmt.Sprintf("/ecs/sc-backup-%s-worker", in.ComponentResourceName),
					"awslogs-region":        in.Region,
					"awslogs-stream-prefix": "worker",
					"awslogs-create-group":  "true",
				},
			},
		},
	}
	containerDefJSON, err := json.Marshal(containerDef)
	if err != nil {
		return nil, errors.Wrap(err, "marshal container definitions")
	}

	taskDef, err := ecs.NewTaskDefinition(ctx, taskFamily, &ecs.TaskDefinitionArgs{
		Family:                  sdk.String(taskFamily),
		NetworkMode:             sdk.String("awsvpc"),
		RequiresCompatibilities: sdk.StringArray{sdk.String("FARGATE")},
		Cpu:                     sdk.String(fmt.Sprintf("%d", in.CPU)),
		Memory:                  sdk.String(fmt.Sprintf("%d", in.MemoryMB)),
		EphemeralStorage: &ecs.TaskDefinitionEphemeralStorageArgs{
			SizeInGib: sdk.Int(in.EphemeralStorageGB),
		},
		ExecutionRoleArn:     execRole.Arn,
		TaskRoleArn:          taskRole.Arn,
		ContainerDefinitions: sdk.String(string(containerDefJSON)),
	}, sdk.Provider(in.AwsProvider), sdk.DependsOn([]sdk.Resource{cluster}))
	if err != nil {
		return nil, errors.Wrap(err, "ecs.NewTaskDefinition")
	}

	return &EcsProvisionOutput{
		ClusterName:    cluster.Name,
		ClusterArn:     cluster.Arn,
		TaskFamily:     sdk.String(taskFamily).ToStringOutput(),
		TaskRoleArn:    taskRole.Arn,
		ExecutionRole:  execRole.Arn,
		TaskDefinition: taskDef.Arn,
	}, nil
}

// EcsProvisionInputFromRaw builds an EcsProvisionInput from the
// ProvisionRequest's typed inputs + the ComponentRaw map. Returns
// (nil, nil) when deployTo isn't aws-lambda (no ECS extras needed).
func EcsProvisionInputFromRaw(
	resourceName string,
	raw map[string]any,
	defaultImage string,
	defaultRegion string,
) (*EcsProvisionInput, error) {
	if raw == nil {
		return nil, nil
	}
	if dt, _ := raw["deployTo"].(string); dt != "lambda" {
		return nil, nil
	}
	worker, _ := raw["worker"].(map[string]any)
	if worker == nil {
		return nil, errors.New("deployTo: lambda requires `worker` block (see BackupConfig.Worker)")
	}
	image, _ := raw["image"].(string)
	if image == "" {
		image = defaultImage
	}
	if image == "" {
		return nil, errors.New("EcsProvisionInputFromRaw: no image on config.image or defaultImage")
	}
	in := &EcsProvisionInput{
		ComponentResourceName: resourceName,
		Image:                 image,
		Region:                defaultRegion,
	}
	if v, ok := worker["cpu"].(int); ok {
		in.CPU = v
	}
	if v, ok := worker["memoryMB"].(int); ok {
		in.MemoryMB = v
	}
	if v, ok := worker["ephemeralStorageGB"].(int); ok {
		in.EphemeralStorageGB = v
	}
	// ArchivesBucket is pulled from the Lambda's env (BACKUP_S3_BUCKET).
	// We look it up on raw["env"]["BACKUP_S3_BUCKET"] — operator-declared
	// alongside the other backup env.
	if env, ok := raw["env"].(map[string]any); ok {
		if b, ok := env["BACKUP_S3_BUCKET"].(string); ok {
			in.ArchivesBucket = b
		}
	}
	if in.ArchivesBucket == "" {
		return nil, errors.New("EcsProvisionInputFromRaw: env.BACKUP_S3_BUCKET required when deployTo: lambda")
	}
	return in, nil
}
