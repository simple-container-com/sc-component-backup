// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
)

// Retry policy constants — per sc-components-platform design doc v1.3 §P3
// push-model operational contract. DO NOT lower the attempt count or the
// timeout without updating the design doc and reasoning about how
// consumer deploys behave when the Backup service is cold / rolling /
// 5xx-ing.
const (
	maxAttempts        = 3
	attemptTimeout     = 10 * time.Second
	jwtLifetime        = 60 * time.Second
	userAgent          = "sc-component-backup/0.0.1 (+https://github.com/simple-container-com/sc-component-backup)"
	signingKeyEnvVar   = "BACKUP_SIGNING_KEY" // on plugin side, from Provision inputs
	backupAgentURLVar  = "BACKUP_AGENT_URL"   // injected into consumer's env (plain)
	backupTokenVar     = "BACKUP_TOKEN"       // injected into consumer's env (secret, per-consumer)
	tenantsRegisterURL = "/api/v1/tenants/register"
)

// Backoff schedule, in seconds. Jitter ±25% applied per attempt.
var backoffSeconds = []int{1, 2, 4}

// ProcessorConfig carries per-plugin wiring into the ComputeProcessor fn.
// Progress is required; HTTPClient defaults to a sensible one with
// attemptTimeout per attempt (not the whole chain).
type ProcessorConfig struct {
	Progress   *scPlugin.Publisher
	HTTPClient *http.Client // nil → use default
}

// Processor is the ComputeProcessor for a `uses: [my-backup]` consumer.
type Processor struct {
	cfg ProcessorConfig
}

func NewProcessor(cfg ProcessorConfig) *Processor {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: attemptTimeout}
	}
	return &Processor{cfg: cfg}
}

// Process is the push-register call.
//
//   - Reads the deployed service URL from the dependency outputs (the
//     `_deployed_service` key SC core's sidecar wrapper populates in
//     slice 1a; or a `<my-backup>-service-url` export from Provision).
//   - Mints a short-TTL HS256 JWT with the consumer stack as `tenant`.
//   - POSTs a tenant record to {service-url}/api/v1/tenants/register,
//     with the retry policy from design doc §P3.
//   - Injects BACKUP_AGENT_URL + BACKUP_TOKEN env into the consumer.
//
// The signing key is read from the plugin's own Provision inputs
// (`signingKey`); for the backup's purposes we expect the test harness to
// pass it via the DependencyOutputsJson or an env var in the plugin
// binary's process.
func (p *Processor) Process(ctx *sdk.Context, _ api.Stack, input api.ResourceInput, collector pApi.ComputeContextCollector, _ pApi.ProvisionParams) (*api.ResourceOutput, error) {
	resName := input.Descriptor.Name
	resType := input.Descriptor.Type
	stackName := input.StackParams.StackName

	serviceURL, err := p.readServiceURL(input)
	if err != nil {
		return nil, errors.Wrapf(err, "component %q unable to resolve service URL", resName)
	}
	signingKey, err := p.readSigningKey(input)
	if err != nil {
		return nil, errors.Wrapf(err, "component %q unable to resolve signingKey", resName)
	}

	tenantToken, err := mintJWT(signingKey, stackName, jwtLifetime)
	if err != nil {
		return nil, errors.Wrap(err, "mint per-consumer JWT")
	}

	record := TenantRecord{
		StackName: stackName,
		Version:   deriveConsumerVersion(input),
		Resources: readConsumerResources(input),
	}

	if err := p.pushRegister(ctx.Context(), serviceURL, tenantToken, record); err != nil {
		// The consumer's deploy fails, with a diagnosable error naming
		// the component as the blocker (per §P3 non-silent-skip rule).
		return nil, errors.Wrapf(err,
			"component %q unreachable after %d attempts (check the component service is live at %s; or remove the component from client.yaml uses: if intentionally torn down)",
			resName, maxAttempts, serviceURL)
	}

	// On success, inject BACKUP_AGENT_URL (plain) + BACKUP_TOKEN (secret,
	// per-consumer short-TTL JWT) into the consumer's compute env.
	collector.AddEnvVariableIfNotExist(backupAgentURLVar, serviceURL, resType, resName, stackName)
	collector.AddSecretEnvVariableIfNotExist(backupTokenVar, tenantToken, resType, resName, stackName)

	if p.cfg.Progress != nil {
		p.cfg.Progress.Info("compute-process", fmt.Sprintf("registered tenant %q at %s", stackName, serviceURL))
	}

	return &api.ResourceOutput{Ref: sdk.String(fmt.Sprintf("backup-compute:%s:%s", resType, resName)).ToStringOutput()}, nil
}

// pushRegister executes the retry loop. Returns nil on first successful
// attempt (2xx); wrapped error on all-retries-exhausted. Each attempt uses
// its own context.WithTimeout so the overall wall time is bounded.
func (p *Processor) pushRegister(ctx context.Context, serviceURL, token string, record TenantRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return errors.Wrap(err, "marshal tenant record")
	}

	target := strings.TrimRight(serviceURL, "/") + tenantsRegisterURL
	var lastErr error
	var lastStatus int
	var lastBody string

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay := jitteredBackoff(backoffSeconds[attempt-2])
			if p.cfg.Progress != nil {
				p.cfg.Progress.Info("compute-process", fmt.Sprintf("register attempt %d/%d in %s", attempt, maxAttempts, delay))
			}
			select {
			case <-ctx.Done():
				return errors.Wrap(ctx.Err(), "retry loop cancelled")
			case <-time.After(delay):
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		status, respBody, err := doPost(attemptCtx, p.cfg.HTTPClient, target, token, body)
		cancel()

		if err == nil && status >= 200 && status < 300 {
			if p.cfg.Progress != nil {
				p.cfg.Progress.Info("compute-process", fmt.Sprintf("register attempt %d/%d status=%d (ok)", attempt, maxAttempts, status))
			}
			return nil
		}
		lastStatus = status
		lastBody = truncate(respBody, 256)
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.Errorf("http %d: %s", status, lastBody)
		}
		if p.cfg.Progress != nil {
			p.cfg.Progress.Error("compute-process", fmt.Sprintf("register attempt %d/%d status=%d err=%v", attempt, maxAttempts, status, lastErr))
		}

		// Non-retryable: 4xx except 429. See design doc §P3.
		if status >= 400 && status < 500 && status != 429 {
			return errors.Wrapf(lastErr, "non-retryable http %d from %s", status, target)
		}
	}

	return errors.Wrapf(lastErr, "all %d attempts failed (last status=%d, last body=%q)", maxAttempts, lastStatus, lastBody)
}

func doPost(ctx context.Context, client *http.Client, url, token string, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody), nil
}

// jitteredBackoff returns `base ± 25%` as a time.Duration. When baseSeconds
// is 0 (e.g. the first-attempt backoff on the push-register loop), returns
// 0 immediately — `rand.Int(_, 0)` panics with "argument to Int is <= 0".
func jitteredBackoff(baseSeconds int) time.Duration {
	if baseSeconds <= 0 {
		return 0
	}
	// jitter in [-250ms, +250ms] per second of base, i.e. ±25%.
	base := time.Duration(baseSeconds) * time.Second
	jitterRange := base / 2 // ±25% = range of 50%
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(jitterRange)))
	if err != nil {
		return base
	}
	return base - (jitterRange / 2) + time.Duration(jitter.Int64())
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// readServiceURL reads the deployed service URL. In slice 1a's Path A,
// SC core populates this via DependencyOutputsJson._deployed_service.
// For the pre-slice-1a smoke, the plugin's test harness passes it via
// the resource's own Config.Raw["_test_service_url"] override.
func (p *Processor) readServiceURL(input api.ResourceInput) (string, error) {
	// TODO slice 1a: once SC core's sidecar wrapper populates
	// DependencyOutputsJson._deployed_service, read from there.
	// For now, read from the plugin's own Config.Raw["_test_service_url"]
	// — a test-only override.
	if raw, ok := extractConfigRaw(input.Descriptor); ok {
		if v, ok := raw["_test_service_url"].(string); ok && v != "" {
			return v, nil
		}
	}
	return "", errors.New("service URL not in DependencyOutputsJson._deployed_service (slice 1a not yet landed) and no _test_service_url override on Config.Raw")
}

func (p *Processor) readSigningKey(input api.ResourceInput) (string, error) {
	if raw, ok := extractConfigRaw(input.Descriptor); ok {
		if v, ok := raw["signingKey"].(string); ok && v != "" {
			return v, nil
		}
	}
	return "", errors.New("signingKey not found on Config.Raw — operator must declare config.signingKey: ${secret:...}")
}

// extractConfigRaw reads the opaque PluginFieldConfig.Raw map off a
// resource descriptor. Returns nil + false if the shape is unexpected
// (shouldn't happen in production — slice 1a's parse-time validation
// catches it earlier — but we guard defensively here).
func extractConfigRaw(desc *api.ResourceDescriptor) (map[string]any, bool) {
	// desc.Config (embedded) carries .Config as `any`. In production,
	// that's *api.PluginFieldConfig with .Raw populated.
	if desc == nil || desc.Config.Config == nil {
		return nil, false
	}
	type rawHaver interface {
		GetRaw() map[string]any
	}
	if rh, ok := desc.Config.Config.(rawHaver); ok {
		return rh.GetRaw(), true
	}
	// Direct access on *api.PluginFieldConfig via reflection-free field read.
	type rawField interface {
		GetRaw() map[string]any
	}
	_ = rawField(nil)
	// Last-resort: check for a plain map[string]any (tests, dev).
	if m, ok := desc.Config.Config.(map[string]any); ok {
		return m, true
	}
	return nil, false
}

// deriveConsumerVersion picks a stable version string for idempotency.
// In production, this is the consumer's deployed image version;
// pre-slice-1a-wired, we use the resource name as the version marker.
func deriveConsumerVersion(input api.ResourceInput) string {
	return input.Descriptor.Name
}

// readConsumerResources enumerates the consumer's sibling resources
// relevant to a real component (DBs, S3, etc.). For the backup, we just
// return an empty list — real components (Backup, Monitoring, …) will
// populate this from DependencyOutputsJson.
func readConsumerResources(_ api.ResourceInput) []TenantResource {
	return []TenantResource{}
}

// mintJWT creates a short-TTL HS256 JWT for the tenant record. Signing
// key is the operator's `config.signingKey` secret value, the same key
// the service was deployed with (as SERVICE_JWT_SECRET env).
func mintJWT(signingKey, tenant string, lifetime time.Duration) (string, error) {
	now := time.Now().UTC()
	claims := jwt.MapClaims{
		"op":     "register",
		"tenant": tenant,
		"iat":    now.Unix(),
		"exp":    now.Add(lifetime).Unix(),
		"nbf":    now.Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(signingKey))
}

// TenantRecord is the request body shape for POST /api/v1/tenants/register.
type TenantRecord struct {
	StackName string           `json:"stackName"`
	Version   string           `json:"version"`
	Resources []TenantResource `json:"resources"`
}

// TenantResource describes one sibling resource the consumer has
// (DB, S3, etc.). Backup v1 has no resources; real components populate
// from DependencyOutputsJson via the §P3 canonical `connection.*` family.
type TenantResource struct {
	Type       string            `json:"type"`
	Name       string            `json:"name"`
	Connection map[string]string `json:"connection,omitempty"`
}

// ReadDeployedServiceURL reads the Path A sidecar wrapper's populated
// `_deployed_service` key off DependencyOutputsJson. Returns empty + false
// if the key is absent (pre-slice-1a SC core).
func ReadDeployedServiceURL(depOutputsJSON []byte) (string, bool) {
	if len(depOutputsJSON) == 0 {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(depOutputsJSON, &m); err != nil {
		return "", false
	}
	if v, ok := m["_deployed_service"].(string); ok && v != "" {
		return v, true
	}
	// Alternate key the dispatched template might populate:
	if ds, ok := m["_deployed_service"].(map[string]any); ok {
		if v, ok := ds["url"].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}
