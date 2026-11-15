// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

// Default-VPC lookup + per-component Fargate security group.
//
// PreProvision (api PR #620 + #621) runs this at component-dispatch
// time to populate `${resource:<name>.subnets-csv}` and
// `${resource:<name>.security-groups-csv}` tpl extensions on the
// shared ComputeContextCollector. The backup component's
// ComponentDefaults in main.go then defaults
// `env.BACKUP_ECS_SUBNETS` + `env.BACKUP_ECS_SECURITY_GROUPS` to
// those references, so operators don't have to spell them out.
//
// Design note: the helper creates ONE per-component security group
// rather than looking up an existing "default" SG — Fargate tasks
// need outbound-only access to MongoDB (27017) / Postgres (5432) /
// MySQL (3306) / HTTPS (443), which a generic "default" SG may not
// permit. One SG per component scopes the firewall change to the
// component's blast radius.

import (
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
)

// DefaultVPCDiscovery is the result of the PreProvision lookup.
// Both fields are already-resolved strings: SubnetIDsCSV is a comma-
// joined list suitable for the `BACKUP_ECS_SUBNETS` env var the
// service runtime reads via `splitCSV` (scheduler_tick.go); SecurityGroupID
// is a single SG id (the per-component SG this helper creates).
type DefaultVPCDiscovery struct {
	SubnetIDsCSV    string
	SecurityGroupID string
}

// DiscoverDefaultVPC looks up the AWS account's default-for-az
// subnets in the deploy's region and creates a per-component
// security group permitting outbound traffic to MongoDB (27017),
// Postgres (5432), MySQL (3306), and HTTPS (443). Returns the
// subnet IDs CSV + the resolved SG ID for the plugin's PreProvision
// to land as tpl extensions via AddResourceTplExtension.
//
// `componentName` is the `my-backup` resource name; the created SG
// is named `sc-backup-<name>-fargate-sg` (same naming pattern as
// the cluster/task-family resources ProvisionEcsWorker creates).
//
// `UnsafeAwaitOutput` is used to materialise the SG ID synchronously
// — ComputeContextCollector.AddResourceTplExtension takes plain
// strings, and PreProvision's goal is to land values the subsequent
// ResolvePlaceholders pass reads at string level.
//
// Fails cleanly when `ec2.GetSubnets` returns no results — the
// AWS account has no default subnets (unusual; happens on
// hand-crafted VPCs). The caller (PreProvision) swallows the error
// and skips the tpl extension; the operator can override by
// writing `env.BACKUP_ECS_SUBNETS` explicitly.
func DiscoverDefaultVPC(
	stdctx context.Context,
	ctx *sdk.Context,
	awsProvider sdk.ProviderResource,
	componentName string,
) (*DefaultVPCDiscovery, error) {
	subnets, err := ec2.GetSubnets(ctx, &ec2.GetSubnetsArgs{
		Filters: []ec2.GetSubnetsFilter{
			{Name: "default-for-az", Values: []string{"true"}},
		},
	}, sdk.Provider(awsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "GetSubnets default-for-az=true")
	}
	if len(subnets.Ids) == 0 {
		return nil, errors.New("no default-for-az subnets found in region — this AWS account has no default VPC; set env.BACKUP_ECS_SUBNETS explicitly")
	}

	sgName := fmt.Sprintf("sc-backup-%s-fargate-sg", componentName)
	sg, err := ec2.NewSecurityGroup(ctx, sgName, &ec2.SecurityGroupArgs{
		Name:        sdk.String(sgName),
		Description: sdk.String(fmt.Sprintf("sc-backup %s: Fargate worker outbound to backup targets", componentName)),
		Egress: ec2.SecurityGroupEgressArray{
			// MongoDB
			&ec2.SecurityGroupEgressArgs{
				Protocol:   sdk.String("tcp"),
				FromPort:   sdk.Int(27017),
				ToPort:     sdk.Int(27017),
				CidrBlocks: sdk.StringArray{sdk.String("0.0.0.0/0")},
			},
			// Postgres
			&ec2.SecurityGroupEgressArgs{
				Protocol:   sdk.String("tcp"),
				FromPort:   sdk.Int(5432),
				ToPort:     sdk.Int(5432),
				CidrBlocks: sdk.StringArray{sdk.String("0.0.0.0/0")},
			},
			// MySQL / MariaDB
			&ec2.SecurityGroupEgressArgs{
				Protocol:   sdk.String("tcp"),
				FromPort:   sdk.Int(3306),
				ToPort:     sdk.Int(3306),
				CidrBlocks: sdk.StringArray{sdk.String("0.0.0.0/0")},
			},
			// HTTPS — S3, Atlas REST, docker image pulls
			&ec2.SecurityGroupEgressArgs{
				Protocol:   sdk.String("tcp"),
				FromPort:   sdk.Int(443),
				ToPort:     sdk.Int(443),
				CidrBlocks: sdk.StringArray{sdk.String("0.0.0.0/0")},
			},
		},
		// No ingress: Fargate task is outbound-only.
	}, sdk.Provider(awsProvider))
	if err != nil {
		return nil, errors.Wrap(err, "create per-component Fargate SG")
	}

	sgIDVal, err := internals.UnsafeAwaitOutput(stdctx, sg.ID())
	if err != nil {
		return nil, errors.Wrap(err, "await SG ID")
	}
	sgIDStr, ok := sgIDVal.Value.(sdk.ID)
	if !ok {
		return nil, errors.Errorf("SG ID resolved to %T, not sdk.ID", sgIDVal.Value)
	}

	return &DefaultVPCDiscovery{
		SubnetIDsCSV:    strings.Join(subnets.Ids, ","),
		SecurityGroupID: string(sgIDStr),
	}, nil
}
