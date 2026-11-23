# sc-component-backup

Scheduled backups for the Simple Container fleet. Any SC consumer that
declares `uses: [the-backup]` in its `client.yaml` gets per-tenant
`mongodump` / `pg_dump` / `mariadb-dump` to a shared S3 bucket, driven
from a managed backup microservice the operator declares once under the
parent `server.yaml`.

**Component class, not plugin.** `sc-component-backup` is the first real
member of the SC Component class (defined in
[sc-components-platform](https://github.com/simple-container-com/forge/blob/main/docs/roadmap/items/sc-components-platform.md),
slice 1b.b): the plugin's resource declaration provisions a cloud template
(Lambda or ECS Fargate — operator picks per-tenant) *plus* a full backup
microservice behind it (Go backend + React SPA via `go:embed` + Atlas
tenant store). Scaffold cloned from
[sc-component-dummy](https://github.com/simple-container-com/sc-component-dummy)
(the empty reference), business-logic surface ported from
[local-db-backup](https://github.com/simple-container-com/local-db-backup)
(the ad-hoc Python impl we're replacing).

## How it works

```
operator's server.yaml                  operator's client.yaml (per service)
  resources:
    the-backup:                             stacks:
      type: sc-backup                         staging:
      config:                                   uses: [the-backup]        ← consumer opt-in
        deployTo: lambda                        type: single-image
        image: {...}                            template: lambda
        signingKey: ${secret:JWT_KEY}           parent: forge/infra
        ingress:
          hostname: backups.example.com
```

1. `sc provision` on the parent → slice 1a's sidecar wrapper dispatches
   the Backup service into the chosen deployTo template (Lambda in the
   example). A real backup service Lambda comes up at
   `backups.example.com`, wired to the shared S3 bucket + its own
   Mongo tenant store.
2. `sc deploy` on a consumer → the plugin's `ComputeProcess` sidecar
   reads the consumer's sibling DB dependency outputs via
   `dependency_outputs_json`, mints a per-consumer HS256 JWT, and
   `POST`s a tenant row to the Backup service's
   `/api/v1/tenants/register`. The consumer gets `BACKUP_AGENT_URL`
   and `BACKUP_TOKEN` injected into its env.
3. A timer POSTs `/api/v1/scheduler/tick`. The handler walks the
   registered tenants, fires an ECS Fargate one-shot per tenant that is
   DUE, and reports the rest under `skipped` with a reason —
   `mongodump`/`pg_dump` to
   `s3://<bucket>/<consumer-stack>/<db>/<ts>.archive.gz`.

   Due-ness is `now - lastSuccessAt >= IntervalMinutes`, read from the
   run history rather than a stored marker, so there is no second source
   of truth to drift. `IntervalMinutes: 0` means **use the default**
   (4 h), not "never" — zero is what records written before the field
   carry. Only SUCCEEDED runs count, so a failing tenant stays due
   rather than being marked done by having been attempted.

### The timer is operator config, and without it nothing runs

The service does not schedule itself: a Lambda cannot hold a ticker (the
runtime freezes background goroutines when the handler returns), so the
timer must be a platform resource. Declare it on the component:

```yaml
    schedulerToken: "${secret:backup-scheduler-token}"
    cloudExtras:
      lambdaSchedules:
        - name: bk-tick                # <= 14 chars, rejected at APPLY
          expression: "rate(30 minutes)"
          request: |-
            {
              "requestId": "scheduled-backup",
              "httpMethod": "POST",
              "path": "/api/v1/scheduler/tick",
              "requestContext": {
                "http": {
                  "path": "/api/v1/scheduler/tick",
                  "method": "POST",
                  "protocol": "HTTP/1.1"
                }
              },
              "body": "{}",
              "headers": {
                "Authorization": "Bearer ${secret:backup-scheduler-token}"
              }
            }
```

**Tick every 30 minutes, back up every 4 hours.** The tick is cheap — it
lists tenants and fires only the due ones — and a finer tick than the
interval is what makes a missed fire self-heal instead of doubling the
gap to 8 h.

It lives in operator config rather than the component's defaults because
SC resolves `${secret:NAME}` inside a schedule request but not the
`${resource:...}` form, and a generic component cannot hardcode one
stack's secret name.

> This was missing until 2026-11-19, and the gap was invisible: no rule
> existed, the handler was a no-op without `?force=true`, and the SPA
> displayed "every hour" from a stored value nothing read. Every archive
> in the bucket was from a manual Run Now.

## Three Go modules

```
sc-component-backup/
├── api/         ← config tier. Documents the expected .Raw keys of a
│                  `type: sc-backup` resource. NOT imported by SC core;
│                  the production `sc` binary reaches this plugin only
│                  via go-plugin gRPC, so SC core reads these keys off
│                  PluginFieldConfig.Raw directly (see TRAPS.md).
├── plugin/      ← provisioning tier. hashicorp/go-plugin subprocess
│                  binary SC core spawns via the `plugins:` block.
│                  Schema advertises `sc-backup` under
│                  `component_resource_types`. ComputeProcess is where
│                  the P3 push-register call lives (see the design doc).
└── service/     ← the microservice. go-aws-lambda-sdk entry (runs under
                   Lambda or as plain HTTP when `PORT` is set). go-chi
                   router. Embedded React SPA via `//go:embed all:dist`.
                   MongoDB tenant store. Published to Dockerhub as
                   `simplecontainer/sc-component-backup:<CalVer>` by
                   the release workflow when `DOCKERHUB_USERNAME` +
                   `DOCKERHUB_TOKEN` org secrets are present.
```

## Running tests

```
make test       # fans out to all three modules
```

All three modules stay green on `main`. `service/` tests cover the
HTTP handlers, JWT middleware, scheduler, storage, and driver
dispatch; they need Go 1.26+ and no external services (mongodump /
pg_dump / mariadb-dump are stubbed when unavailable).

## Release

Push to `main` → the release workflow:
1. Builds `sc-component-backup-{linux,darwin}-{amd64,arm64}`
   binaries (OOM-pinned per the `sc-plugin-echo` template).
2. Cuts a `v<CalVer>-<sha>` tag + GitHub Release with the binaries.
3. Builds + pushes `simplecontainer/sc-component-backup:<version>`
   + `:latest` to Dockerhub **if** `DOCKERHUB_USERNAME` +
   `DOCKERHUB_TOKEN` org secrets are present. Without them, the step
   emits a `::warning` and no-ops; the plugin release still completes.

Manual tag push (`git tag v2026.11.12-XXXX && git push origin v2026.11.12-XXXX`)
cuts a named release without an auto-tag step.

## Status

- Repo scaffolded 2026-11-11 (from sc-component-dummy).
- Dummy→backup rename + scaffold bug fixes 2026-11-12.
- Slice 1b.b bring-up — this commit. Dispatch mechanism (Schema+Raw,
  Path A) proven live by slice 1a's fleet smoke on 2026-11-12; see the
  [sc-components-platform roadmap item](https://github.com/simple-container-com/forge/blob/main/docs/roadmap/items/sc-components-platform.md).
- Slice 1c (plugin Provision wires the service URL back) — next.
- Slice 1e (backup logic port from `local-db-backup`) — the Python
  engines/scheduler/storage/retention become Go. Not in 1b.b.
- Slice 1f (restore SPA + live smoke) — after 1e.

## Storage

The service persists two very different kinds of thing, and the
go-embedb adoption only moves one of them.

- **Archive blobs** (`.tar.gz`, `.sql.gz`, `.gz` artefacts produced
  by `mongodump` / `pg_dump` / `mariadb-dump`) stay in
  **S3 / GCS / filesystem**, picked by `STORAGE_TYPE`
  (`service/cmd/backup/config.go#buildArchiveStore`). Flipping
  `USE_GOEMBEDB=1` does NOT move these — the archive store is read
  on every download + presigned-URL request, and it is deliberately
  object-storage-shaped. The S3 keys the operator sees in
  `s3://<bucket>/<consumer-stack>/<db>/<ts>.archive.gz` keep their
  shape across the cutover.
- **Metadata rows** (tenants, projects, backup + restore runs,
  snapshots + object samples, telegram settings, alerts
  dismissals — the `sc_component_backup` database's eight
  collections) can ride on the existing external-Mongo backend
  (`METADATA_TYPE=mongo`) OR on an embedded **go-embedb**
  (ferretdb + sqlite + optional S3 active-active replication) when
  `USE_GOEMBEDB=1` is set.

### Environment variables (go-embedb)

| var | default | purpose |
|---|---|---|
| `USE_GOEMBEDB` | `0` | `1` diverts the metadata backend onto the embedded listener. |
| `GOEMBEDB_SQLITE_DIR` | `/tmp/goembedb/` | sqlite base dir (per-Lambda, warm-reused). |
| `GOEMBEDB_REPLSET_NAME` | `backup-rs` | replica-set name the embed advertises. |
| `GOEMBEDB_REPLICATION_BACKEND` | `s3` | replication substrate (`s3` is the only shipped backend today). |
| `GOEMBEDB_S3_BUCKET` | — | S3 bucket for active-active replication. Unset → single-node embed (fine for a smoke). |
| `GOEMBEDB_S3_PREFIX` | `sc-component-backup--staging/` | key prefix inside the bucket. |
| `GOEMBEDB_S3_REGION` | `$AWS_REGION` | bucket region. |
| `GOEMBEDB_ACK_WAIT_LIVE_ONLY` | auto (on under Lambda) | guards against go-embedb Bug 13 — never wait for a replica that never came back. |
| `GOEMBEDB_MAX_REPLICAS` | 10 under Lambda | upper bound on tracked replicas. |
| `GOEMBEDB_HEARTBEAT_TTL` | 3m under Lambda | when a silent replica is dropped. |
| `ADMIN_MIGRATION_TOKEN` | — | enables the migration endpoint (see below). |

### Building against go-embedb (a private dependency)

go-embedb is a **private** fork of FerretDB, published under the upstream module path, so
`service/go.mod` routes it with

```
replace github.com/FerretDB/FerretDB => github.com/simple-container-com/go-embedb v0.0.0-…
```

Two properties of that line are load-bearing, and the first one took this repo's `main` red
once already (`3a888c5`, reverted by `#35`):

- **It must be a module path + pseudo-version, never a filesystem path.** A sibling-path
  replace (`=> ../../go-embedb`) resolves only on a machine that happens to have the fork
  checked out next door. And it fails in `go mod download` — while the module graph is
  being loaded, *before* any build tag or env var is read — so `USE_GOEMBEDB` defaulting to
  `0` gates nothing: the external-Mongo path becomes unbuildable too.
- **A filesystem replace never validates the pseudo-version string**, so a wrong timestamp
  in it stays invisible until the day the path is swapped for a module path.

Because the fork is private, a build needs a read credential:

| Where | How |
|---|---|
| **CI** (`.github/workflows/{pr,release}.yml`) | `sc secrets reveal --force` materialises `.sc/deploy-keys/go-embedb.pem` from this repo's SC registry, and `docker/build-push-action`'s `secret-files:` mounts it into the `go-builder` stage. The mount is not a layer and not a build-arg, so the key reaches neither the image nor the log. |
| **Locally** | `sc secrets reveal --force`, then `GOPRIVATE='github.com/simple-container-com/*' GIT_SSH_COMMAND='ssh -i .sc/deploy-keys/go-embedb.pem -o IdentitiesOnly=yes' go mod download`. `GOPRIVATE` is required: without it the public proxy tries the fetch, cannot read a private repo, and reports git's auth failure wrapped as a misleading `invalid version`. |
| **`dev/smoke.sh`** | Same reveal; `dev/docker-compose.yml` passes the plaintext path as a compose build secret. The script checks for the file up front rather than letting the failure surface 90 s into the image build. |

The key itself is **SC-encrypted in `.sc/secrets.yaml`**, which is committed; the plaintext
half and `.sc/cfg.*.yaml` are gitignored. The registry is encrypted to the fleet's shared
recipient set, so the single **org-level `SC_CONFIG`** decrypts it with no per-repo GitHub
secret — see the fleet convention in `forge/CLAUDE.md` §"Secret management".

## Migration (external Mongo → go-embedb)

The cutover uses an admin-only endpoint and is **opt-in**: the route
is mounted only when `ADMIN_MIGRATION_TOKEN` is set AND
`USE_GOEMBEDB=1` is live (a sink exists). One-time operator tool;
not part of the steady-state surface.

```
POST /admin/migrate-mongo-to-goembedb[?dry_run=1]
Headers: X-Admin-Token: <ADMIN_MIGRATION_TOKEN>
```

Behaviour:

1. For each collection in `internal/goembedbadmin/collections.go#Collections`:
   - `CountDocuments` on the source external Mongo.
   - `Find` streams every document; each upserted into the same-named
     sink collection by its own `_id`.
2. The response reports per-collection source + migrated counts,
   plus a `verified` boolean that is true only when every source
   row landed and no per-document error fired.

`dry_run=1` counts but does not copy. Per-document errors are
non-fatal — the endpoint continues past them and lists every
failure, so a half-populated sink is observable rather than silent.

Example (staging smoke):

```sh
# Dry-run — see what WOULD move.
curl -s -X POST -H "X-Admin-Token: $TOK" \
  "https://backups.example.com/admin/migrate-mongo-to-goembedb?dry_run=1" | jq .

# Real run.
curl -s -X POST -H "X-Admin-Token: $TOK" \
  "https://backups.example.com/admin/migrate-mongo-to-goembedb" | jq .verified
```

The archive bucket is **never** touched by this endpoint. See the
package-doc §"Blob carve-out" for the closed set of collections
that are copied.

## Rollback

- `USE_GOEMBEDB=0` on the Lambda env → the next deploy reverts to
  the `METADATA_TYPE=mongo` + `METADATA_URI` path exactly as before
  go-embedb was wired. Any writes made against the embed after the
  cutover are NOT replicated back to the external cluster by this
  service; keep the external cluster hot during the first 24h of a
  cutover for exactly this reason, then decommission.
- The archive bucket keeps its contents regardless — there is no
  rollback step for blobs because they never moved.
- Pulumi state for the per-tenant ECS Fargate task + the EventBridge
  rule is untouched by the cutover.

## Metrics

A Prometheus text-exposition endpoint ships at `GET /metrics` for
operator alerting:

```
backup_store_backend{backend="goembedb"} 1
backup_store_backend{backend="mongo"} 0
backup_store_backend{backend="postgres"} 0
backup_store_backend{backend="mysql"} 0
backup_store_backend{backend="sqlite"} 0
backup_store_backend{backend="colocated"} 0
```

The gauge is set once at boot in `cmd/backup/main.go` after the
metadata backend is chosen; a deploy flip (operator set
`USE_GOEMBEDB=1` but the env never propagated) shows up as a stale
`mongo==1` sample instead of a silent misconfiguration.

The endpoint is unauthenticated on purpose — it exposes no
tenant-scoped data, only process-level counters + the backend
gauge. Same shape as forge-notifier, forge-aigateway and
sc-component-observatory.

## Design docs

- Platform: [sc-components-platform](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-11-10/sc-components-platform/)
- Backup refactor plan: [sc-component-backup-refactor](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-11-12/sc-component-backup-refactor/)
- go-embedb active-active S3: [go-embedb-active-active-s3](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-11-22/go-embedb-active-active-s3/)
- Source for the business-logic port: [local-db-backup](https://github.com/simple-container-com/local-db-backup)

## License

MIT. See [LICENSE](LICENSE).
