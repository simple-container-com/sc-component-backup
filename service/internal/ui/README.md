# sc-component-backup — web UI

SPA built with Vite + React 18 + TypeScript. Backend contract: Go HTTP handlers
in `service/internal/api/` + the `/api/v1/*` surface defined in
`service/internal/ui/src/api/client.ts`. The build goes to `dist/`; the Go
binary serves it from `/` via `//go:embed all:dist` (SPA fallback to
`index.html`). Historical note: the SPA was ported from the Python reference
`local-db-backup` in slice 1e of the sc-components-platform milestone — a few
mock fixtures (`mock/data.ts`) still carry the reference's example tenant
names; they are fixtures, not deployed content.

## Commands

```sh
npm ci              # dependencies (versions pinned in package.json)
npm run dev         # http://localhost:5173, /api is proxied to http://127.0.0.1:8080
npm run dev:mock    # same, but with a fake API inside Vite — no backend needed
npm run typecheck   # tsc --noEmit
npm run build       # type check + build into dist/
```

## Mock API (`npm run dev:mock`)

Password: `admin`. Data is in memory and resets on restart:

- `infra-staging` — MongoDB, 23 databases (including a large one at ~1.5 GB); the last
  finished run is partial (`Failed: connection closed`), a manual run is in progress;
- `billing` — PostgreSQL every 6 h, with globals;
- `shop-legacy` — MariaDB, never run.

A connection check with `fail` in the string returns an error. Environment variables:
`MOCK_PASSWORD=…`, `MOCK_NO_PASSWORD=1` (no password set), `MOCK_EMPTY=1` (no projects),
`MOCK_STORAGE_DOWN=1` (storage unavailable).

## Rules

- No native `<select>`, `<input type="date">`, `<input type="number">` — only our own controls
  from `src/components/ui`.
- The connection string is write-only; the UI shows "Set · Replace".
- Fonts come from npm (`@fontsource`), no external requests: the NAS may have no internet access.
