// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// sc-component-backup is the TEMPLATE every SC component clones from, AND
// the throwaway smoke subject of sc-components-platform slice 1a.
//
// The plugin DOES NOT register any Pulumi resource in its Provision RPC.
// SC core's Path A sidecar wrapper (slice 1a) dispatches the
// operator-chosen template with the component's image + env BEFORE
// Provision fires, and passes the deployed service URL back as
// `DependencyOutputsJson._deployed_service`. The plugin's Provision just
// reads that URL and republishes it as a PluginResourceOutput so consumers
// that `uses: [my-backup]` can reach the service.
//
// ComputeProcess is where the real plugin work lives — the push-register
// call. See plugin/internal/compute.go.
//
// # The trap this template exists to prevent
//
// Never type-assert a core-defined Go interface against a plugin-owned
// config value. The production `sc` binary does NOT link this plugin's
// /api module; plugin-owned values arrive at core as opaque
// *api.PluginFieldConfig with .Raw carrying the operator's YAML. See
// the sc-components-platform design doc §P1 v1.5 for the full rationale.
// Dispatch is Schema-advertised component_resource_types + `.Raw`
// reader, not Go-interface assertion.
//
// Schema() below advertises the component machinery:
//
//   - ProviderTypes / ResourceTypes / ComputeProcessorTypes carry the
//     one owned token, `sc-backup`.
//
//   - ComponentResourceTypes advertises `sc-backup` as a Component class
//     resource. Core's sidecar wrapper (slice 1a) checks against this
//     set-membership at dispatch.
//
//   - ComponentAllowedOverrides lists the `.Raw` keys this plugin may
//     set on the dispatched template config: image / env / ingressRoutes
//     / imagePullSecretRef. Anything outside this list is refused at
//     parse time, closing the security hole where a plugin could
//     re-point an operator's Lambda at an attacker VPC.
//
// Until slice 1a lands + a new api release is cut, the two new
// ComponentResourceTypes / ComponentAllowedOverrides fields on
// SchemaResponse don't exist in the proto. The code below guards against
// the shipped-api-version via build-tag reflection; once the pin bump
// happens, the un-guarded form is cleaner.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	hclog "github.com/hashicorp/go-hclog"
	hplugin "github.com/hashicorp/go-plugin"
	"github.com/pkg/errors"
	awsprov "github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
	"github.com/simple-container-com/api/pkg/plugin/runtime"

	backupApi "github.com/simple-container-com/sc-component-backup/api"
	backupInternal "github.com/simple-container-com/sc-component-backup/plugin/internal"
)

// backupPlugin is the gRPC server impl.
type backupPlugin struct {
	scpluginv1.UnimplementedSCCloudPluginServer

	mu       sync.Mutex
	progress *scPlugin.Publisher
	broker   *hplugin.GRPCBroker
}

// SetBroker implements scPlugin.BrokerAware. Captures the broker so
// ComputeProcess can mint a per-consumer JWT without re-dialling.
func (p *backupPlugin) SetBroker(b *hplugin.GRPCBroker) { p.broker = b }

// Schema is the first RPC SC core calls. Advertises:
//
//   - owned tokens (one: sc-backup)
//   - the plugin version
//   - zero PulumiPlugins (this backup doesn't register any Pulumi resource
//     of its own — the deployed service is the only artifact, and that
//     comes from the operator-chosen template)
//   - zero VmDrivers (no VM driver)
//
// Plus ComponentResourceTypes + ComponentAllowedOverrides — slice 1a of
// sc-components-platform landed these additive SchemaResponse fields
// (simple-container-com/api#597; feat/sc-component-dispatch branch).
// The plugin advertises `sc-backup` as its single component type + the
// starter allowlist of plugin-overridable `.Raw` keys. SC core's
// ComponentValidator picks these up at plugin-load time and the sidecar
// wrapper dispatches `my-backup` resources into the template named by
// their `config.deployTo:` field.
// componentDefaultsJSON is the plugin's advertised default `.Raw` for
// the sc-backup component resource type. SC core deep-merges the
// operator's declared `.Raw` on top at dispatch time (operator wins),
// so operators who don't write these blocks get the plugin's defaults
// automatically, while operators who DO write them keep their value.
//
// Scope note (Tier-2 + Tier-4 + Tier-5): scalars from Tier-1 +
// interpolated defaults for `storage.bucket` + `worker.cluster` +
// `storage.region` via the api's `$$sc:<expr>` sentinel (api PR
// #615). `atlasAdmin` removed in Tier-4: scoped-user mint via
// peer-invoke (api PR #616).
//
// Tier-5 (api PR #620 PreProvision + #621 ordering fix): operator
// no longer writes `env.BACKUP_ECS_SUBNETS` /
// `env.BACKUP_ECS_SECURITY_GROUPS`. The plugin's PreProvision
// method looks up the AWS account's default-VPC subnets + creates a
// per-component Fargate SG, lands `subnets-csv` /
// `security-groups-csv` as tpl extensions on the shared collector,
// and the defaults below reference them via `${resource:${name}.…}`.
// Operator-wins override still works per-key — writing
// `env.BACKUP_ECS_SUBNETS: "subnet-x,subnet-y"` beats the default.
//
// Marshalled once at init time to avoid re-marshalling on every
// Schema() RPC.
var componentDefaultsJSON = func() []byte {
	b, err := json.Marshal(map[string]any{
		// Lambda routing + the full IAM role list the backup service
		// needs for its own S3-colocated metadata + ECS-dispatched
		// backup workers. Dropping these on a stack with
		// `metadata: colocated` breaks the service's tenant upsert
		// (500) — live-caught 2026-11-14 on backup-smoke-1d's
		// minimal-shape smoke; see memory
		// [[sc_component_auto_discover_requires_withincomingenvvars_opt_in]]
		// + the scalar-defaults roadmap item.
		"cloudExtras": map[string]any{
			"lambdaRoutingType": "function-url",
			"awsRoles": []string{
				"s3:PutObject", "s3:GetObject", "s3:ListBucket",
				"s3:DeleteObject", "s3:AbortMultipartUpload",
				"ecs:RunTask", "iam:PassRole",
			},
		},
		// Only colocated is supported today; fleet-wide uniform setting.
		"metadata": map[string]any{"type": "colocated"},
		// S3 is the only storage backend on the aws-lambda target. Bucket
		// name derives from the component resource name + dispatch env;
		// region defaults from the chosen template's `config.region`.
		// Operator can override any field — operator wins per-key.
		"storage": map[string]any{
			"type":   "s3",
			"bucket": "$$sc:sc-backup-${name}-archives--${env}",
			"region": "$$sc:${template.region}",
		},
		// Worker shape: scalars fixed; cluster name derives from the
		// component resource name. VPC placement
		// (worker.subnetIds/SGs) lives in `env.BACKUP_ECS_*` below —
		// the service reads env at RunTask time; see 2026-11-15
		// struct-field removal note on BackupWorker.
		"worker": map[string]any{
			"cpu":                512,
			"memoryMB":           2048,
			"ephemeralStorageGB": 50,
			"cluster":            "$$sc:sc-backup-${name}-cluster",
		},
		// Default VPC placement — operator gets no-config Fargate
		// out of the box. PreProvision (api PR #620, ordering fix
		// #621) runs the plugin's DiscoverDefaultVPC lookup at
		// component-dispatch time, lands subnets-csv +
		// security-groups-csv as tpl extensions on the shared
		// collector, and SC core's ResolvePlaceholders pass
		// (deploy.go:202 + the post-PreProvision pass in
		// dispatchComponentTemplate) substitutes the references
		// here for their resolved values. Operator can override by
		// writing env.BACKUP_ECS_SUBNETS/SECURITY_GROUPS directly —
		// operator-wins per-key via DeepMergeComponentRaw.
		"env": map[string]any{
			"BACKUP_ECS_SUBNETS":         "$$sc:${resource:${name}.subnets-csv}",
			"BACKUP_ECS_SECURITY_GROUPS": "$$sc:${resource:${name}.security-groups-csv}",
			// Deterministic ECS resource names — PreProvision lands
			// them as tpl extensions (main.go:491). scheduler_tick.go
			// reads them to call ecs.RunTask; without these two the
			// Lambda's Fargate dispatch path 500s on "env
			// BACKUP_ECS_CLUSTER_NAME required". Operator doesn't
			// write them — the plugin owns both names.
			"BACKUP_ECS_CLUSTER_NAME": "$$sc:${resource:${name}.cluster-name}",
			"BACKUP_ECS_TASK_FAMILY":  "$$sc:${resource:${name}.task-family}",
		},
		// The only `deployTo:` target with a per-plugin dispatch
		// today. Operator can override to any aws-lambda-shaped
		// template by name.
		"deployTo": "lambda",
	})
	if err != nil {
		// Can't return an error from an init-time `var ... = func()()`;
		// a bad defaults payload would cascade to every Schema() RPC
		// anyway. Panic so a plugin binary fails fast on load.
		panic("sc-component-backup: componentDefaultsJSON: " + err.Error())
	}
	return b
}()

func (p *backupPlugin) Schema(_ context.Context, _ *scpluginv1.SchemaRequest) (*scpluginv1.SchemaResponse, error) {
	return &scpluginv1.SchemaResponse{
		PluginVersion:         "sc-component-backup-0.0.1",
		ProviderTypes:         []string{backupApi.ProviderType},
		ResourceTypes:         []string{backupApi.ResourceType},
		ComputeProcessorTypes: []string{backupApi.ComputeProcessorType},
		PulumiPlugins:         []*scpluginv1.PulumiPluginRequirement{
			// Empty — the backup plugin registers no Pulumi provider resources
			// directly. The deployed service's lambda/compose/k8s resources
			// come from the operator-chosen template (owned by
			// sc-plugin-aws / in-tree / etc.), dispatched by Path A.
		},
		VmDrivers: []*scpluginv1.VmDriverSpec{},
		// Slice 1a (SC Components Platform) — Path A dispatch.
		ComponentResourceTypes: []string{backupApi.ResourceType},
		// Slice 1d.6 (ComponentDefaults, api PR #614 → v>=TBD): plugin-
		// declared defaults SC core merges UNDER the operator's .Raw at
		// dispatch time (operator wins per-key; see pApi.DeepMergeComponentRaw).
		// First adoption of the generic mechanism — scalar constants only.
		// Follow-ups (as each generic layer lands):
		//   - storage.{bucket,region} + worker.cluster: need
		//     ${name}/${env}/${template.region} interpolation
		//     ([[sc-components-defaults-template-interpolation]]).
		//   - worker.subnetIds/securityGroupIds: need parent-output
		//     interpolation (same item, parent-output tokens section).
		//   - atlasAdmin removed entirely: needs peer-invoke RPC
		//     ([[sc-components-plugin-peer-invoke]]).
		ComponentDefaults: map[string][]byte{
			backupApi.ResourceType: componentDefaultsJSON,
		},
		// PreProvision opt-in: fires BEFORE dispatchComponentTemplate
		// so the plugin's own DiscoverDefaultVPC lookup can land
		// subnets-csv + security-groups-csv tpl extensions on the
		// shared collector. SC core's post-PreProvision
		// ResolvePlaceholders pass (api PR #621) then substitutes
		// `${resource:<name>.subnets-csv}` references in the merged
		// env for their resolved values. Without this flag SC core
		// never dials PreProvision — the plugin's lookup never runs
		// and the Lambda env gets literal tokens.
		SupportsPreProvision: true,
		ComponentAllowedOverrides: map[string]*scpluginv1.StringList{
			// cloudExtras added for slice 1c expansion (design doc §5.3):
			// the backup Component overlays lambdaRoutingType=function-url
			// (fixes the APIGW stage-prefix doubling that bit slice 1c's
			// smoke), lambdaSchedules (1-min scheduler tick → ecs.RunTask),
			// and awsRoles (ecs:RunTask + iam:PassRole + ec2:Describe* so
			// the Lambda can fire Fargate one-shot backup tasks).
			//
			// Needs sc-plugin-aws ≥ v2026.11.12-90d5f1b (the overlay-side
			// cloudExtras pass-through from sc-plugin-aws#19).
			// `worker`, `databases`, `atlasAdmin`, `storage`, `metadata`,
			// `operatorPassword` are the backup-plugin-specific top-level
			// keys on BackupConfig that SC core's validator sees as
			// NON-well-known. They land here NOT as "overlay these onto
			// the dispatched template" (template-side overlays are
			// image/env/ingressRoutes/imagePullSecretRef/cloudExtras
			// above) but because SC core's component validator dual-uses
			// the same list for allowed-top-level-keys on the component
			// config — surfaced 2026-11-13 on the first live preview
			// against the backup-smoke stack ("unknown or disallowed
			// top-level key 'worker' for component type 'sc-backup'").
			// Semantically the two concepts differ; core's
			// mergeComponentIntoTemplate leaves these unseen. The plugin
			// reads worker/databases/atlasAdmin off ComponentRaw.* in its
			// own Provision + ComputeProcess paths; storage/metadata are
			// advisory today (slice 1e.4g schema) + translate to env on
			// the dispatched service in slice 1e.4g.2.
			backupApi.ResourceType: {Values: []string{
				"image", "env", "ingressRoutes", "imagePullSecretRef", "cloudExtras",
				"worker", "storage", "metadata", "operatorPassword",
				// Removed in Tier-4 (sc-components-plugin-peer-invoke):
				//   - databases: discovery is auto now (slice 1d.7, api v2026.11.59)
				//   - atlasAdmin: creds live on the mongodb resource;
				//     scoped-user mint via peer-invoke to that plugin.
				//     Backup component is driver-agnostic.
			}},
		},
		// Generic secret-env map (api PR #630 → v>=TBD): tell SC core
		// which env var the template plugin should bind each
		// `${secret:...}` field to on the dispatched service. Replaces
		// the per-field hardcoded signingKey → SERVICE_JWT_SECRET block
		// in sc-plugin-aws's aws-lambda template component_raw overlay
		// (previously the only wired field; operatorPassword silently
		// never reached the Lambda env until live-caught 2026-11-16 on
		// backup-smoke-tier4's SPA 'No password set yet').
		//
		// Template plugins (sc-plugin-aws + successors) read this via
		// pApi.GetComponentSecretEnvMap(resourceType) and translate each
		// populated `.Raw[field]` to `cfg.Secrets[envName]`.
		ComponentSecretEnvMap: map[string]*scpluginv1.StringMap{
			backupApi.ResourceType: {Entries: map[string]string{
				"signingKey":       "SERVICE_JWT_SECRET",
				"operatorPassword": "BACKUP_OPERATOR_PASSWORD",
			}},
		},
	}, nil
}

// Provision is intentionally a near-no-op. SC core's Path A sidecar
// wrapper (slice 1a) dispatches the operator-chosen template BEFORE this
// RPC fires, passing the deployed service's URL in
// DependencyOutputsJson._deployed_service. The plugin reads that URL and
// republishes it as a PluginResourceOutput so consumers can reach the
// service.
//
// Pre-slice-1a-release: `_deployed_service` is absent from
// DependencyOutputsJson (SC core doesn't dispatch yet). The plugin logs
// the absence and returns a placeholder URL; the smoke will proceed
// against the service's address injected manually.
func (p *backupPlugin) Provision(stdctx context.Context, req *scpluginv1.ProvisionRequest) (resp *scpluginv1.ProvisionResponse, err error) {
	rctx, err := runtime.NewContext(stdctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)

	p.progress.Info("provision", fmt.Sprintf("backup component %q (type %q)", req.GetResourceName(), req.GetResourceType()))

	serviceURL, hadURL := backupInternal.ReadDeployedServiceURL(req.GetDependencyOutputsJson())
	if !hadURL {
		p.progress.Warn("provision", "DependencyOutputsJson._deployed_service absent — Path A sidecar wrapper not yet landed in SC core; service URL will be unset")
		serviceURL = "unset-pre-slice-1a"
	}

	rctx.Pulumi.Export(fmt.Sprintf("%s-service-url", req.GetResourceName()), sdk.String(serviceURL))

	// PreDestroy cleanup prep: export signingKey to parent stack state
	// so the parent-side hook can mint a short-TTL JWT to call the
	// backup service for tenant enumeration. Wrapped in sdk.ToSecret
	// so the state checkpoint encrypts it + `sc stack outputs` masks.
	//
	// Pre-Tier-4 this block also exported atlasAdmin.{public-key,
	// private-key,project-id} for the plugin's own Atlas REST calls.
	// Tier-4 removes atlasAdmin from the component config entirely —
	// scoped-user mint + delete both flow via peer-invoke to the
	// database-owning plugin (sc-plugin-mongodb for mongodb, etc.).
	// No driver-specific credentials cross through this plugin.
	{
		raw, rawErr := backupInternal.ComponentRawFromProvisionRequestLocal(req)
		if rawErr == nil && raw != nil {
			resName := req.GetResourceName()
			if sk, ok := raw["signingKey"].(string); ok && sk != "" {
				rctx.Pulumi.Export(resName+"-signing-key", sdk.ToSecret(sdk.String(sk)))
			}
		}
	}

	// Slice 1c expansion — template-aware compute backend provisioning
	// (design doc §5.3). When deployTo: lambda, the Lambda's 15-min cap
	// can't fit a real mongodump — the actual dump runs in an ECS Fargate
	// one-shot task. The plugin provisions the Fargate cluster + task
	// definition + IAM alongside (pulumi-managed per operator decision
	// α.1 + β.2). For other deployTo values this is a no-op.
	componentRaw, crErr := backupInternal.ComponentRawFromProvisionRequestLocal(req)
	if crErr != nil {
		return nil, errors.Wrap(crErr, "decode ComponentRaw")
	}
	// defaultImage: the Fargate worker task def needs a concrete image ref
	// at Provision time. When the operator declared `image:` as a string
	// on BackupConfig it reaches us verbatim; map-form (dockerfile+context
	// → SC build-and-push to ECR) can't be used for the worker because
	// the ECR URI isn't known until push-time. Fall back to the fleet-
	// canonical Dockerhub ref so a map-form smoke still exercises the
	// full ECS branch end-to-end. See EcsProvisionInputFromRaw's doc.
	ecsIn, ecsErr := backupInternal.EcsProvisionInputFromRaw(
		req.GetResourceName(), componentRaw,
		"simplecontainer/sc-component-backup:latest",
		"eu-central-1",
	)
	if ecsErr != nil {
		return nil, errors.Wrapf(ecsErr, "backup %q EcsProvisionInputFromRaw", req.GetResourceName())
	}
	if ecsIn != nil {
		// Build an aws provider in the plugin's pctx. Credentials fall
		// through the AWS SDK default chain (env, shared creds file,
		// IMDS). SC core resolved ${auth:aws} on BackupConfig and
		// exposes it via env to the plugin process — the pulumi-aws
		// provider picks it up without explicit wiring.
		awsProv, perr := awsprov.NewProvider(rctx.Pulumi, fmt.Sprintf("sc-backup-%s-aws", req.GetResourceName()), &awsprov.ProviderArgs{
			Region: sdk.String(ecsIn.Region),
		})
		if perr != nil {
			return nil, errors.Wrap(perr, "build aws provider for ECS extras")
		}
		ecsIn.AwsProvider = awsProv
		ecsOut, provErr := backupInternal.ProvisionEcsWorker(rctx.Pulumi, *ecsIn)
		if provErr != nil {
			return nil, errors.Wrapf(provErr, "backup %q ProvisionEcsWorker", req.GetResourceName())
		}
		// Export cluster + task family so `sc stack outputs` shows them
		// and operators can wire-check the Lambda env matches.
		resName := req.GetResourceName()
		rctx.Pulumi.Export(resName+"-ecs-cluster-name", ecsOut.ClusterName)
		rctx.Pulumi.Export(resName+"-ecs-cluster-arn", ecsOut.ClusterArn)
		rctx.Pulumi.Export(resName+"-ecs-task-family", ecsOut.TaskFamily)
		rctx.Pulumi.Export(resName+"-ecs-task-def-arn", ecsOut.TaskDefinition)
		rctx.Pulumi.Export(resName+"-ecs-execution-role-arn", ecsOut.ExecutionRole)
		rctx.Pulumi.Export(resName+"-ecs-task-role-arn", ecsOut.TaskRoleArn)
		p.progress.Info("provision", fmt.Sprintf("backup %q ECS worker extras: cluster=sc-backup-%s-cluster, task family=sc-backup-%s-worker", resName, resName, resName))
	}

	outputs, herr := runtime.HarvestExports(stdctx, rctx.Pulumi)
	if herr != nil {
		return nil, errors.Wrap(herr, "harvest provision exports")
	}

	// Pulumi URN of the plugin's logical resource. The real service URN
	// is owned by the dispatched template; this is just the plugin's
	// bookkeeping resource.
	urnStr := fmt.Sprintf("urn:pulumi:%s::%s::sc-component-backup:index:%s::%s",
		req.GetHeader().GetStackName(),
		req.GetHeader().GetEnvironment(),
		req.GetResourceType(),
		req.GetResourceName())

	p.progress.Info("provision", fmt.Sprintf("backup %q registered (service_url=%s)", req.GetResourceName(), serviceURL))
	return &scpluginv1.ProvisionResponse{Urn: urnStr, Outputs: outputs}, nil
}

// PreProvision fires BEFORE SC core dispatches the operator-chosen
// template for a `my-backup` resource. The plugin's Pulumi context
// is live here (same wire shape as Provision), so cloud-SDK lookups
// are legal.
//
// For `deployTo: lambda`, the plugin looks up the AWS account's
// default-VPC subnets + creates a per-component Fargate SG (allow
// outbound to MongoDB/Postgres/MySQL/HTTPS) and returns both as tpl
// extensions keyed by the component resource's name. SC core's
// post-PreProvision ResolvePlaceholders pass (api PR #621) then
// substitutes `${resource:<name>.subnets-csv}` +
// `${resource:<name>.security-groups-csv}` references in the
// merged `env` map for their resolved values — the Lambda built by
// the aws-lambda template receives the resolved comma-separated
// IDs in BACKUP_ECS_SUBNETS / BACKUP_ECS_SECURITY_GROUPS env vars.
//
// For other `deployTo:` values (vm-compose, k8s-run, ecs-fargate),
// PreProvision early-returns an empty response — VPC lookup is
// aws-lambda-specific; those branches use their own placement.
//
// Design: forge/docs/design/2026-11-15/plugin-preprovision-rpc/.
func (p *backupPlugin) PreProvision(stdctx context.Context, req *scpluginv1.PreProvisionRequest) (resp *scpluginv1.PreProvisionResponse, err error) {
	rctx, err := runtime.NewContext(stdctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)

	p.progress.Info("pre-provision", fmt.Sprintf("backup component %q (type %q)", req.GetResourceName(), req.GetResourceType()))

	// Decode ComponentRaw — the merged .Raw post-ComponentDefaults.
	// A non-lambda deployTo is a valid shape (vm-compose, k8s-run);
	// we just have nothing to look up.
	var rawMap map[string]any
	if raw := req.GetComponentRaw(); len(raw) > 0 {
		if jerr := json.Unmarshal(raw, &rawMap); jerr != nil {
			return nil, errors.Wrap(jerr, "decode ComponentRaw")
		}
	}
	dt, _ := rawMap["deployTo"].(string)
	if dt != "lambda" {
		p.progress.Info("pre-provision", fmt.Sprintf("backup %q deployTo=%q — no VPC lookup needed, skipping", req.GetResourceName(), dt))
		return &scpluginv1.PreProvisionResponse{}, nil
	}

	region := "eu-central-1"
	if storage, ok := rawMap["storage"].(map[string]any); ok {
		if r, ok := storage["region"].(string); ok && r != "" {
			region = r
		}
	}

	return runtime.RunPreProvision(
		stdctx, rctx,
		api.Stack{Name: req.GetHeader().GetStackName()},
		api.ResourceInput{Descriptor: &api.ResourceDescriptor{
			Type: req.GetResourceType(),
			Name: req.GetResourceName(),
		}},
		pApi.ProvisionParams{},
		func(ctx *sdk.Context, _ api.Stack, _ api.ResourceInput, collector pApi.ComputeContextCollector, _ pApi.ProvisionParams) error {
			// Deterministic ECS resource names — mirrors
			// ecs_provision.go:152-153. Pulumi.Export under these
			// same names happens in Provision; the tpl-extension
			// here lets the componentDefaults placeholder
			// `${resource:<name>.cluster-name}` /
			// `.task-family` resolve at dispatch time so the
			// deployed Lambda's scheduler-tick handler can call
			// `ecs.RunTask` without the operator spelling the
			// names out in server.yaml env:.
			tpl := map[string]string{
				"cluster-name": fmt.Sprintf("sc-backup-%s-cluster", req.GetResourceName()),
				"task-family":  fmt.Sprintf("sc-backup-%s-worker", req.GetResourceName()),
			}

			awsProv, perr := awsprov.NewProvider(ctx, fmt.Sprintf("sc-backup-%s-aws-preprovision", req.GetResourceName()), &awsprov.ProviderArgs{
				Region: sdk.String(region),
			})
			if perr != nil {
				return errors.Wrap(perr, "build aws provider for PreProvision")
			}
			discovery, derr := backupInternal.DiscoverDefaultVPC(stdctx, ctx, awsProv, req.GetResourceName())
			if derr != nil {
				// Swallow + log: operator can override via explicit
				// env.BACKUP_ECS_SUBNETS/SECURITY_GROUPS. Returning
				// error here would fail the whole dispatch; the
				// defaulting is a UX nice-to-have, not load-bearing.
				// The cluster-name/task-family tpl extensions ship
				// regardless — they need no AWS call.
				p.progress.Warn("pre-provision", fmt.Sprintf("backup %q DiscoverDefaultVPC: %v — operator must set env.BACKUP_ECS_SUBNETS/SECURITY_GROUPS explicitly", req.GetResourceName(), derr))
				collector.AddResourceTplExtension(req.GetResourceName(), tpl)
				return nil
			}
			tpl["subnets-csv"] = discovery.SubnetIDsCSV
			tpl["security-groups-csv"] = discovery.SecurityGroupID
			collector.AddResourceTplExtension(req.GetResourceName(), tpl)
			p.progress.Info("pre-provision", fmt.Sprintf("backup %q default-VPC: subnets=%s SG=%s", req.GetResourceName(), discovery.SubnetIDsCSV, discovery.SecurityGroupID))
			return nil
		},
	)
}

// ComputeProcess is the push-register call. When a consumer declares
// `uses: [my-backup]`, this RPC fires once per (consumer, backup) pair.
// The handler mints an HS256 JWT, POSTs the tenant record to the
// deployed service's /api/v1/tenants/register, and injects
// BACKUP_AGENT_URL + BACKUP_TOKEN env into the consumer's compute.
//
// Retry policy per sc-components-platform design doc §P3 push-model
// operational contract: see plugin/internal/compute.go.
func (p *backupPlugin) ComputeProcess(stdctx context.Context, req *scpluginv1.ComputeProcessRequest) (resp *scpluginv1.ComputeProcessResponse, err error) {
	rctx, err := runtime.NewContext(stdctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)
	p.progress.Info("compute-process", fmt.Sprintf("resource=%q type=%q (consumer=%q)",
		req.GetResourceName(), req.GetResourceType(), req.GetHeader().GetStackName()))

	parent, err := runtime.ParentInfoFromRequest(req)
	if err != nil {
		p.progress.Error("compute-process", "parent decode: "+err.Error())
		return nil, err
	}
	// Slice 1d wire-in: unmarshal the consumer's sibling-resource outputs
	// (api PR #602) and hand them to Processor.Process via
	// pApi.ProvisionParams.ResourceOutputs. That's the canonical carrier
	// for sibling outputs on the compute-process side, matching the
	// Provision side's `params.ResourceOutputs`.
	deps, err := runtime.DependencyOutputsFromComputeProcessRequest(req)
	if err != nil {
		p.progress.Error("compute-process", "deps decode: "+err.Error())
		return nil, err
	}

	// Slice 1d.4: decode the component's own `.Raw` map off the
	// ComputeProcessRequest.component_raw side-channel (api#608, released
	// in 2026.11.57). The backup plugin's compute path reads
	// `databases:` + `atlasAdmin:` off this map to mint a per-consumer
	// scoped Atlas user. extractConfigRaw (compute.go:427) picks up a
	// plain `map[string]any` as its last-resort fallback, so handing the
	// decoded raw in here lights up `maybeProvisionScopedUser` on the
	// live RPC path. Nil raw (non-component dispatch, older SC core) is
	// treated as an empty map so every other code path stays unchanged.
	componentRaw, err := runtime.ComponentRawFromComputeProcessRequest(req)
	if err != nil {
		p.progress.Error("compute-process", "component_raw decode: "+err.Error())
		return nil, err
	}
	if componentRaw == nil {
		componentRaw = map[string]any{}
	}
	input := runtime.ResourceInputForComputeProcess(req, parent, componentRaw)

	// Tier-4 (sc-components-plugin-peer-invoke) wiring: the mint-scoped-user
	// path uses sc-plugin-mongodb's peer capability when `atlasAdmin:` is
	// not configured on the backup component. PeerDialer closes over this
	// plugin's broker + the ComputeProcessRequest.header so the mint path
	// can open an SCCore client without reaching back into main.go's
	// context on each call. Returns a fresh client per call; caller closes.
	reqHeader := req.GetHeader()
	peerDialer := func(ctx context.Context) (*scPlugin.PeerClient, error) {
		return scPlugin.NewPeerClient(ctx, p.broker, reqHeader)
	}

	processor := backupInternal.NewProcessor(backupInternal.ProcessorConfig{
		Progress:   p.progress,
		PeerDialer: peerDialer,
	})

	// Seed the plugin's collector from `ComputeProcessRequest.compute_env_vars`
	// (api PR #611, released in api v2026.11.59). SC core's sidecar packs the
	// shared `ComputeContextCollector` into that field before the RPC fires, so
	// `processor.Process` sees env vars prior sibling compute processors have
	// emitted on the same consumer's deploy — `MONGO_URI`/`MONGO_DATABASE` from
	// the mongodb plugin, in the slice 1d.7 auto-discover path. Without the
	// option the collector's incoming pool is empty (the default) and
	// `discoverDatabasesFromCollector` sees nothing, which is what live-caught
	// the 2026-11-14 backup-smoke-1d run.
	resp, _, err = runtime.RunComputeProcess(
		stdctx, rctx,
		api.Stack{Name: req.GetHeader().GetStackName()}, input,
		pApi.ProvisionParams{Log: logger.New(), ParentStack: parent, ResourceOutputs: deps},
		processor.Process,
		runtime.WithIncomingEnvVars(req),
	)
	if err != nil {
		p.progress.Error("compute-process", err.Error())
		return nil, err
	}
	p.progress.Info("compute-process", fmt.Sprintf("emitted %d env var(s)", len(resp.GetEnvVars())))
	return resp, nil
}

// PreDestroy is called when the backup component resource itself is being
// destroyed (operator removes `my-backup` from server.yaml, or destroys the
// whole parent stack). The engine's reverse-topo order means consumers are
// destroyed BEFORE the backup — by the time this fires, no tenant row has
// a live consumer anymore. For a real component, this is where the cron
// scheduler stops, pending backups flush, S3 writers close. For backup,
// there is nothing to clean up; log and return.
func (p *backupPlugin) PreDestroy(ctx context.Context, req *scpluginv1.PreDestroyRequest) (*scpluginv1.PreDestroyResponse, error) {
	// PeerDialer for the scoped-user peer-invoke delete calls. Same
	// shape as the ComputeProcess wiring — closes over this plugin's
	// broker + the PreDestroyRequest.header so each per-tenant delete
	// opens a fresh SCCore client.
	reqHeader := req.GetHeader()
	peerDialer := func(ctx context.Context) (*scPlugin.PeerClient, error) {
		return scPlugin.NewPeerClient(ctx, p.broker, reqHeader)
	}
	if err := backupInternal.PreDestroy(ctx, p.progress, req, peerDialer); err != nil {
		p.progress.Error("pre-destroy", err.Error())
	}
	return &scpluginv1.PreDestroyResponse{}, nil
}

// LogStream drains the plugin's progress Publisher onto the gRPC stream.
// SC core opens one of these per plugin after Schema() returns.
func (p *backupPlugin) LogStream(_ *scpluginv1.LogStreamRequest, stream scpluginv1.SCCloudPlugin_LogStreamServer) error {
	ctx := stream.Context()
	for {
		batch, ok := p.progress.Drain()
		if !ok {
			return nil
		}
		for _, ev := range batch {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// Anchor the api import so test fixtures compile against the typed
// BackupConfig without an unused-import warning.
var _ = api.Credentials{}

func main() {
	impl := &backupPlugin{progress: scPlugin.NewPublisher(0)}
	scPlugin.Serve(scPlugin.ServerConfig{
		Impl: impl,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "sc-component-backup",
			Level:  hclog.Warn,
			Output: os.Stderr,
		}),
	})
}
