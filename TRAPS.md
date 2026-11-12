# The traps this repo already prevents

Carries the fifteen traps from `sc-plugin-echo`'s `TRAPS.md` plus two new
ones specific to the SC Components class.

Canonical catalogue (traps 1-15):
**[forge/docs/design/2026-11-01/plugin-extraction-playbook/](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-11-01/plugin-extraction-playbook/)**.
Traps 16-17 live here because they're specific to the Component class
mechanism — not generic to all plugins.

## Already encoded in this repo — don't undo

| # | Trap | How the repo handles it |
|---|---|---|
| 1-15 | See sc-plugin-echo TRAPS.md | Inherited verbatim via the shared `api/pkg/plugin/runtime` scaffolding (NewContext, Settle, HarvestExports, ParentInfoFromRequest, RunComputeProcess). |
| **16** | **Go-interface assertion against plugin-owned value never matches in production**. `cfg.(api.X)` where `X` is a core-defined interface and `cfg` crossed go-plugin gRPC falls through to the default branch — production `sc` does not link plugin `/api` modules, so plugin-owned values arrive as opaque `*api.PluginFieldConfig`. 8th exposure in 24h at the time of writing. | Dispatch uses **Schema-advertised `component_resource_types` + `.Raw` reader** on the opaque wrapper. Same pattern as `safe_to_replace_types` (api #557) and api #592's `multiRegistrar.forDomain` fix. No `cfg.(api.…)` anywhere in `plugin/` for plugin-owned values. Pre-release grep: `grep -rn 'cfg\.(api\.' plugin/ && echo REJECT`. |
| **17** | **Push-register call to a freshly-deployed Lambda can cold-start, roll, or 5xx mid-redeploy**. A naive single-attempt call fails consumer deploys on transient 5xx with no retry. | `plugin/internal/compute.go` wraps the `POST /api/v1/tenants/register` call in a retry policy per the sc-components-platform design doc §P3 push-model operational contract: 3 attempts total, exponential backoff `1s → 2s → 4s` + 25% jitter, 10s per-attempt timeout, non-retryable 4xx except 429, consumer deploy fails diagnosably on all-retries-exhausted with a hint naming the component as the blocker. |

## Checklist before cutting a release

Grep across the plugin:

- [ ] `grep -rn 'cfg\.(api\.' plugin/` → nothing (trap 16)
- [ ] `grep -rn 'PluginFieldConfig' plugin/internal/` → ok to appear ONLY in comments or as explicit `.Raw` reads (trap 16)
- [ ] `grep -rn 'defer done()' plugin/` → nothing (trap 1)
- [ ] `grep -rn 'pctx\.Close()' plugin/` → nothing outside `_test.go` (trap 1)
- [ ] `grep -rn 'ProvisionResponse{Urn:' plugin/` → every match carries `Outputs:` (trap 3)
- [ ] `plugin/internal/compute.go` retry policy verified: 3 attempts, exponential 1s/2s/4s + 25% jitter, 10s timeout per attempt, non-retryable 4xx except 429 (trap 17)
- [ ] `grep -rn 'go:embed.*dist' service/` → every match uses `all:dist` prefix (verbatim pattern from forge-notifier/internal/ui/ui.go)

## When you hit a new trap

Add it to this file + the forge plugin-extraction playbook. The pre-release
lint checklist is the floor; add greps as you go.
