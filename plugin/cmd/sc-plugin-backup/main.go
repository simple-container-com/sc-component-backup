// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// sc-plugin-backup is the TEMPLATE every SC component clones from, AND
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
// forge memory interface_assertion_against_plugin_owned_value_fails_in_prod
// + sc-components-platform design doc §P1 v1.5 for why. Dispatch is
// Schema-advertised component_resource_types + `.Raw` reader, not
// Go-interface assertion.
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
	"fmt"
	"os"
	"sync"

	hclog "github.com/hashicorp/go-hclog"
	hplugin "github.com/hashicorp/go-plugin"
	"github.com/pkg/errors"
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
		ComponentAllowedOverrides: map[string]*scpluginv1.StringList{
			backupApi.ResourceType: {Values: []string{"image", "env", "ingressRoutes", "imagePullSecretRef"}},
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
	input := runtime.ResourceInputForComputeProcess(req, parent, map[string]any{})

	processor := backupInternal.NewProcessor(backupInternal.ProcessorConfig{
		Progress: p.progress,
	})

	resp, _, err = runtime.RunComputeProcess(
		stdctx, rctx,
		api.Stack{Name: req.GetHeader().GetStackName()}, input,
		pApi.ProvisionParams{Log: logger.New(), ParentStack: parent},
		processor.Process,
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
	if err := backupInternal.PreDestroy(ctx, p.progress, req); err != nil {
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
			Name:   "sc-plugin-backup",
			Level:  hclog.Warn,
			Output: os.Stderr,
		}),
	})
}
