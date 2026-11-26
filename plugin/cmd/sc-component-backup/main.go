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
	"net/http"
	"os"
	"strings"
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

// version is stamped by the release workflow via
// `go build -ldflags "-X main.version=${VERSION}"` (release.yml:87). The
// default `"dev"` is what a local `make build` or a test run sees.
//
// Historically this field did not exist and the PluginVersion below was
// the literal `"sc-component-backup-0.0.1"` — so a provision log said
// nothing about which plugin binary was actually loaded, and the
// migrationToken wire (PR #37) had no log-level verification that the
// allowlist-carrying binary was the one running. observatory +
// notifier both stamp this way; the gap was a backup-only miss.
var version = "dev"

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
	defaults := map[string]any{
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
			// `rate(2 minutes)`, NOT the `rate(30 minutes)` this was
			// until 2026-11-26, and the number is a DATA-DURABILITY
			// knob rather than a scheduling one.
			//
			// The tick is the only thing that drives go-embedb's
			// replication loops here — a frozen Lambda does not tick,
			// so `Handler.TickNow` is called from the tick handler and
			// nowhere else. Reader catch-up is one of those loops. So
			// the tick period IS the window in which a peer will
			// replay another replica's log, and at 30 minutes that
			// window was long enough to lose acked writes:
			//
			//   A replica serves a write, uploads the log segment to
			//   S3, collects peer acks, returns 200 — and freezes. Its
			//   heartbeat never renews. R14 reclaims the slot, the
			//   epoch bumps, and from then on peer readers REJECT
			//   every entry it wrote (entries below the slot table's
			//   current epoch are refused by design — §3.4), or GC
			//   Phase A deletes the evicted slot's log outright. The
			//   bytes are durable in S3 and permanently unreadable.
			//
			// Live-caught 2026-11-26, two symptoms one cause: the
			// forge-baas deploy's two `tenants/register` writes were
			// both acked 200 and lost, leaving a rotated-away Atlas
			// password in the tenant row (every subsequent mongodump:
			// `(AuthenticationFailed)`); and four worker
			// `runs/{id}/complete` callbacks were acked 200 and lost,
			// leaving finished 1.9 GB backups stuck on `running` until
			// a reaper called them `interrupted` — which in turn made
			// `tenantIsDue` read forge-conductor as never-backed-up and
			// dump it 8 times in a day. The timing is the tell: every
			// write that landed AT a tick persisted, every write that
			// landed between ticks was lost.
			//
			// 2 minutes also removes most cold starts (a container
			// stays warm across a 2-minute gap), which means fewer
			// slot claims, fewer epoch bumps, and less of the churn
			// that orphans entries in the first place. The two healthy
			// components on this engine already live here —
			// notifier's `nt-tick` is `rate(1 minute)`, observatory's
			// `hc-tick` is `rate(5 minutes)`; backup at 30 minutes was
			// the fleet's only ~1.2%-duty-cycle replica, and
			// `TickNow`'s own measured table names that row as the
			// broken one.
			//
			// Raising the cadence NARROWS the loss window; it does not
			// close it. The structural fix — an acked write must not
			// become unreadable when its writer's slot is reclaimed —
			// is go-embedb's, tracked in
			// `forge/docs/roadmap/items/go-embedb-reclaim-orphans-unreplayed-writes.md`.
			//
			// If you re-time this rule, re-read the `HeartbeatTTL`
			// comment in `service/cmd/backup/boot_goembedb.go`: the
			// two are related, and `Handler.TickNow` warns when the
			// TTL drops below 2x the cadence it observes.
			"lambdaSchedules": []any{
				map[string]any{
					"name":       "bk-tick",
					"expression": "rate(2 minutes)",
					"request":    schedulerTickRequest("scheduled-backup", "/api/v1/scheduler/tick"),
				},
			},
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
			// Self-URL the Fargate worker POSTs its completion
			// callback to. Derived from `ingress.hostname` at
			// PreProvision time — when the operator declares a
			// public hostname, this lands automatically. Without
			// it the Fargate-dispatched Run Now row never flips
			// from `running` to `succeeded` until the startup
			// watchdog sweeps it.
			"BACKUP_AGENT_URL": "$$sc:${resource:${name}.backup-agent-url}",
			// Where to publish operator-relevant events — a failed
			// backup, a failed restore. Plain env on purpose: a URL
			// is not a credential, and the matching NOTIFY_TOKEN
			// travels the SecretsManager-backed ComponentSecretEnvMap
			// path instead. Empty when the operator declared no
			// `notify:` block, and the service treats an empty
			// NOTIFY_URL as "notifications off".
			"NOTIFY_URL": "$$sc:${resource:${name}.notify-url}",
			// Default-VPC subnets have an IGW route but no NAT;
			// without a public IP the Fargate task can't reach
			// CloudWatch/S3/ECR and dies before it starts.
			// PreProvision lands "true" in the default-VPC code
			// path; operator can override by setting the env
			// explicitly in server.yaml if they have a NAT + VPC
			// endpoints.
			"BACKUP_ECS_ASSIGN_PUBLIC_IP": "$$sc:${resource:${name}.assign-public-ip}",
			// go-embedb is the metadata engine, not an opt-in
			// experiment: the colocated store it replaced served
			// read-modify-write upserts from whichever warm Lambda
			// execution environment held the lease and answered
			// NotWritablePrimary(10107) from every other one, which
			// failed roughly half of all tenant registrations and so
			// failed the deploy of every service naming this
			// component in `uses:`. An operator who leaves this unset
			// should get the engine the fleet actually runs.
			"USE_GOEMBEDB": "1",
			// The bucket go-embedb keeps its log + snapshots in. The
			// SAME bucket as the archives on purpose — one lifecycle
			// policy, one set of grants, and the `awsRoles` above
			// already cover it. Derived at PreProvision from the
			// operator's `storage:` block so the two shapes
			// (`storage: my-data` vs `storage: {bucket: my-data}`)
			// cannot disagree; see [backup.BucketForStorageRef] for
			// why that is a live hazard rather than a nicety.
			"GOEMBEDB_S3_BUCKET": "$$sc:${resource:${name}.storage-bucket}",
		},
		// Lambda sizing. 512 MB is measured, not guessed: the service
		// is a Go binary plus an embedded SPA whose largest in-flight
		// object is one dump-dispatch payload (the archive itself is
		// streamed by the Fargate worker, never buffered here).
		//
		// The 300 s timeout is NOT about a slow handler — it is the
		// scheduler tick, which fans out across every registered
		// tenant and dispatches a Fargate task per due database. SC's
		// own default is 128 MB / 30 s; the sweep does not fit in it.
		"maxMemory": 512,
		"timeout":   300,
		// The only `deployTo:` target with a per-plugin dispatch
		// today. Operator can override to any aws-lambda-shaped
		// template by name.
		"deployTo": "lambda",
	}
	// The image this plugin version's service ships as. Omitted for an
	// unstamped build so the operator gets SC's "image required" rather
	// than a pull of `:dev` that does not exist — see [defaultImage].
	if img := defaultImage(); img != "" {
		defaults["image"] = img
	}
	b, err := json.Marshal(defaults)
	if err != nil {
		// Can't return an error from an init-time `var ... = func()()`;
		// a bad defaults payload would cascade to every Schema() RPC
		// anyway. Panic so a plugin binary fails fast on load.
		panic("sc-component-backup: componentDefaultsJSON: " + err.Error())
	}
	return b
}()

// imageRepo is the Docker Hub repository this plugin's service image is
// published to. Kept beside [defaultImage] because the two are a pair:
// release.yml pushes `<imageRepo>:<tag>` and stamps `<tag>` into
// [version] in the same job, off the same CalVer value.
const imageRepo = "simplecontainer/sc-component-backup"

// defaultImage is the service image that matches THIS plugin binary.
//
// The plugin and its service image are released together —
// `.github/workflows/release.yml` computes one CalVer tag, stamps it
// into `main.version` with `-ldflags -X`, and pushes
// `simplecontainer/sc-component-backup:<that same tag>`. So the binary
// already knows the image it belongs with, and an operator who pins
// the plugin version has, by construction, pinned the image too.
//
// Defaulting it here is therefore not a convenience — it ENFORCES the
// lockstep. Two pins for one release is two chances to bump one and
// not the other, and a plugin running against a mismatched service
// image fails in whichever way the contract happened to change that
// week, with nothing in the config to point at.
//
// Returns "" for an unstamped build (`version` is "dev" for a local
// `go build`), so a developer gets SC's own "image required" error
// instead of a pull of a tag that was never pushed.
func defaultImage() string {
	if version == "" || version == "dev" {
		return ""
	}

	return imageRepo + ":" + version
}

// schedulerTickRequest renders the EventBridge target input that drives
// an inline scheduler tick.
//
// The schedule has to exist, and it has to live HERE rather than in
// every operator's server.yaml, because Lambda freezes background
// goroutines the instant the handler returns: the sweep cannot be a
// ticker inside the process, so it is an HTTP endpoint that runs the
// work inline when called, and something external has to call it. On
// aws-lambda that something is a rule built from
// `cloudExtras.lambdaSchedules`.
//
// THE SENTINEL POSITION IS THE WHOLE TRICK, and it is why this default
// was previously believed impossible. `substituteString`
// (api `pkg/clouds/pulumi/api/defaults_interpolation.go:174`) returns
// the string UNCHANGED unless it BEGINS with `$$sc:`. So the obvious
// shape — the sentinel next to the token it guards, as in
// `"Bearer $$sc:${resource:${name}.scheduler-token}"` — is skipped in
// silence. That is exactly what shipped to sc-component-observatory on
// 2026-11-19: the rule was created, fired on time, and the tick
// answered 401 twice with the literal
// `Bearer $$sc:${resource:${name}.scheduler-token}` in its input. The
// conclusion drawn then was that SC resolves `${secret:NAME}` inside a
// schedule payload but not the tpl-extension form, and that a generic
// component therefore cannot ship a schedule at all — a ~25-line
// comment to that effect went into all three components. It was a
// misdiagnosis. With the sentinel at position 0 of the whole payload,
// `${name}` substitutes, the sentinel is stripped, and SC core's
// post-PreProvision `ResolvePlaceholders` pass
// (`plugin_sidecars.go:1580`) walks into
// `cloudExtras.lambdaSchedules[].request` — a string inside a map
// inside a slice — and substitutes `${resource:…}` mid-string.
//
// Constraint worth keeping in view: a schedule `name` longer than 14
// characters is rejected at APPLY, after preview has passed.
func schedulerTickRequest(requestID, path string) string {
	req := map[string]any{
		"requestId":  requestID,
		"httpMethod": http.MethodPost,
		"path":       path,
		"requestContext": map[string]any{
			"http": map[string]any{
				"path":     path,
				"method":   http.MethodPost,
				"protocol": "HTTP/1.1",
			},
		},
		"body": "{}",
		"headers": map[string]any{
			"Authorization": "Bearer ${resource:${name}.scheduler-token}",
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		// Unreachable: every value above is a marshallable literal.
		// Panic rather than ship a schedule whose input is empty — a
		// rule that fires every 2 minutes and 400s reads as "the
		// backup component is broken".
		panic("sc-component-backup: schedulerTickRequest: " + err.Error())
	}

	return pApi.DefaultsSentinel + string(b)
}

func (p *backupPlugin) Schema(_ context.Context, _ *scpluginv1.SchemaRequest) (*scpluginv1.SchemaResponse, error) {
	return &scpluginv1.SchemaResponse{
		PluginVersion:         "sc-component-backup-" + version,
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
				// Where to publish operator-relevant events, and the
				// credential to present. Both must be allowlisted or
				// SC core's dual-use of this list as
				// allowed-top-level-keys rejects the whole stack.
				"notify", "notifyToken",
				// Compute-shape overrides for the dispatched aws-lambda
				// template. The default Lambda memory (128 MB) OOMs any
				// mongodump bigger than a handful of MB on the in-process
				// scheduler.RunOnce path (requestId 06a3aa57 on
				// backup.simple-forge.com, forge-notifier adoption
				// 2026-11-18). Design doc §P1 v1.5 names "memory/timeout/
				// vpc/iam" as the operator-wins compute shape; these two
				// keys are the half that was never landed on the allowlist.
				// Follow-up: route Run Now through Fargate so Lambda
				// memory stops mattering.
				"maxMemory", "timeout",
				// Grandfather for the aws-lambda template's
				// per-Component resource naming (sc-plugin-aws#28). This
				// component was deployed before that scoping existed, so
				// its live resources are named from the parent stack
				// alone; switching it over is a pure rename, and Pulumi
				// matches by name, so the plan is an independent create
				// plus a DELETE of each live resource — including the
				// Lambda serving backup.simple-forge.com. SC core's
				// destructive-rename guard refuses that plan, correctly
				// (forge run 44466834062, 14 resources).
				//
				// Declaring `legacyResourceNames: true` keeps this
				// component's names as they are; a second Component on
				// the same parent stack gets scoped names and no longer
				// collides with it. Here only because SC core dual-uses
				// this list as allowed-top-level-keys for the component
				// config — the key is read by the TEMPLATE plugin, not
				// by this one.
				//
				// Dropping it later is a deliberate choice to take the
				// replace: new Function URL (SC core rewires the
				// hostname in the same update) and a fresh log group.
				// Nothing stateful moves — the archives and the tenant
				// registry live in the S3 bucket, a separate resource.
				"legacyResourceNames",
				// The `op: "scheduler"` token the EventBridge rule
				// presents on the tick. SC core dual-uses this list as
				// the allowed-top-level-keys validator, so without the
				// entry the whole stack is refused at parse time with
				// `unknown or disallowed top-level key "schedulerToken"`
				// — live-caught on forge preview run 44514355119.
				"schedulerToken",
				// The bearer the one-shot external-Mongo → go-embedb
				// migration endpoint requires. Paired with the
				// `migrationToken → ADMIN_MIGRATION_TOKEN` entry in
				// ComponentSecretEnvMap below so a `${secret:...}` value
				// here reaches the dispatched Lambda as a SecretsManager
				// ARN (per-function secret, not plaintext env). Without
				// this allowlist entry SC core refuses the stack at parse
				// time with `unknown or disallowed top-level key
				// "migrationToken"`. Unmount semantics: the service only
				// wires `/admin/migrate-mongo-to-goembedb` when the env
				// var is non-empty — dropping the key from server.yaml
				// (not just rotating it) tears the route down.
				"migrationToken",
				// Which consumer environments may register as tenants.
				// Empty/absent allows all — the pre-filter behaviour.
				"tenantEnvironments",
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
				// Paired with the `migrationToken` allowlist entry above.
				// Resolution of the ARN happens in the service at startup
				// — see service/cmd/backup/main.go, next to the
				// SERVICE_JWT_SECRET + BACKUP_OPERATOR_PASSWORD resolution.
				"migrationToken": "ADMIN_MIGRATION_TOKEN",
				// The publisher's event token: an `op:"event"` JWT the
				// notifier minted for this tenant. Routed through the
				// secret-env map rather than ComponentDefaults env so
				// it lands in a per-function SecretsManager secret
				// instead of plaintext in the Lambda's environment.
				//
				// The notifier's own signing key is deliberately NOT
				// handed to publishers: it is also the KEK that seals
				// every tenant's channel credentials at rest, so a
				// publisher holding it could decrypt every stored
				// Telegram token and mint a token for any tenant.
				"notifyToken": "NOTIFY_TOKEN",
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

	// Decode ComponentRaw — the OPERATOR's raw, PRE-ComponentDefaults.
	// api says so in as many words at `plugin_sidecars.go:1869`: "this
	// emits the pre-ComponentDefaults operator Raw. Plugins that need
	// to see the fully-merged Raw can call `pApi.DeepMergeComponentRaw`
	// themselves". (This comment used to claim the opposite, which is
	// why the `region` fallback below hardcodes a value instead of
	// trusting the `$$sc:${template.region}` default — the default is
	// not here yet, and would still be an uninterpolated sentinel if it
	// were.) Anything derived here must reproduce the default rather
	// than read it; see [backup.DefaultBucketName].
	//
	// A non-lambda deployTo is a valid shape (vm-compose, k8s-run);
	// we just have nothing to look up.
	var rawMap map[string]any
	if raw := req.GetComponentRaw(); len(raw) > 0 {
		if jerr := json.Unmarshal(raw, &rawMap); jerr != nil {
			return nil, errors.Wrap(jerr, "decode ComponentRaw")
		}
	}
	// The `notify:` wiring is resolved FIRST and fails the provision
	// when it cannot be, rather than deploying with notifications
	// quietly off. An operator who wrote a `notify:` block believes
	// they are covered; the first thing they would otherwise learn is
	// that a backup failed and nobody was paged. Absent `notify:` is
	// not an error — see backup.ValidateNotifyWiring.
	notify, nerr := backupInternal.ValidateNotifyWiring(rawMap)
	if nerr != nil {
		return nil, errors.Wrapf(nerr, "backup component %q", req.GetResourceName())
	}

	// A non-lambda deployTo (vm-compose, k8s-run) has no default-VPC
	// lookup to do, but it still needs its tpl extensions landed:
	// `notify-url` is referenced from ComponentDefaults env on EVERY
	// deployTo, and an extension that never lands leaves the literal
	// placeholder in the service's environment. This used to be a bare
	// early return, which is why the flag exists rather than a second
	// exit.
	skipVPCLookup := false
	dt, _ := rawMap["deployTo"].(string)
	if dt != "lambda" {
		p.progress.Info("pre-provision", fmt.Sprintf("backup %q deployTo=%q — no VPC lookup needed, skipping", req.GetResourceName(), dt))
		skipVPCLookup = true
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
				// Where this component POSTs operator-relevant
				// events. Empty when no `notify:` block is declared,
				// which the service reads as "notifications off" —
				// the same contract as an absent env.
				"notify-url": notify.URL,
				// The EFFECTIVE archive bucket, which is also where
				// go-embedb keeps its log + snapshots. Derived from the
				// operator's `storage:` block across all three shapes it
				// may take, so `env.GOEMBEDB_S3_BUCKET` can be a plugin
				// default instead of a hand-copied line that disagrees
				// with the bucket the template creates.
				"storage-bucket": backupInternal.BucketForStorageRef(rawMap, req.GetResourceName(), req.GetHeader().GetEnvironment()),
				// The bearer token the scheduler tick's EventBridge rule
				// presents. Landed as a tpl extension rather than named
				// as a secret in ComponentDefaults, because those
				// defaults ship to every operator and cannot know one
				// stack's secret names. See [schedulerTickRequest] for
				// the sentinel-position constraint that makes the
				// reference in the schedule payload resolve at all.
				"scheduler-token": backupInternal.SchedulerTokenFromConfig(rawMap),
			}

			// Self-URL for the Fargate worker's completion callback —
			// derive from `ingress.hostname` (the operator-declared
			// public hostname for the Component). Without this the
			// deployed Lambda has no way to tell the worker where to
			// POST /api/v1/internal/runs/{id}/complete, and the SPA
			// row never flips from `running` to `succeeded` on a
			// Fargate-dispatched Run Now (startup watchdog catches it
			// eventually, but the UX is wrong).
			if ingress, ok := rawMap["ingress"].(map[string]any); ok {
				if host, ok := ingress["hostname"].(string); ok && host != "" {
					tpl["backup-agent-url"] = "https://" + host
				}
			}

			if skipVPCLookup {
				collector.AddResourceTplExtension(req.GetResourceName(), tpl)
				p.progress.Info("pre-provision", fmt.Sprintf("backup %q derived: %s", req.GetResourceName(), backupInternal.NotifyEnvDescription(notify)))
				return nil
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
			// Default-VPC subnets have an IGW route but no NAT; a
			// Fargate task without a public IP therefore cannot
			// reach CloudWatch Logs (or S3 / ECR / etc.) and dies
			// with "ResourceInitializationError: failed to validate
			// logger args: ... There is a connection issue between
			// the task and Amazon CloudWatch" at start. Public IP
			// is free for ephemeral one-shot tasks and is the right
			// default for the auto-discovered default-VPC shape;
			// operators running inside a private subnet with a NAT
			// + VPC endpoints can override via env.BACKUP_ECS_ASSIGN_PUBLIC_IP=false.
			tpl["assign-public-ip"] = "true"
			collector.AddResourceTplExtension(req.GetResourceName(), tpl)
			p.progress.Info("pre-provision", fmt.Sprintf("backup %q default-VPC: subnets=%s SG=%s assign-public-ip=true; %s", req.GetResourceName(), discovery.SubnetIDsCSV, discovery.SecurityGroupID, backupInternal.NotifyEnvDescription(notify)))
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
	// Environment gate — BEFORE the scoped-user mint and before any
	// registration.
	//
	// An SC parent stack can be cloned into short-lived isolated stacks
	// (the Forge fleet spins one per agent-workflow run). Those blocks
	// inherit the base config with a YAML merge key, and a merge key
	// cannot DELETE an inherited entry — so an isolated deploy inherits
	// `uses: [… my-backup]` with no way to opt out from the YAML side.
	//
	// The cost of letting it through is not cosmetic: the tenant row
	// carries a live Atlas URI, so the scheduler dumps a throwaway
	// database every interval — one Fargate task and one S3 archive per
	// run, for a stack destroyed hours ago. It also mints a scoped
	// Atlas user per isolated deploy, which is a credential nobody
	// reaps.
	//
	// Gated here rather than in the service so the ephemeral deploy
	// never gets a user minted for it either.
	consumerEnv := consumerEnvironment(req, parent)
	if allowed := backupInternal.TenantEnvironmentsFromConfig(componentRaw); !backupInternal.EnvMayRegister(allowed, consumerEnv) {
		p.progress.Info("compute-process", fmt.Sprintf(
			"skipping tenant registration for %q: environment %q is not in tenantEnvironments %v",
			req.GetHeader().GetStackName(), consumerEnv, allowed,
		))
		return &scpluginv1.ComputeProcessResponse{}, nil
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

// consumerEnvironment resolves the environment the CONSUMER is being
// deployed to.
//
// The header is authoritative; ParentInfo.StackEnv covers a dispatch
// shape that leaves it empty, and the stack name's `--<env>` suffix is
// the last resort.
func consumerEnvironment(req *scpluginv1.ComputeProcessRequest, parent *pApi.ParentInfo) string {
	if e := strings.TrimSpace(req.GetHeader().GetEnvironment()); e != "" {
		return e
	}
	if parent != nil {
		if e := strings.TrimSpace(parent.StackEnv); e != "" {
			return e
		}
	}
	if stack := req.GetHeader().GetStackName(); stack != "" {
		if i := strings.LastIndex(stack, "--"); i >= 0 && i+2 < len(stack) {
			return stack[i+2:]
		}
	}
	return ""
}
