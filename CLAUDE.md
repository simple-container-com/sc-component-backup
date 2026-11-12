# sc-component-backup

The template **and** the throwaway smoke subject for the sc-components-platform
milestone's slice 1a. Both roles at once — like sc-plugin-echo is both a
template and a working stub.

This repo has THREE modules, not two. The new one is `service/` — the
operator-facing microservice. `api/` + `plugin/` match the sc-plugin-echo
layout (config tier + provisioning tier).

## Hard invariants you must not undo

1. **No Go-interface assertion against plugin-owned values anywhere.**
   The production `sc` binary does NOT link this repo's `/api` module —
   plugins reach core exclusively via `hashicorp/go-plugin` gRPC, and
   plugin-owned values arrive at core as opaque `*api.PluginFieldConfig`
   with the operator's YAML in `.Raw`. A core-defined Go interface
   defined on a plugin-owned concrete type is UNREACHABLE in production
   — the assertion always falls through to the default branch. See
   `forge` memory `interface_assertion_against_plugin_owned_value_fails_in_prod`
   under `index-sc-deploy-secrets`, and the sc-components-platform design
   doc §P1 v1.5 rewrite that this invariant exists to prevent re-breaking.

   The write-site check: `go list -deps ./cmd/sc | grep sc-component-backup/api`
   in the SC api repo must return EMPTY in production. If this ever fills,
   you're relying on compile-time linkage, which breaks the first time an
   operator ships an SC release built without the blank-import.

2. **Schema-advertised component types + `.Raw` reader is the dispatch
   mechanism**. `SchemaResponse.component_resource_types` lists
   `["sc-backup"]`; SC core treats resources whose `type:` is in that set
   as components, reads their config fields via a core-side
   `api.ComponentConfig` wrapper over `PluginFieldConfig.Raw`, and
   dispatches to the operator-named template. The plugin binary does
   NOT initiate the dispatch — it just reads the deployed service URL
   from `DependencyOutputsJson._deployed_service` in its own `Provision`
   RPC (post-dispatch) and republishes it.

3. **Push-register, not stack-exports.** When a consumer declares
   `uses: [my-backup]`, the plugin's `ComputeProcess` sidecar PUSHES a
   tenant registration to the dispatched service via
   `POST /api/v1/tenants/register`, signed with an HS256 JWT. 3 attempts
   with exponential backoff 1s/2s/4s + 25% jitter, 10s per-attempt
   timeout, non-retryable 4xx except 429, consumer deploy fails
   diagnosably on all-retries-exhausted. See `plugin/internal/compute.go`.
   Tenants persist in the service's own Mongo, not in Pulumi state —
   this is the §P3 push model (replaces the "service reads its own
   stack outputs at runtime" plan that was refuted as unimplementable
   against today's primitives).

4. **signingKey is per-component, not fleet-wide.** The operator sets
   `signingKey: ${secret:backup-service-jwt-key}` under
   `my-backup.config:` in server.yaml. Path A's dispatch injects this as
   `SERVICE_JWT_SECRET` env on the deployed container; the plugin reads
   it from its own Provision inputs when minting per-tenant JWTs at
   ComputeProcess time. Rotation is a server.yaml edit + re-provision —
   both sides pick up the new key on the next run. This is NOT
   `SERVICE_JWT_SECRET` as used by the Forge fleet (which is a
   fleet-wide shared key); do NOT reach for `staging-forge-encryption-key`
   or any fleet alias.

5. **No Dockerhub publish from this repo's CI.** The service image is
   built locally at smoke time (`docker build + docker load` on the
   target host, or `go build` + `docker` run from a cloned repo on the
   host). The operator's explicit yes is required to push
   `simplecontainer/sc-component-backup:<CalVer>` to the shared
   namespace (fleet CLAUDE.md "package publishing, registries or
   release pipelines" ratchet). The release workflow in
   `.github/workflows/release.yml` ships the PLUGIN binary only.

## Smoke path (local, no Forge fleet redeploy)

1. **Build locally**: `make build` at the repo root.
2. **Plugin delivery**: `type: path` in the throwaway parent
   `server.yaml`'s `plugins.backup.source` block (not `github-release`
   until a tagged release exists). OR produce a tar.gz of the linux/amd64
   binary + `scp` to the target VPS and pin via `type: path`.
3. **Service image**: build on the host directly, OR `docker save` →
   `scp` → `docker load`. Pin by digest, not by tag.
4. **Throwaway stack names**: literal prefix `smoke-sc-component-backup-`
   so nothing can collide with any fleet-owned stack name. NEVER `sc
   provision` or `sc deploy` against a stack name that could be a Forge
   fleet stack.

## Running tests

```
make test
```

Both `api/` + `plugin/` tests must stay green on `main`. `service/`
tests cover HTTP handlers + JWT middleware + the tenant store; they
run against an in-memory Mongo stub (no external deps).

## Writing a real component from this template

Clone, rename `backup` → `<component-name>` throughout, swap the service
body for the real work (Backup → pg_dump/mongodump + S3 writer;
Monitoring → OTel ingest + Mongo; …), and keep the five hard
invariants above intact. The sc-components-platform design doc §P2
has the three-module target layout and §Slice map has the per-slice
DoD.
