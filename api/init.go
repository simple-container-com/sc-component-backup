// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package backup is the CONFIG TIER of the sc-component-backup template.
//
// This package is what SC core imports for fool-protection auto-inject
// discovery. It contributes a `ProviderType` discriminator plus a single
// `ResourceType` token — `sc-backup` — which doubles as the
// ComputeProcessorType (one RPC class per resource type).
//
// Unlike sc-plugin-echo, which exports every kind of type token (auth,
// registrar, template, VM driver, state-storage, secrets-provider), an
// SC component exports only ONE resource token. Everything else the
// component needs (the registrar for its DNS record, the deploy template
// for the service container, the auth for the operator cloud) is
// inherited from the PARENT's templates/registrars/auth blocks — the
// operator declares those at the top level of server.yaml and the
// component's config.deployTo names which template to run the service
// into.
//
// IMPORTANT: the production `sc` binary does NOT link this package. SC
// core reaches us only via hashicorp/go-plugin gRPC, and plugin-owned
// values arrive at core as opaque `*api.PluginFieldConfig`. Do NOT define
// Go interfaces with the expectation that core will assert against them
// — see the forge memory `interface_assertion_against_plugin_owned_value_fails_in_prod`
// and the sc-components-platform design doc §P1 v1.5. The dispatch
// mechanism is Schema-advertised component_resource_types +
// `.Raw`-reading on the opaque wrapper, built in the slice 1a api PR.
//
// This /api module exists for:
//  1. init() calls to `api.RegisterProviderConfig` + `api.RegisterPluginRequirement`
//     so SC core's fool-protection auto-inject can map `type: sc-backup`
//     to this plugin.
//  2. Local tooling + test fixtures that want a typed view of the
//     component config (see backup.go — the BackupConfig struct is
//     doc-only; core reads `.Raw` keys directly in production).
package backup

import (
	"github.com/simple-container-com/api/pkg/api"
)

// ProviderType is the top-level discriminator every backup config reports.
const ProviderType = "sc-backup"

// ResourceType is the single owned type token. An operator declares
// `type: sc-backup` in a parent server.yaml resources: block; SC core's
// sidecar wrapper (slice 1a) recognises it from the Schema-advertised
// `component_resource_types` set and dispatches the deployTo template.
const ResourceType = "sc-backup"

// ComputeProcessorType is the same token — one RPC class per resource type.
// When a consumer declares `uses: [my-backup]`, SC core routes the consumer's
// ComputeProcess call to this plugin via the resource type.
const ComputeProcessorType = ResourceType

func init() {
	// Register the config reader so SC core can parse a `type: sc-backup`
	// resource block out of server.yaml. The reader returns the typed
	// struct for local-side use; production SC uses PluginFieldConfig.Raw
	// via the sidecar wrapper (slice 1a).
	api.RegisterProviderConfig(api.ConfigRegisterMap{
		ResourceType: ReadBackupConfig,
	})

	// Fool-protection: SC core's parse-time validator auto-injects
	// `plugins.backup:` with the latest release from GitHub when it finds
	// an un-declared OwnedTypes token in server.yaml. OwnedTypes must list
	// every token this plugin owns.
	api.RegisterPluginRequirement(api.PluginRequirement{
		PluginName: "backup",
		OwnedTypes: []string{
			ProviderType,
			ResourceType,
		},
		DefaultSource: api.PluginSource{
			Type:       "github-release",
			Repository: "simple-container-com/sc-component-backup",
		},
	})
}
