// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// resolveAWSSecretARN returns the plaintext SecretString when `raw`
// is an `arn:aws:secretsmanager:*` ARN, else returns `raw` verbatim.
//
// Why: sc-plugin-aws wires `cfg.Secrets[*]` entries onto the Lambda's
// environment as SecretsManager ARNs (the fleet convention — plaintext
// never lands in Lambda env). The Lambda runtime does NOT dereference
// those ARNs; `os.Getenv("SERVICE_JWT_SECRET")` returns the ARN string.
//
// Two paths depend on the RESOLVED value:
//   - The plugin's ComputeProcessor mints a JWT with the resolved
//     signingKey at provision time, and the service MUST validate with
//     the same resolved value or every push-register call is 401. Live-
//     caught 2026-11-14 on backup-smoke-1d: plugin signs with the 64-
//     char hex, service validates with the 115-char ARN → mismatch.
//   - The service's own session-cookie JWT is self-consistent (signs +
//     validates with whatever the env holds) — breaking it is NOT the
//     direct symptom, but every cross-process JWT from an operator / CI
//     smoke that minted with the resolved value also hits 401.
//
// Resolution happens once at startup; failures exit 2 so a misconfigured
// deploy surfaces the mistake rather than running with the ARN-as-key
// and 401-looping.
func resolveAWSSecretARN(ctx context.Context, raw string) (string, error) {
	if !strings.HasPrefix(raw, "arn:aws:secretsmanager:") {
		return raw, nil
	}
	slog.Info("resolving SERVICE_JWT_SECRET from SecretsManager ARN")
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", err
	}
	sm := secretsmanager.NewFromConfig(cfg)
	out, err := sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &raw})
	if err != nil {
		return "", err
	}
	if out.SecretString != nil {
		return *out.SecretString, nil
	}
	if len(out.SecretBinary) > 0 {
		return string(out.SecretBinary), nil
	}
	return "", nil
}
