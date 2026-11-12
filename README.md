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
3. The Backup service's scheduler runs the registered tenants on the
   cadence declared in each tenant row — `mongodump`/`pg_dump` to
   `s3://<bucket>/<consumer-stack>/<db>/<ts>.archive.gz`.

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
1. Builds `sc-plugin-backup-{linux,darwin}-{amd64,arm64}`
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

## Design docs

- Platform: [sc-components-platform](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-11-10/sc-components-platform/)
- Backup refactor plan: [sc-component-backup-refactor](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-11-12/sc-component-backup-refactor/)
- Source for the business-logic port: [local-db-backup](https://github.com/simple-container-com/local-db-backup)

## License

MIT. See [LICENSE](LICENSE).
