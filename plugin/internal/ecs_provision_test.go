// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	awsProvider "github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// ecsMocks captures what the Pulumi runtime would ask the state backend
// to register, so we can assert the resource graph shape without any
// AWS network. Pattern matches sc-plugin-aws/plugin/internal/ecs_compatibility_test.go.
type ecsMocks struct {
	mu     sync.Mutex
	inputs map[string]resource.PropertyMap
}

func (m *ecsMocks) Call(a sdk.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args.Copy(), nil
}

func (m *ecsMocks) NewResource(a sdk.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inputs[a.TypeToken+"/"+a.Name] = a.Inputs.Copy()
	out := a.Inputs.Copy()
	out["arn"] = resource.NewStringProperty("arn:aws:test:eu-central-1:123456789012:" + a.Name)
	if _, ok := out["name"]; !ok {
		out["name"] = resource.NewStringProperty(a.Name)
	}
	return a.Name, out, nil
}

func renderEcsWorker(t *testing.T, in *EcsProvisionInput) map[string]resource.PropertyMap {
	t.Helper()
	mocks := &ecsMocks{inputs: map[string]resource.PropertyMap{}}
	err := sdk.RunErr(func(ctx *sdk.Context) error {
		p, err := awsProvider.NewProvider(ctx, "aws", nil)
		if err != nil {
			return err
		}
		in.AwsProvider = p
		_, err = ProvisionEcsWorker(ctx, *in)
		return err
	}, sdk.WithMocks("project", "stack", mocks))
	if err != nil {
		t.Fatal(err)
	}
	return mocks.inputs
}

func TestProvisionEcsWorker_RegistersAllSixResources(t *testing.T) {
	in := &EcsProvisionInput{
		ComponentResourceName: "my-backup",
		Image:                 "simplecontainer/sc-component-backup:v1",
		CPU:                   512,
		MemoryMB:              2048,
		EphemeralStorageGB:    50,
		ArchivesBucket:        "backup-archives-smoke",
		Region:                "eu-central-1",
	}
	graph := renderEcsWorker(t, in)

	want := map[string]bool{
		"aws:ecs/cluster:Cluster/sc-backup-my-backup-cluster":                                           false,
		"aws:iam/role:Role/sc-backup-my-backup-exec-role":                                               false,
		"aws:iam/role:Role/sc-backup-my-backup-task-role":                                               false,
		"aws:iam/rolePolicyAttachment:RolePolicyAttachment/sc-backup-my-backup-exec-role-ecs-task-exec": false,
		"aws:iam/rolePolicy:RolePolicy/sc-backup-my-backup-exec-role-secrets":                           false,
		"aws:iam/rolePolicy:RolePolicy/sc-backup-my-backup-task-role-s3":                                false,
		"aws:ecs/taskDefinition:TaskDefinition/sc-backup-my-backup-worker":                              false,
	}
	for k := range graph {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, saw := range want {
		if !saw {
			t.Errorf("missing registered resource: %s", k)
		}
	}
	if t.Failed() {
		t.Logf("graph had %d resources:", len(graph))
		for k := range graph {
			t.Logf("  - %s", k)
		}
	}
}

func TestProvisionEcsWorker_TaskDefShape(t *testing.T) {
	in := &EcsProvisionInput{
		ComponentResourceName: "my-backup",
		Image:                 "simplecontainer/sc-component-backup:v2",
		CPU:                   1024,
		MemoryMB:              4096,
		EphemeralStorageGB:    100,
		ArchivesBucket:        "backup-archives-smoke",
		Region:                "eu-central-1",
	}
	graph := renderEcsWorker(t, in)

	tdef, ok := graph["aws:ecs/taskDefinition:TaskDefinition/sc-backup-my-backup-worker"]
	if !ok {
		t.Fatal("task def not registered")
	}
	if got := tdef["family"].StringValue(); got != "sc-backup-my-backup-worker" {
		t.Errorf("family: %q", got)
	}
	if got := tdef["cpu"].StringValue(); got != "1024" {
		t.Errorf("cpu: %q", got)
	}
	if got := tdef["memory"].StringValue(); got != "4096" {
		t.Errorf("memory: %q", got)
	}
	if got := tdef["networkMode"].StringValue(); got != "awsvpc" {
		t.Errorf("networkMode: %q", got)
	}

	// requiresCompatibilities = ["FARGATE"]
	rc := tdef["requiresCompatibilities"].ArrayValue()
	if len(rc) != 1 || rc[0].StringValue() != "FARGATE" {
		t.Errorf("requiresCompatibilities: %v", rc)
	}

	// Container definition JSON — verify image + BACKUP_MODE env + awslogs.
	cdefRaw := tdef["containerDefinitions"].StringValue()
	var cdefs []map[string]any
	if err := json.Unmarshal([]byte(cdefRaw), &cdefs); err != nil {
		t.Fatalf("containerDefinitions unmarshal: %v", err)
	}
	if len(cdefs) != 1 {
		t.Fatalf("expected 1 container, got %d", len(cdefs))
	}
	c := cdefs[0]
	if c["name"] != "worker" {
		t.Errorf("container name: %v", c["name"])
	}
	if c["image"] != "simplecontainer/sc-component-backup:v2" {
		t.Errorf("container image: %v", c["image"])
	}
	envRaw := c["environment"].([]any)
	if len(envRaw) != 1 {
		t.Errorf("expected 1 env var on container def, got %d", len(envRaw))
	}
	if e := envRaw[0].(map[string]any); e["name"] != "BACKUP_MODE" || e["value"] != "worker" {
		t.Errorf("BACKUP_MODE env: %v", e)
	}

	// Ephemeral storage.
	esz := tdef["ephemeralStorage"].ObjectValue()
	if esz["sizeInGib"].NumberValue() != 100 {
		t.Errorf("ephemeralStorageGB: %v", esz["sizeInGib"])
	}
}

func TestProvisionEcsWorker_S3PolicyScopedToBucket(t *testing.T) {
	in := &EcsProvisionInput{
		ComponentResourceName: "my-backup",
		Image:                 "simplecontainer/sc-component-backup:v1",
		ArchivesBucket:        "backup-archives-smoke",
		Region:                "eu-central-1",
	}
	graph := renderEcsWorker(t, in)

	policy, ok := graph["aws:iam/rolePolicy:RolePolicy/sc-backup-my-backup-task-role-s3"]
	if !ok {
		t.Fatal("s3 role policy not registered")
	}
	raw := policy["policy"].StringValue()
	if !strings.Contains(raw, `"arn:aws:s3:::backup-archives-smoke"`) {
		t.Errorf("policy doesn't reference bucket arn: %s", raw)
	}
	if !strings.Contains(raw, `"arn:aws:s3:::backup-archives-smoke/*"`) {
		t.Errorf("policy doesn't reference bucket object arn prefix: %s", raw)
	}
	if !strings.Contains(raw, `"s3:PutObject"`) {
		t.Errorf("policy missing PutObject action: %s", raw)
	}
}

func TestProvisionEcsWorker_DeterministicNaming(t *testing.T) {
	cases := []struct {
		name    string
		resName string
		wantCl  string
		wantTD  string
	}{
		{"short", "x", "sc-backup-x-cluster", "sc-backup-x-worker"},
		{"typical", "my-backup", "sc-backup-my-backup-cluster", "sc-backup-my-backup-worker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graph := renderEcsWorker(t, &EcsProvisionInput{
				ComponentResourceName: tc.resName,
				Image:                 "img:v1",
				ArchivesBucket:        "b",
				Region:                "eu-central-1",
			})
			if _, ok := graph["aws:ecs/cluster:Cluster/"+tc.wantCl]; !ok {
				t.Errorf("cluster %q not registered; graph: %v", tc.wantCl, keys(graph))
			}
			if _, ok := graph["aws:ecs/taskDefinition:TaskDefinition/"+tc.wantTD]; !ok {
				t.Errorf("task def %q not registered; graph: %v", tc.wantTD, keys(graph))
			}
		})
	}
}

func TestEcsProvisionInputFromRaw(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		wantNil  bool
		wantErr  string
		wantCPU  int
		wantMem  int
		wantEph  int
		wantBckt string
	}{
		{
			name:    "no raw: nil, no error",
			raw:     nil,
			wantNil: true,
		},
		{
			name: "deployTo != lambda: nil, no error",
			raw: map[string]any{
				"deployTo": "ecs-fargate",
				"image":    "simplecontainer/sc-component-backup:v1",
			},
			wantNil: true,
		},
		{
			name: "deployTo: lambda without worker: error",
			raw: map[string]any{
				"deployTo": "lambda",
				"image":    "simplecontainer/sc-component-backup:v1",
				"env":      map[string]any{"BACKUP_S3_BUCKET": "b"},
			},
			wantErr: "requires `worker`",
		},
		{
			name: "deployTo: lambda without env.BACKUP_S3_BUCKET: error",
			raw: map[string]any{
				"deployTo": "lambda",
				"image":    "simplecontainer/sc-component-backup:v1",
				"worker":   map[string]any{"cpu": 512, "memoryMB": 2048},
			},
			wantErr: "storage.bucket",
		},
		{
			name: "complete lambda config: input populated",
			raw: map[string]any{
				"deployTo": "lambda",
				"image":    "simplecontainer/sc-component-backup:v1",
				"worker": map[string]any{
					"cpu":                1024,
					"memoryMB":           4096,
					"ephemeralStorageGB": 100,
				},
				"env": map[string]any{"BACKUP_S3_BUCKET": "backup-smoke"},
			},
			wantCPU:  1024,
			wantMem:  4096,
			wantEph:  100,
			wantBckt: "backup-smoke",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := EcsProvisionInputFromRaw("my-backup", tc.raw, "", "eu-central-1")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.wantNil {
				if in != nil {
					t.Errorf("want nil input, got %+v", in)
				}
				return
			}
			if in.CPU != tc.wantCPU {
				t.Errorf("CPU: got %d, want %d", in.CPU, tc.wantCPU)
			}
			if in.MemoryMB != tc.wantMem {
				t.Errorf("MemoryMB: got %d, want %d", in.MemoryMB, tc.wantMem)
			}
			if in.EphemeralStorageGB != tc.wantEph {
				t.Errorf("EphemeralStorageGB: got %d, want %d", in.EphemeralStorageGB, tc.wantEph)
			}
			if in.ArchivesBucket != tc.wantBckt {
				t.Errorf("ArchivesBucket: %q", in.ArchivesBucket)
			}
		})
	}
}

func keys(m map[string]resource.PropertyMap) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
