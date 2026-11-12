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

	// Databases declares which operator-side DB resources this backup
	// component is authorised to back up. When a consumer `uses:` BOTH
	// this backup component AND one of the listed DB resources, the
	// plugin's ComputeProcess mints a scoped DB user on the fly (via
	// AtlasAdmin creds below for mongodb), push-registers the scoped
	// creds to the service, and the service dumps the consumer's DB
	// using that scoped user.
	//
	// Slice 1d expansion of the Component class (sc-components-platform
	// design doc §5.2). Each entry names the SC resource name + its
	// driver; driver determines which admin-REST backend the plugin
	// calls (atlas-admin REST for mongodb; aws-rds for rds-*; etc.).
	Databases []BackupDatabase `json:"databases,omitempty" yaml:"databases,omitempty"`

	// AtlasAdmin is the operator-side admin credential sc-plugin-backup
	// uses to call Atlas REST (CreateDatabaseUser / Delete) when
	// provisioning scoped users for Mongo-backed Databases entries.
	// Required iff any Databases entry has driver == "mongodb".
	//
	// These are PRIVILEGED creds — the plugin stores them in the Lambda
	// env under BACKUP_ATLAS_ADMIN_PRIVATE_KEY + _PUBLIC_KEY and only
	// uses them during the Component's own provision + each consumer's
	// ComputeProcess. The scoped users the plugin mints have
	// `readWrite` on the consumer's specific DB only — the admin cred
	// itself never leaves the backup Lambda's env.
	AtlasAdmin *BackupAtlasAdmin `json:"atlasAdmin,omitempty" yaml:"atlasAdmin,omitempty"`

	// Worker shapes the ECS Fargate one-shot task that runs the actual
	// mongodump/pg_dump when the Component is deployed to aws-lambda.
	// Per design doc §5.3 — Lambda's 15-min cap means long-running dumps
	// can't live in the Lambda itself; they run in a Fargate task fired
	// by the Lambda's scheduler tick via `ecs.RunTask`.
	//
	// ONLY consulted when `deployTo: lambda`. Ignored for ecs-fargate /
	// vm-compose / k8s deploys — those run long-lived and dump in-process,
	// and the Worker block has no effect there.
	Worker *BackupWorker `json:"worker,omitempty" yaml:"worker,omitempty"`

	// api.Credentials embedded inline so a resource can optionally carry
	// a `${auth:...}` reference. Rarely needed for a component (templates
	// carry their own credentials); kept for forward-compat.
	api.Credentials `json:",inline" yaml:",inline"`
}

// BackupDatabase names an operator-declared DB resource this backup component
// is authorised to back up. The plugin's ComputeProcess matches the consumer's
// `uses:` list against the list of declared `Resource` names and mints a
// scoped user for each match.
type BackupDatabase struct {
	// Resource is the SC resource name as declared under the parent
	// server.yaml's resources.resources.<env>.resources block (e.g. "mongodb"
	// for the main cluster). Required.
	Resource string `json:"resource" yaml:"resource"`

	// Driver is the DB flavour: "mongodb" | "postgres" | "mysql". Required.
	// Determines which admin-REST client sc-plugin-backup uses to mint the
	// scoped user.
	Driver string `json:"driver" yaml:"driver"`
}

// BackupWorker shapes the ECS Fargate one-shot task that runs the actual
// mongodump / pg_dump / mariadb-dump when this Component is deployed to
// aws-lambda. Per design doc §5.3.
//
// All fields are operator-declared. The plugin's Provision provisions an
// `aws:ecs/taskDefinition:TaskDefinition` using these knobs (image is the
// same as the Lambda's — one Dockerhub image, mode flag at entry).
// The cluster + subnets + security groups are operator-declared (not
// provisioned by this plugin) so the operator keeps full control over the
// VPC + routing posture for the backup worker. Inside the Fargate task, the
// backup service entrypoint reads `BACKUP_MODE=worker` + the per-tenant env
// the Lambda injected via `ecs.RunTask` and dumps to S3.
type BackupWorker struct {
	// CPU is the Fargate vCPU allocation in millicpu steps (256, 512, 1024,
	// 2048, 4096). Must pair with MemoryMB per the Fargate matrix.
	// Default: 512 (0.5 vCPU) — enough for mongodump streaming without
	// paying for compute it won't use.
	CPU int `json:"cpu,omitempty" yaml:"cpu,omitempty"`

	// MemoryMB is the Fargate memory allocation in MB. Default 2048 (2 GB).
	// Oversized dumps use the ephemeral storage for buffering, not RAM.
	MemoryMB int `json:"memoryMB,omitempty" yaml:"memoryMB,omitempty"`

	// EphemeralStorageGB is the task's /tmp allocation (Fargate default 20
	// GB, max 200). Default 50 — matches local-db-backup's observed
	// mongodump spool peak of 2× uncompressed DB size.
	EphemeralStorageGB int `json:"ephemeralStorageGB,omitempty" yaml:"ephemeralStorageGB,omitempty"`

	// Cluster is the ECS cluster ARN (or name) the Fargate task runs on.
	// Operator-declared; this plugin does NOT create ECS clusters —
	// clusters are a shared operator resource (think `forge-fleet-tasks`)
	// that backup tasks join alongside other short-lived jobs.
	// Required for deployTo: lambda.
	Cluster string `json:"cluster" yaml:"cluster"`

	// SubnetIds is the VPC subnet list the Fargate task attaches to.
	// Must have outbound reachability to Atlas / RDS hosts AND to S3 (via
	// a VPC endpoint OR via NAT gateway).
	// Required for deployTo: lambda.
	SubnetIds []string `json:"subnetIds" yaml:"subnetIds"`

	// SecurityGroupIds is the security-group list applied to the task's
	// ENI. Must allow outbound 27017 (Mongo) / 5432 (Postgres) / 3306
	// (MySQL) + 443 (S3 + Atlas REST).
	// Required for deployTo: lambda.
	SecurityGroupIds []string `json:"securityGroupIds" yaml:"securityGroupIds"`
}

// BackupAtlasAdmin carries operator-provided Atlas admin API-key ids. Both
// fields are expected to be `${secret:...}` references.
type BackupAtlasAdmin struct {
	// PrivateKey is a `${secret:...}` reference to the Atlas API-key
	// private secret.
	PrivateKey string `json:"privateKey" yaml:"privateKey"`

	// PublicKey is a `${secret:...}` reference to the Atlas API-key
	// public id.
	PublicKey string `json:"publicKey" yaml:"publicKey"`

	// ProjectID is the Atlas project (group) ID the admin cred belongs
	// to. Used to scope CreateDatabaseUser calls. Required.
	ProjectID string `json:"projectId" yaml:"projectId"`
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
	// If any configured Database is mongodb, atlasAdmin must be supplied —
	// without it ComputeProcess can't mint a scoped user on the fly.
	needsAtlasAdmin := false
	for _, db := range c.Databases {
		if db.Resource == "" {
			return errors.Errorf("`databases[].resource` is required for %q (SC resource name of the DB to back up)", ResourceType)
		}
		if db.Driver == "" {
			return errors.Errorf("`databases[].driver` is required for %q resource %q (one of mongodb|postgres|mysql)", ResourceType, db.Resource)
		}
		if db.Driver == "mongodb" {
			needsAtlasAdmin = true
		}
	}
	if needsAtlasAdmin {
		if c.AtlasAdmin == nil {
			return errors.Errorf("`atlasAdmin` is required for %q when any `databases[].driver` is \"mongodb\" (Atlas admin REST needs the private/public key pair + projectId)", ResourceType)
		}
		if c.AtlasAdmin.PrivateKey == "" || c.AtlasAdmin.PublicKey == "" || c.AtlasAdmin.ProjectID == "" {
			return errors.Errorf("`atlasAdmin.{privateKey,publicKey,projectId}` are all required when any `databases[].driver` is \"mongodb\"")
		}
	}
	// Worker is required when deployTo: lambda (§5.3). For other
	// deployTo values (ecs-fargate / vm-compose / k8s) Worker is ignored
	// and may be absent.
	if c.DeployTo == "lambda" {
		if c.Worker == nil {
			return errors.Errorf("`worker` is required for %q when `deployTo: lambda` — Fargate one-shot task shape (cluster + subnets + securityGroups). See design doc §5.3.", ResourceType)
		}
		if c.Worker.Cluster == "" {
			return errors.Errorf("`worker.cluster` is required (ECS cluster ARN or name the Fargate task runs on)")
		}
		if len(c.Worker.SubnetIds) == 0 {
			return errors.Errorf("`worker.subnetIds` is required (at least one subnet)")
		}
		if len(c.Worker.SecurityGroupIds) == 0 {
			return errors.Errorf("`worker.securityGroupIds` is required (at least one security group)")
		}
	}
	return nil
}

// ReadBackupConfig is the api.ConfigReaderFunc registered in init.go.
func ReadBackupConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &BackupConfig{})
}
