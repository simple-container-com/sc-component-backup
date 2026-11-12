// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"github.com/pkg/errors"
	"github.com/simple-container-com/api/pkg/api"
)

// BackupConfig documents the expected `.Raw` keys for a `type: sc-backup`
// resource. **Core does NOT import this struct**; the production `sc`
// binary reaches this plugin only via go-plugin gRPC, so SC core's
// sidecar wrapper (slice 1a) reads these keys off `PluginFieldConfig.Raw`
// directly. The struct exists for:
//
//   - Local tooling / IDE hints when authoring a server.yaml
//   - Test fixtures in the plugin module that need a typed view
//   - Documentation of the on-the-wire shape
//
// See the forge memory
// `interface_assertion_against_plugin_owned_value_fails_in_prod` and
// sc-components-platform design doc §P1 v1.5 for why this is doc-only.
type BackupConfig struct {
	// DeployTo names a top-level template from the parent server.yaml's
	// `templates:` block. SC core's sidecar wrapper (slice 1a) looks this
	// up in `stack.Server.Templates`, merges the component's inputs over
	// the operator's template config (plugin wins on image/env/
	// ingressRoutes; operator wins on compute shape — memory/timeout/
	// vpc/iam/etc.), and dispatches
	// `ProvisionFuncByType[tpl.Type](ctx, stack, merged, params)` on the
	// same engine.
	//
	// Required. Must reference an existing template name in the parent
	// stack. Validated at parse time.
	DeployTo string `json:"deployTo" yaml:"deployTo"`

	// Image is the Dockerhub (or other registry) reference for the
	// backup service container. Required.
	Image string `json:"image" yaml:"image"`

	// SigningKey is a `${secret:...}` reference to a 32+-byte HS256 key
	// shared between the plugin (for minting per-tenant JWTs at
	// ComputeProcess time) and the deployed service (for verifying
	// them). Path A's dispatch injects this as `SERVICE_JWT_SECRET` env
	// on the container.
	//
	// Rotation: edit the secret value in SC's secrets registry +
	// re-provision — both sides pick up the new key on the next run.
	// Required.
	SigningKey string `json:"signingKey" yaml:"signingKey"`

	// Env is an optional map of plain env vars to inject into the
	// deployed container (merged with the template's own env; plugin
	// values win on key collisions). Per the §P1 component_allowed_overrides
	// allowlist.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// Ingress declares the service's public entry point. Routed via the
	// parent's registrar (default: the `default: true` registrar in
	// parent's resources.registrars:).
	Ingress *BackupIngress `json:"ingress,omitempty" yaml:"ingress,omitempty"`

	// api.Credentials embedded inline so a resource can optionally carry
	// a `${auth:...}` reference. Rarely needed for a component (templates
	// carry their own credentials); kept for forward-compat.
	api.Credentials `json:",inline" yaml:",inline"`
}

// BackupIngress models the component's public entry point. Optional — a
// smoke run can disable ingress via `ingress: null` and expose the service
// on a high port with no TLS.
type BackupIngress struct {
	// Hostname is the FQDN the service answers on (e.g.
	// `my-backup.example.com`). Routed via the parent's registrar.
	Hostname string `json:"hostname" yaml:"hostname"`

	// Registrar is an optional override naming a specific registrar from
	// the parent's resources.registrars: block. Defaults to the
	// `default: true` registrar.
	Registrar string `json:"registrar,omitempty" yaml:"registrar,omitempty"`
}

// ProviderType satisfies api.Config.
func (c *BackupConfig) ProviderType() string { return ProviderType }

// Validate performs minimum shape checks. Production SC validates
// against `.Raw` directly in the slice 1a sidecar wrapper (louder +
// earlier at server.yaml READ time); this method is for local tests.
func (c *BackupConfig) Validate() error {
	if c.DeployTo == "" {
		return errors.Errorf("`deployTo` is required for %q (name of a template in the parent server.yaml's templates: block)", ResourceType)
	}
	if c.Image == "" {
		return errors.Errorf("`image` is required for %q (Docker image reference)", ResourceType)
	}
	if c.SigningKey == "" {
		return errors.Errorf("`signingKey` is required for %q (${secret:...} reference to a 32+-byte HS256 key)", ResourceType)
	}
	return nil
}

// ReadBackupConfig is the api.ConfigReaderFunc registered in init.go.
func ReadBackupConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &BackupConfig{})
}
