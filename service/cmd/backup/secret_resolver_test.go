// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"testing"
	"time"
)

// TestResolveAWSSecretARN_NonARNPassesThrough asserts the fallback branch
// — a plaintext token (or any non-ARN string) is returned verbatim with
// no SDK call. This is what makes operator-supplied plaintext values
// still work when the plugin's ComponentSecretEnvMap wire is bypassed
// (e.g. an operator setting `env.ADMIN_MIGRATION_TOKEN: "raw"` instead
// of `migrationToken: ${secret:…}`).
func TestResolveAWSSecretARN_NonARNPassesThrough(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name, raw string
	}{
		{"plaintext", "plaintext-token-value"},
		{"empty", ""},
		{"arn-like-but-wrong-prefix", "arn:aws:iam::123:role/Foo"},
		{"arn-prefix-with-missing-colon", "arn:aws:secretsmanager"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAWSSecretARN(ctx, "ADMIN_MIGRATION_TOKEN", tc.raw)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.raw {
				t.Errorf("passthrough failed: got %q, want %q", got, tc.raw)
			}
		})
	}
}

// TestResolveAWSSecretARN_NonARNPassesThrough_InversionCanFail inverts
// the assertion once: a string that WAS an ARN but has been altered to
// drop the SecretsManager marker must passthrough — a passthrough on an
// actual ARN would be a bug, and the test above would hide it. Here we
// verify the inversion by checking that the assertion CAN fail: given
// an actual ARN, the function must NOT passthrough.
func TestResolveAWSSecretARN_NonARNPassesThrough_InversionCanFail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bogus := "arn:aws:secretsmanager:eu-central-1:000000000000:secret:does-not-exist-XX"
	got, err := resolveAWSSecretARN(ctx, "ADMIN_MIGRATION_TOKEN", bogus)
	// Must NOT return the ARN unchanged with no error — that is the
	// exact bug the ARN plumbing exists to prevent (gate compares
	// bearer against ARN string, no token matches). Either an error
	// surfaces (SDK can't reach SM / no creds / bogus secret) OR a
	// resolved value comes back — never the ARN itself with nil err.
	if err == nil && got == bogus {
		t.Fatalf("ARN passed through unchanged → SM branch bypassed")
	}
}

// TestResolveAWSSecretARN_ARNEntersSMBranch asserts the ARN-shaped input
// enters the GetSecretValue branch (verified by the fact that we get an
// error in a unit-test env without AWS credentials, rather than the raw
// ARN back). This is the invariant that makes the migration gate work
// after the plugin binds a `${secret:…}` config value as a SecretsManager
// ARN.
func TestResolveAWSSecretARN_ARNEntersSMBranch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bogus := "arn:aws:secretsmanager:eu-central-1:000000000000:secret:does-not-exist-XX"
	got, err := resolveAWSSecretARN(ctx, "ADMIN_MIGRATION_TOKEN", bogus)
	if err == nil {
		t.Fatalf("want error from GetSecretValue on bogus ARN (unit env has no SM reach), got %q", got)
	}
}

// TestResolveAWSSecretARN_ARNEntersSMBranch_InversionCanFail: a non-ARN
// input must NOT reach the SDK branch — if it did, every non-ARN call
// would fail in the unit env, including the plaintext-passthrough path
// that operators rely on for pre-wire deployments.
func TestResolveAWSSecretARN_ARNEntersSMBranch_InversionCanFail(t *testing.T) {
	got, err := resolveAWSSecretARN(context.Background(), "ADMIN_MIGRATION_TOKEN", "not-an-arn")
	if err != nil {
		t.Fatalf("non-ARN input must NOT touch the SDK branch, got err: %v", err)
	}
	if got != "not-an-arn" {
		t.Errorf("non-ARN passthrough broken: got %q", got)
	}
}
