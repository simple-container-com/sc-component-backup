// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import "strings"

// DefaultBucketName is the archive bucket an operator gets when they
// declare no `storage:` block at all.
//
// It MUST stay byte-identical to the `storage.bucket` entry in
// componentDefaultsJSON (`$$sc:sc-backup-${name}-archives--${env}`).
// The duplication is forced, not sloppy: PreProvision receives the
// operator's raw config BEFORE ComponentDefaults is merged onto it
// (api `pkg/clouds/pulumi/plugin_sidecars.go:1869`, "this emits the
// pre-ComponentDefaults operator Raw"), so a tpl extension derived
// here cannot read the default and has to reproduce it. Change one,
// change both — a drift renames the bucket the service writes to
// while leaving the bucket the template creates where it was.
func DefaultBucketName(resourceName, env string) string {
	return scResourceBucketName("sc-backup-"+resourceName+"-archives", env)
}

// BucketForStorageRef resolves the EFFECTIVE archive bucket from the
// operator's raw `storage:` declaration, across the three shapes SC
// accepts: a bare string naming an SC s3-bucket resource, a map with
// an explicit `bucket:`, or a map naming a resource. Falls back to
// [DefaultBucketName].
//
// It exists so `env.GOEMBEDB_S3_BUCKET` can be a plugin default
// (`$$sc:${resource:${name}.storage-bucket}`) rather than a line every
// operator copies by hand — and copies wrong, because the two shapes
// do NOT yield the same name: a bare `storage: my-data` becomes
// `my-data--<env>`, while `storage: {bucket: my-data}` is taken
// verbatim. An operator who writes the bare form and then repeats the
// base name under `env.GOEMBEDB_S3_BUCKET` points the metadata store
// at a bucket that does not exist.
func BucketForStorageRef(raw map[string]any, resourceName, env string) string {
	switch st := raw["storage"].(type) {
	case string:
		if name := strings.TrimSpace(st); name != "" {
			return scResourceBucketName(name, env)
		}
	case map[string]any:
		if b, ok := st["bucket"].(string); ok && strings.TrimSpace(b) != "" {
			return strings.TrimSpace(b)
		}
		if n, ok := st["name"].(string); ok && strings.TrimSpace(n) != "" {
			return scResourceBucketName(strings.TrimSpace(n), env)
		}
	}

	return DefaultBucketName(resourceName, env)
}

// SchedulerTokenFromConfig reads the operator-declared bearer token
// the deploy template's EventBridge timer presents on the scheduler
// tick.
//
// It is landed as a tpl extension rather than named as a secret inside
// ComponentDefaults because those defaults ship to every operator and
// must not assume one stack's secret names: the component cannot write
// `${secret:sc-backup-scheduler-token}`, only the operator can.
func SchedulerTokenFromConfig(raw map[string]any) string {
	if v, ok := raw["schedulerToken"].(string); ok {
		return strings.TrimSpace(v)
	}

	return ""
}

// scResourceBucketName mirrors the s3-bucket resource's own naming:
// `<resourceName>--<env>`.
//
// The double hyphen is SC's resource-name/environment separator and is
// not cosmetic — a single one would collide with a resource name that
// itself ends in `-staging`.
func scResourceBucketName(resourceName, env string) string {
	if env == "" {
		return resourceName
	}

	return resourceName + "--" + env
}
