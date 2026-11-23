// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package backup

import (
	"strings"

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
// See the sc-components-platform design doc §P1 v1.5 for why this is
// doc-only: the production `sc` binary does not link plugin /api
// modules, so a plugin-owned config cannot be Go-interface-asserted
// against; the dispatch reads `.Raw` instead.
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

	// OperatorPassword is a `${secret:...}` reference to the shared
	// password the SPA's /login handler validates against. Required for
	// the operator-facing SPA; the service refuses POST /api/v1/login
	// when unset (SPA reports `password_set: false` on /api/v1/session).
	//
	// Slice 1e.3 originally took this via the BACKUP_OPERATOR_PASSWORD
	// env — operators still set that env today. Slice 1e.4g promotes it
	// to a first-class config field so the plugin can inject it into the
	// dispatched Lambda alongside SigningKey.
	OperatorPassword string `json:"operatorPassword,omitempty" yaml:"operatorPassword,omitempty"`

	// Storage declares WHERE backup archives land. Required for every
	// deploy. One of s3 / gcs / filesystem (per design doc §5.1).
	//
	// Slice 1e.4a shipped the per-backend archive.Store implementations;
	// slice 1e.4g wires this config block through the plugin's dispatch
	// so an operator can declare the archive target in server.yaml
	// instead of reaching for BACKUP_STORAGE_* env vars by hand.
	Storage *BackupStorage `json:"storage,omitempty" yaml:"storage,omitempty"`

	// Metadata declares WHERE the service's own bookkeeping (tenants,
	// projects, runs, snapshots) lives. Optional — when absent,
	// resolution defaults to:
	//   - Storage.Type == filesystem → sqlite (same directory)
	//   - Storage.Type == s3 / gcs   → colocated (same bucket)
	//
	// The explicit backends (mongo / postgres / mysql / sqlite / colocated)
	// land across slices 1e.4b…e. See docs/design/2026-11-13/
	// sc-component-backup-pluggable-storage §5.2 for the per-backend
	// operator profiles.
	Metadata *BackupMetadata `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Notify points this component at an sc-component-notifier instance,
	// so a failed backup reaches Telegram (or a signed webhook) instead
	// of waiting to be noticed in the SPA.
	//
	// Optional. Absent means notifications are off — every existing
	// stack keeps deploying unchanged on the release that adds this.
	// PRESENT but unresolvable fails the provision: a publisher that
	// deploys with notifications silently off is the worst available
	// outcome, because the operator believes they are covered.
	//
	// The plugin also accepts the shorthand `notify: "https://host"`,
	// which this typed struct cannot express (a YAML union). Both are
	// resolved by plugin/internal/notify.go, which is the code that
	// actually runs — the `sc` binary never links this module.
	Notify *BackupNotify `json:"notify,omitempty" yaml:"notify,omitempty"`

	// NotifyToken is a `${secret:...}` reference to the notifier-minted
	// publisher credential (`op: "event"`, scoped to this tenant).
	//
	// A SEPARATE top-level field and deliberately NOT nested under
	// Notify: it reaches the service as a SecretsManager-backed secret,
	// which only happens for a top-level field registered in the
	// plugin's ComponentSecretEnvMap. SC core's applyComponentSecretEnvMap
	// reads `merged[field]` and nothing nested, so a token written
	// inside the `notify:` block could never be delivered that way.
	//
	// Required whenever Notify is set — see Validate.
	NotifyToken string `json:"notifyToken,omitempty" yaml:"notifyToken,omitempty"`

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

// BackupWorker shapes the ECS Fargate one-shot task-definition that runs
// the actual mongodump / pg_dump / mariadb-dump when this Component is
// deployed to aws-lambda. Per design doc §5.3.
//
// Scope note: this struct governs the TASK-DEFINITION sizing + the ECS
// cluster the plugin provisions for the backup worker. Runtime VPC
// placement (subnets + security groups) is NOT here — the service runtime
// reads `env.BACKUP_ECS_SUBNETS` + `env.BACKUP_ECS_SECURITY_GROUPS`
// directly from the Lambda's env block
// (service/internal/api/scheduler_tick.go:105), so the operator writes
// those as comma-separated strings in the `env:` section of their
// backup component config. A previous shape of this struct carried
// `SubnetIds []string` + `SecurityGroupIds []string` that were validated
// but never consumed by either the plugin or the service — the service
// has always read the env-var form, and the plugin's ProvisionEcsWorker
// never passed them anywhere. The fields were removed 2026-11-15 to
// drop the duplication; operators who wrote both get a slightly cleaner
// yaml, operators who wrote only `env.BACKUP_ECS_*` are unaffected.
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

	// Cluster is the ECS cluster name the plugin provisions for this
	// backup component's Fargate task family. Defaulted by the plugin's
	// ComponentDefaults to `sc-backup-<name>-cluster` — operator rarely
	// overrides. Required for deployTo: lambda.
	Cluster string `json:"cluster" yaml:"cluster"`
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

// BackupStorage declares the archive-storage backend. Exactly one
// backend-group of fields should be set — the plugin's Validate below
// cross-checks + errors on inconsistent combinations (e.g. Path set
// with Type == s3).
//
// Design: forge docs/design/2026-11-13/sc-component-backup-pluggable-storage §5.1.
type BackupStorage struct {
	// Type is the archive backend: "s3" | "gcs" | "filesystem".
	// Required.
	Type string `json:"type" yaml:"type"`

	// Object-store (s3 / gcs) fields.
	Bucket    string `json:"bucket,omitempty" yaml:"bucket,omitempty"`
	Region    string `json:"region,omitempty" yaml:"region,omitempty"`
	Endpoint  string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`   // S3-compatible endpoint override (MinIO / R2 / DO Spaces / Yandex OS)
	AccessKey string `json:"accessKey,omitempty" yaml:"accessKey,omitempty"` // `${secret:...}` — omit to use instance profile / Workload Identity
	SecretKey string `json:"secretKey,omitempty" yaml:"secretKey,omitempty"` // `${secret:...}`
	// UsePathStyle forces S3 path-style addressing (bucket in URL path
	// rather than hostname). Required for MinIO; harmless on AWS.
	UsePathStyle bool `json:"usePathStyle,omitempty" yaml:"usePathStyle,omitempty"`
	// Credentials is a path to a GCP service-account JSON (or a
	// `${secret:...}` reference the plugin materialises to a temp file).
	// Empty for GCS uses Application Default Credentials (Workload
	// Identity on GKE / Cloud Run / GCE).
	Credentials string `json:"credentials,omitempty" yaml:"credentials,omitempty"`

	// Filesystem field.
	Path string `json:"path,omitempty" yaml:"path,omitempty"` // absolute directory path
}

// BackupMetadata declares the metadata-store backend. Optional — when
// absent, defaults are chosen from BackupStorage:
//
//   - Storage.Type == "filesystem" → sqlite
//   - Storage.Type == "s3" / "gcs" → colocated
//
// Design: forge docs/design/2026-11-13/sc-component-backup-pluggable-storage §5.2.
type BackupMetadata struct {
	// Type is one of "colocated" | "mongo" | "postgres" | "mysql" | "sqlite".
	// Empty resolves per Storage.Type (see above).
	Type string `json:"type,omitempty" yaml:"type,omitempty"`

	// URI is the connection string for mongo / postgres / mysql.
	// Typically a `${secret:...}` reference.
	URI string `json:"uri,omitempty" yaml:"uri,omitempty"`

	// Path overrides the default SQLite file location
	// (<storage.path>/_meta/sc-component-backup.sqlite). Ignored for
	// non-SQLite backends.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

// BackupNotify is the `notify:` block — where this component publishes
// operator-relevant events.
//
// ORDERING IS LOAD-BEARING: the notifier resource must be declared
// BEFORE this component in server.yaml, the same class of trap as
// `uses:` ordering.
type BackupNotify struct {
	// URL is the notifier service's base URL. Either an explicit
	// hostname, or — preferred, because it survives a redeploy that
	// moves the notifier — a reference the OPERATOR writes:
	//
	//	notify:
	//	  url: "$sc:${resource:my-notifier.self-url}"
	//
	// Written by the operator rather than derived by this plugin on
	// purpose: a plugin cannot read a sibling resource's declaration
	// (PreProvisionRequest carries only the component's own config), and
	// a `${resource:…}` reference that a plugin emits is not re-resolved
	// by SC. Writing it here puts it in the resolution pass that
	// definitely runs.
	URL string `json:"url,omitempty" yaml:"url,omitempty"`

	// Name is the SC resource name of the notifier, recorded so logs and
	// error messages can name it. Informational only — it does not
	// resolve a URL, for the reason above.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
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
			return errors.Errorf("`worker.cluster` is required (ECS cluster name the plugin provisions for the Fargate task; defaulted by ComponentDefaults to `sc-backup-<name>-cluster`)")
		}
		// Runtime VPC placement is read from env.BACKUP_ECS_SUBNETS +
		// env.BACKUP_ECS_SECURITY_GROUPS (comma-separated), set by the
		// operator in the `env:` block. Deliberately NOT a worker field
		// (see BackupWorker's doc note on the 2026-11-15 removal).
	}
	// Storage: optional for back-compat (service-side env falls back to
	// S3_* legacy vars for one release), but when PRESENT must be
	// well-formed.
	if c.Storage != nil {
		if err := c.Storage.Validate(); err != nil {
			return err
		}
	}
	// Metadata: ditto. Also cross-check against Storage.Type for the
	// sqlite + non-filesystem rejection.
	if c.Metadata != nil {
		if err := c.Metadata.Validate(c.Storage); err != nil {
			return err
		}
	}
	// Notify: the url+token pair is all-or-nothing. Half of it is the
	// failure mode this component exists to remove — a publisher that
	// can reach the notifier but cannot authenticate gets a 401 on its
	// first real alert, at the exact moment the operator is relying on
	// it. The authoritative check runs plugin-side
	// (plugin/internal/notify.go, which also sees the string shorthand
	// this struct cannot express); this one keeps the typed config
	// honest for anyone reading or generating it.
	if err := c.validateNotify(); err != nil {
		return err
	}
	// Lambda + filesystem archive: refuse at parse time (/tmp is
	// ephemeral on Lambda cold starts — metadata would evaporate).
	if c.DeployTo == "lambda" && c.Storage != nil && c.Storage.Type == "filesystem" {
		return errors.Errorf("`storage.type: filesystem` is incompatible with `deployTo: lambda` — Lambda /tmp is ephemeral; use `storage.type: s3` instead")
	}
	return nil
}

// validateNotify enforces the notify.url + notifyToken pair.
//
// Each message names the key to add, because "notifications don't work"
// is otherwise a four-layer debug across plugin, env, token and
// channel.
func (c *BackupConfig) validateNotify() error {
	hasToken := strings.TrimSpace(c.NotifyToken) != ""
	if c.Notify == nil {
		if hasToken {
			return errors.Errorf("`notifyToken` is set but there is no `notify:` block — add `notify: {url: \"https://...\"}` so the token has somewhere to be presented")
		}
		return nil
	}
	if strings.TrimSpace(c.Notify.URL) == "" {
		if name := strings.TrimSpace(c.Notify.Name); name != "" {
			return errors.Errorf("`notify.name: %[1]s` names a resource but no URL: add `notify.url: \"$sc:${resource:%[1]s.self-url}\"`, which SC resolves because you wrote it", name)
		}
		return errors.Errorf("`notify:` is declared with no `url` — add `notify.url: \"https://...\"`, or remove the `notify:` block to leave notifications off")
	}
	if !hasToken {
		return errors.Errorf("`notify.url` is set but `notifyToken` is not — the publisher would reach the notifier and be refused with a 401 on its first alert. Mint a publisher token on the notifier, register it with `sc secrets add`, and set `notifyToken: \"${secret:<name>}\"`")
	}
	return nil
}

// Validate checks the BackupStorage block for self-consistency.
func (s *BackupStorage) Validate() error {
	switch s.Type {
	case "":
		return errors.Errorf("`storage.type` is required (one of s3|gcs|filesystem)")
	case "s3":
		if s.Bucket == "" {
			return errors.Errorf("`storage.bucket` is required when `storage.type: s3`")
		}
		if s.Path != "" {
			return errors.Errorf("`storage.path` is only valid when `storage.type: filesystem`")
		}
	case "gcs":
		if s.Bucket == "" {
			return errors.Errorf("`storage.bucket` is required when `storage.type: gcs`")
		}
		if s.Path != "" {
			return errors.Errorf("`storage.path` is only valid when `storage.type: filesystem`")
		}
	case "filesystem":
		if s.Path == "" {
			return errors.Errorf("`storage.path` is required when `storage.type: filesystem`")
		}
		if s.Bucket != "" || s.Region != "" || s.Endpoint != "" || s.AccessKey != "" || s.SecretKey != "" {
			return errors.Errorf("object-store fields (bucket/region/endpoint/accessKey/secretKey) are not valid when `storage.type: filesystem`")
		}
	default:
		return errors.Errorf("`storage.type: %q` is not recognised (one of s3|gcs|filesystem)", s.Type)
	}
	return nil
}

// Validate checks the BackupMetadata block + its cross-constraints
// against the paired BackupStorage.
func (m *BackupMetadata) Validate(storage *BackupStorage) error {
	switch m.Type {
	case "", "colocated":
		return nil
	case "mongo", "postgres", "mysql":
		if m.URI == "" {
			return errors.Errorf("`metadata.uri` is required when `metadata.type: %s`", m.Type)
		}
	case "sqlite":
		// SQLite only makes sense with a filesystem archive.
		if storage == nil {
			return errors.Errorf("`metadata.type: sqlite` requires a `storage:` block of type filesystem")
		}
		if storage.Type != "filesystem" {
			return errors.Errorf("`metadata.type: sqlite` is only valid with `storage.type: filesystem` (got %q) — object stores can't host SQLite's random writes; use `metadata.type: colocated` instead", storage.Type)
		}
	default:
		return errors.Errorf("`metadata.type: %q` is not recognised (one of colocated|mongo|postgres|mysql|sqlite)", m.Type)
	}
	return nil
}

// ReadBackupConfig is the api.ConfigReaderFunc registered in init.go.
func ReadBackupConfig(config *api.Config) (api.Config, error) {
	return api.ConvertConfig(config, &BackupConfig{})
}
