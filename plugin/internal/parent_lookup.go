// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"fmt"

	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
)

// Slice 1d.4-redux (2026-11-14): SC core does not populate
// `params.ResourceOutputs` on consumer-side ComputeProcess calls
// (api/pkg/clouds/pulumi/provision.go:373-382 returns `pApi.ProvisionParams`
// without that field set), so the DependencyOutputsJson that api #602/#608
// landed on the proto arrives empty on the plugin side. This file carries
// the fallback helpers that open a `sdk.NewStackReference` to the parent
// and resolve sibling exports synchronously — matching the working pattern
// in sc-plugin-mongodb's `plugin/internal/compute_proc.go` (slice-1d.4
// smoke, 2026-11-14 finding).
//
// Primary path stays the typed `pApi.ResourcesOutputs` lookup — it'll
// light up the day SC core populates that map. The helpers below are the
// fallback: try deps first, fall back to StackReference.

// stackRefName is the Pulumi RESOURCE NAME for the parent-stack reference.
// Matches sc-plugin-mongodb's pattern: `<childStack>--<resourceName>--
// <parentFullRef>--backup-ref`. The `-ref` suffix keeps it distinct from
// the resource's own URN; the include of childStack + resourceName keeps
// the ref unique when a consumer uses multiple component resources.
func stackRefName(childStackName, resourceName, parentFullRef string) string {
	return fmt.Sprintf("%s--%s--%s--backup-ref", childStackName, resourceName, parentFullRef)
}

// newParentStackRef opens a Pulumi StackReference to the component's parent
// stack. Nil ParentStack is a programmer error (SC core always carries it
// on consumer-side ComputeProcess for a parent-less deploy of a Component
// consumer makes no sense) — error fast rather than silently skip the
// sibling lookup.
func newParentStackRef(ctx *sdk.Context, childStackName, resourceName string, parent *pApi.ParentInfo) (*sdk.StackReference, error) {
	if parent == nil || parent.FullReference == "" {
		return nil, errors.New("parent stack info missing — cannot open StackReference for sibling lookup")
	}
	return sdk.NewStackReference(ctx, stackRefName(childStackName, resourceName, parent.FullReference),
		&sdk.StackReferenceArgs{
			Name: sdk.String(parent.FullReference).ToStringOutput(),
		})
}

// parentStringOutput reads one export off the parent stack reference as a
// string, mirroring sc-plugin-mongodb's `parentStringOutput` (plugin/
// internal/compute_proc.go). Returns (value, found, err):
//   - found=false, err=nil when the parent published no output under the key.
//   - found=true, err≠nil when the output exists but is not a string.
//
// SECRET VALUES: GetOutputDetails returns the resolved value even when the
// parent exported it as a secret. The caller is responsible for not logging
// a value its own key hints is sensitive (mongo-uri, atlas password).
func parentStringOutput(ref *sdk.StackReference, exportName string) (string, bool, error) {
	details, err := ref.GetOutputDetails(exportName)
	if err != nil {
		return "", false, errors.Wrapf(err, "GetOutputDetails(%q)", exportName)
	}
	value := details.Value
	if value == nil {
		value = details.SecretValue
	}
	if value == nil {
		return "", false, nil
	}
	s, ok := value.(string)
	if !ok {
		// Never leak the value in the error: a secret may be inside.
		return "", true, errors.Errorf("parent output %q resolved to %T, not string", exportName, value)
	}
	return s, true, nil
}
