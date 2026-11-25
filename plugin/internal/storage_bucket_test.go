// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import "testing"

// The three shapes an operator may write, and the one thing that makes
// this function necessary rather than decorative: a bare
// `storage: my-data` grows a `--<env>` suffix while
// `storage: {bucket: my-data}` is taken verbatim. An operator who
// writes the bare form and then repeats the base name under
// `env.GOEMBEDB_S3_BUCKET` points the metadata store at a bucket that
// does not exist — which is why that env var is now a plugin default
// derived from here instead of a line in server.yaml.
func TestBucketForStorageRef(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  map[string]any
		want string
	}{
		"bare string grows the env suffix": {
			raw:  map[string]any{"storage": "sc-notifier-data"},
			want: "sc-notifier-data--staging",
		},
		"explicit bucket is verbatim": {
			raw:  map[string]any{"storage": map[string]any{"type": "s3", "bucket": "backup-archives--staging"}},
			want: "backup-archives--staging",
		},
		"explicit bucket wins over name": {
			raw:  map[string]any{"storage": map[string]any{"bucket": "chosen", "name": "ignored"}},
			want: "chosen",
		},
		"resource name grows the env suffix": {
			raw:  map[string]any{"storage": map[string]any{"name": "my-data"}},
			want: "my-data--staging",
		},
		"whitespace-only bucket falls through": {
			raw:  map[string]any{"storage": map[string]any{"bucket": "   "}},
			want: "sc-backup-my-backup-archives--staging",
		},
		"no storage block at all": {
			raw:  map[string]any{},
			want: "sc-backup-my-backup-archives--staging",
		},
		"storage of an unexpected type": {
			raw:  map[string]any{"storage": 42},
			want: "sc-backup-my-backup-archives--staging",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := BucketForStorageRef(tc.raw, "my-backup", "staging"); got != tc.want {
				t.Fatalf("BucketForStorageRef = %q, want %q", got, tc.want)
			}
		})
	}
}

// THE regression test for the forced duplication. PreProvision receives
// the operator's raw config BEFORE ComponentDefaults is merged onto it
// (api `plugin_sidecars.go:1869`), so the fallback here cannot read the
// `storage.bucket` default and has to reproduce it. If the two drift,
// the service writes its metadata to one bucket while the template
// creates another, and nothing in either config says so.
//
// The literal below is the default's own interpolated form:
// `$$sc:sc-backup-${name}-archives--${env}`.
func TestDefaultBucketName_MirrorsTheComponentDefault(t *testing.T) {
	if got, want := DefaultBucketName("my-backup", "staging"), "sc-backup-my-backup-archives--staging"; got != want {
		t.Fatalf("DefaultBucketName = %q, want %q — this MUST equal the interpolation of the `storage.bucket` entry in componentDefaultsJSON", got, want)
	}
	// An empty env is the parent-stack shape; no trailing separator.
	if got, want := DefaultBucketName("my-backup", ""), "sc-backup-my-backup-archives"; got != want {
		t.Fatalf("DefaultBucketName with no env = %q, want %q", got, want)
	}
}

func TestSchedulerTokenFromConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  map[string]any
		want string
	}{
		"declared":      {raw: map[string]any{"schedulerToken": "tok"}, want: "tok"},
		"trimmed":       {raw: map[string]any{"schedulerToken": "  tok\n"}, want: "tok"},
		"absent":        {raw: map[string]any{}, want: ""},
		"wrong type":    {raw: map[string]any{"schedulerToken": 7}, want: ""},
		"nil raw entry": {raw: map[string]any{"schedulerToken": nil}, want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := SchedulerTokenFromConfig(tc.raw); got != tc.want {
				t.Fatalf("SchedulerTokenFromConfig = %q, want %q", got, tc.want)
			}
		})
	}
}
