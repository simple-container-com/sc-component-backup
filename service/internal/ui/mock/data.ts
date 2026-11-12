// In-memory fake backend for `npm run dev:mock`. Follows docs/api.md; never used in production builds.
import { createHash } from "node:crypto";
import type {
  Meta,
  Alert, Archive, ArchivesResponse, EngineId, KeptBy, Overview, ProjectConfig, ProjectSummary,
  Retention, RunDetail, RunningOp, RunSummary, DbRun, TestConnectionResult, AppSettings,
} from "../src/api/types";

const GB = 1024 ** 3;
const MB = 1024 ** 2;
const PREFIX = "atlas";

export const META: Meta = {
  version: "0.3.0",
  build: { commit: "4f1fc41", deployed_at: "2026-11-09T10:00:00Z" },
  engines: [
    { id: "mongodb", label: "MongoDB", uri_example: "mongodb+srv://user:pass@cluster.example.net/?readPreference=secondary",
      dump_args: ["--numParallelCollections", "--excludeCollection", "--excludeCollectionsWithPrefix", "--readPreference"], supports_globals: false },
    { id: "postgres", label: "PostgreSQL", uri_example: "postgresql://user:pass@db.example.net:5432/postgres?sslmode=require",
      dump_args: ["--exclude-table", "--exclude-table-data", "--exclude-schema"], supports_globals: true },
    { id: "mariadb", label: "MariaDB", uri_example: "mariadb://user:pass@db.example.net:3306/",
      dump_args: ["--ignore-table", "--max-allowed-packet", "--net-buffer-length"], supports_globals: false },
    { id: "s3import", label: "Import from S3", uri_example: "s3://ACCESS_KEY:SECRET@s3.eu-central-1.amazonaws.com/bucket?region=eu-central-1",
      dump_args: [], supports_globals: false },
  ],
};

const RESTORE: Record<EngineId, string> = {
  mongodb: 'mc cat nas/{key} | mongorestore --uri "<target>" --archive --gzip',
  postgres: 'mc cat nas/{key} | pg_restore --dbname "<target>" --no-owner --exit-on-error',
  mariadb: 'mc cat nas/{key} | gunzip | mariadb --defaults-extra-file=<target> <database>',
  s3import: 'mc cat nas/{key} | gunzip | mariadb --database="<target_db>"   # credentials via ~/.my.cnf',
};
const RESTORE_BY_SUFFIX: Partial<Record<EngineId, Record<string, string>>> = {
  s3import: { ".sql.gz": RESTORE.s3import, ".tar.gz": "mkdir -p <target_dir> && mc cat nas/{key} | tar xz -C <target_dir>" },
};
const SUFFIX: Record<EngineId, string> = { mongodb: ".archive.gz", postgres: ".pgdump", mariadb: ".sql.gz", s3import: ".sql.gz" };
// s3import keeps the source's suffix: the Ghost content directory comes as a tarball.
const suffixFor = (engine: EngineId, db: string) => (engine === "s3import" && db.startsWith("@") ? ".tar.gz" : SUFFIX[engine]);

const INFRA_DBS = [
  "analytics-events--staging", "audit-log--staging", "auth--staging", "billing-api--staging", "catalog--staging",
  "cms--staging", "crm--staging", "feature-flags--staging", "forge-conductor--staging", "forge-gateway--staging",
  "forge-sessions--staging", "geo--staging", "inbox--staging", "ledger--staging", "media-meta--staging",
  "notifications--staging", "payments--staging", "pricing--staging", "reports--staging", "scheduler--staging",
  "search-index--staging", "users--staging", "webhooks--staging",
];
const FAILED_DB = "analytics-events--staging";

interface MockProject {
  config: ProjectConfig;
  uriSet: boolean;
  serverDbs: string[];
  sizes: Record<string, number>;
}

interface Active {
  project: string;
  runId: number;
  startedAt: number;
  queueMs: number;
  discoverMs: number;
  perDbMs: number;
  dbs: string[];
  retryDb?: string;
}

const now = () => Date.now();
const iso = (t: number) => new Date(t).toISOString().replace(/\.\d{3}Z$/, "+00:00");
const tsKey = (t: number) => new Date(t).toISOString().replace(/[-:]/g, "").replace(/\.\d{3}Z$/, "Z");
export const revisionOf = (c: ProjectConfig) => createHash("sha256").update(JSON.stringify(c)).digest("hex");

function rand(seed: number) {
  let s = seed >>> 0;
  return () => ((s = (s * 1664525 + 1013904223) >>> 0) / 2 ** 32);
}

// ---- retention (GFS) ----
function bucket(t: number, rule: KeptBy): string {
  const d = new Date(t);
  if (rule === "daily") return d.toISOString().slice(0, 10);
  if (rule === "monthly") return d.toISOString().slice(0, 7);
  const day = (d.getUTCDay() + 6) % 7;
  const monday = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() - day));
  return monday.toISOString().slice(0, 10);
}

function applyRetention(list: { ts: number; size: number; key: string }[], r: Retention): Archive[] {
  const sorted = [...list].sort((a, b) => b.ts - a.ts);
  const kept = new Map<string, KeptBy[]>();
  const mark = (k: string, rule: KeptBy) => kept.set(k, [...(kept.get(k) ?? []), rule]);
  sorted.slice(0, r.last).forEach((a) => mark(a.key, "last"));
  for (const rule of ["daily", "weekly", "monthly"] as const) {
    const seen = new Set<string>();
    for (const a of sorted) {
      const b = bucket(a.ts, rule);
      if (seen.has(b)) continue;
      if (seen.size >= r[rule]) break;
      seen.add(b);
      mark(a.key, rule);
    }
  }
  return sorted
    .filter((a) => kept.has(a.key))
    .map((a) => ({ key: a.key, ts: iso(a.ts), size: a.size, kept_by: kept.get(a.key)! }));
}

// ---- state ----
const T0 = now();
const projects = new Map<string, MockProject>();
const archives = new Map<string, Map<string, { ts: number; size: number; key: string }[]>>();
const globalsArch = new Map<string, { ts: number; size: number; key: string }[]>();
let runs: RunDetail[] = [];
const active = new Map<string, Active>();
let alerts: Alert[] = [];
let nextRunId = 1;
let nextAlertId = 1;
const dismissed = new Set<number>();

function key(project: string, db: string, t: number) {
  const engine = projects.get(project)!.config.engine;
  return `${PREFIX}/${project}/${encodeURIComponent(db)}/${tsKey(t)}${suffixFor(engine, db)}`;
}
function addArchive(project: string, db: string, t: number, size: number) {
  const m = archives.get(project) ?? new Map();
  archives.set(project, m);
  const list = m.get(db) ?? [];
  list.push({ ts: t, size, key: key(project, db, t) });
  m.set(db, list);
}
function prune(project: string) {
  const p = projects.get(project)!;
  const m = archives.get(project);
  if (m) for (const [db, list] of m) {
    const keep = new Set(applyRetention(list, p.config.retention).map((a) => a.key));
    m.set(db, list.filter((a) => keep.has(a.key)));
  }
  const g = globalsArch.get(project);
  if (g) {
    const keep = new Set(applyRetention(g, p.config.retention).map((a) => a.key));
    globalsArch.set(project, g.filter((a) => keep.has(a.key)));
  }
}

function seed() {
  const r = rand(42);
  const base = (name: string, i: number) =>
    name === "forge-conductor--staging" ? 1.5 * GB : name === "forge-sessions--staging" ? 310 * MB : (2 + ((i * 37) % 90)) * MB + Math.round(r() * 900_000);

  // infra-staging — MongoDB, 23 databases, daily at 02:10 UTC, last run partial, a run in progress.
  const infra: ProjectConfig = {
    name: "infra-staging", engine: "mongodb", databases: "all", interval: "1d", timeout: "6h",
    retention: { last: 7, daily: 7, weekly: 4, monthly: 6 }, retries: 2, shrink_warn_percent: 20,
    dump_args: ["--numParallelCollections=1"], globals: false,
  };
  projects.set(infra.name, { config: infra, uriSet: true, serverDbs: INFRA_DBS, sizes: Object.fromEntries(INFRA_DBS.map((d, i) => [d, base(d, i)])) });

  const billing: ProjectConfig = {
    name: "billing", engine: "postgres", databases: ["billing", "reports"], interval: "6h", timeout: "2h",
    retention: { last: 8, daily: 7, weekly: 4, monthly: 6 }, retries: 2, shrink_warn_percent: 25,
    dump_args: ["--exclude-table-data=public.audit_trail"], globals: true,
  };
  projects.set(billing.name, { config: billing, uriSet: true, serverDbs: ["billing", "reports", "sandbox"], sizes: { billing: 742 * MB, reports: 96 * MB } });

  const shop: ProjectConfig = {
    name: "shop-legacy", engine: "mariadb", databases: "all", interval: "1d", timeout: "3h",
    retention: { last: 7, daily: 7, weekly: 4, monthly: 6 }, retries: 1, shrink_warn_percent: 20, dump_args: [], globals: false,
  };
  projects.set(shop.name, { config: shop, uriSet: true, serverDbs: ["shop", "shop_archive", "wp_blog"], sizes: {} });

  // blog — s3import: a sidecar in AWS uploads daily at 03:00 UTC, the NAS pulls every 6h (3 of 4 runs: unchanged).
  const blog: ProjectConfig = {
    name: "blog", engine: "s3import", databases: "all", interval: "6h", timeout: "2h",
    retention: { last: 7, daily: 14, weekly: 8, monthly: 12 }, retries: 2, shrink_warn_percent: 20, dump_args: [], globals: false,
  };
  projects.set(blog.name, { config: blog, uriSet: true, serverDbs: ["@content", "ghost"], sizes: { "@content": 184 * MB, ghost: 6.4 * MB } });

  const pending: RunDetail[] = [];
  const day = 86400_000;
  const today0210 = (() => { const d = new Date(T0); d.setUTCHours(2, 10, 0, 0); return d.getTime(); })();
  const firstInfra = today0210 > T0 - 3600_000 ? today0210 - day : today0210;

  // Infra: 200 days of daily candidates; retention then trims them down like the real service.
  for (let k = 199; k >= 0; k--) {
    const start = firstInfra - k * day;
    const rr = rand(1000 + k);
    let status: RunDetail["status"] = "ok";
    if (k === 0) status = "partial";
    else if (k === 9) status = "failed";
    else if (k === 15) status = "interrupted";
    const dbRuns: DbRun[] = [];
    let secs = 0;
    for (const db of INFRA_DBS) {
      const sz = Math.round(projects.get(infra.name)!.sizes[db]! * (1 - k * 0.0015) * (0.97 + rr() * 0.06));
      const s = db === "forge-conductor--staging" ? 410 + rr() * 60 : 3 + rr() * 40;
      secs += s;
      if (status === "failed") continue;
      if (status === "interrupted" && db > "geo") break;
      const fail = status === "partial" && db === FAILED_DB;
      dbRuns.push({
        database: db, status: fail ? "failed" : "ok", bytes: fail ? 0 : sz, seconds: Math.round(s * 10) / 10,
        attempts: fail ? 3 : db === "forge-sessions--staging" && k === 3 ? 2 : 1,
        error: fail ? "mongodump exited with code 1: Failed: connection closed" : null, key: fail ? null : key(infra.name, db, start + Math.round(secs * 1000)),
      });
      if (!fail && k < 200) addArchive(infra.name, db, start + Math.round(secs * 1000), sz);
    }
    if (k < 60) {
      const ok = dbRuns.filter((d) => d.status === "ok");
      pending.push({
        id: 0, project: infra.name, trigger: k === 4 ? "manual" : "schedule", status,
        started_at: iso(start), finished_at: iso(start + Math.round(secs * 1000) + 4000),
        error: status === "failed" ? "S3 error (list): dial tcp 192.168.0.131:9000: connect: no route to host"
          : status === "interrupted" ? "service stopped during run (SIGTERM)" : null,
        warnings: k === 0 ? ["sandbox-old: database no longer exists on the server (old archives kept)"] : [],
        db_ok: ok.length, db_failed: dbRuns.length - ok.length, bytes: ok.reduce((s, d) => s + d.bytes, 0), databases: dbRuns,
      });
    }
  }

  // Billing: every 6h, with globals.
  const six = 6 * 3600_000;
  const lastBilling = Math.floor((T0 - 40 * 60_000) / six) * six + 15 * 60_000;
  for (let k = 799; k >= 0; k--) {
    const start = lastBilling - k * six;
    const rr = rand(5000 + k);
    const dbRuns: DbRun[] = [];
    let secs = 0;
    for (const db of ["billing", "reports"]) {
      const shrink = k === 2 && db === "reports";
      const sz = Math.round(projects.get(billing.name)!.sizes[db]! * (1 - k * 0.0002) * (shrink ? 0.64 : 0.98 + rr() * 0.04));
      const s = db === "billing" ? 48 + rr() * 9 : 7 + rr() * 3;
      secs += s;
      const t = start + Math.round(secs * 1000);
      addArchive(billing.name, db, t, sz);
      dbRuns.push({ database: db, status: "ok", bytes: sz, seconds: Math.round(s * 10) / 10, attempts: 1, error: null, key: key(billing.name, db, t) });
    }
    const g = globalsArch.get(billing.name) ?? [];
    g.push({ ts: start + 1500, size: 14_200 + Math.round(rr() * 300), key: `${PREFIX}/billing/@globals/${tsKey(start + 1500)}.sql.gz` });
    globalsArch.set(billing.name, g);
    if (k < 80) {
      pending.push({
        id: 0, project: billing.name, trigger: "schedule", status: "ok", started_at: iso(start), finished_at: iso(start + Math.round(secs * 1000) + 2500),
        error: null, warnings: k === 2 ? ["billing/reports: archive smaller than the previous one: 96.1 MB → 61.5 MB (−36.0%)"] : [],
        db_ok: 2, db_failed: 0, bytes: dbRuns.reduce((s, d) => s + d.bytes, 0), databases: dbRuns,
      });
    }
  }

  // Blog: daily source archives for 400 days; the last 12 runs (3 days) in history.
  const day3 = (() => { const d = new Date(T0); d.setUTCHours(3, 0, 0, 0); return d.getTime() > T0 ? d.getTime() - day : d.getTime(); })();
  for (let k = 399; k >= 0; k--) {
    for (const db of ["@content", "ghost"]) {
      addArchive(blog.name, db, day3 - k * day, Math.round(projects.get(blog.name)!.sizes[db]! * (1 - k * 0.001)));
    }
  }
  const lastBlog = Math.floor((T0 - 50 * 60_000) / six) * six + 20 * 60_000;
  for (let k = 11; k >= 0; k--) {
    const start = lastBlog - k * six;
    const src = Math.floor((start - day3) / day) * day + day3;
    const fresh = start - src < six;
    const dbRuns: DbRun[] = ["@content", "ghost"].map((db) => ({
      database: db, status: fresh ? "ok" : "unchanged", bytes: Math.round(projects.get(blog.name)!.sizes[db]!),
      seconds: fresh ? (db === "ghost" ? 1.8 : 14.2) : 0.3, attempts: 1, error: null, key: key(blog.name, db, src),
    }));
    pending.push({
      id: 0, project: blog.name, trigger: "schedule", status: "ok", started_at: iso(start), finished_at: iso(start + (fresh ? 17_000 : 900)),
      error: null, warnings: [], db_ok: 2, db_failed: 0, bytes: dbRuns.reduce((s, d) => s + d.bytes, 0), databases: dbRuns,
    });
  }

  // legacy-crm — overdue: the last success 4 days ago, every daily run since has failed.
  const crm: ProjectConfig = {
    name: "legacy-crm", engine: "mongodb", databases: "all", interval: "1d", timeout: "2h",
    retention: { last: 7, daily: 7, weekly: 4, monthly: 6 }, retries: 1, shrink_warn_percent: 20, dump_args: [], globals: false,
  };
  projects.set(crm.name, { config: crm, uriSet: true, serverDbs: ["crm", "crm-files"], sizes: { crm: 210 * MB, "crm-files": 34 * MB } });
  for (let k = 7; k >= 0; k--) {
    const start = today0210 - k * day + (today0210 > T0 ? -day : 0) + 15 * 60_000;
    const ok = k >= 4;
    const dbRuns: DbRun[] = ok ? ["crm", "crm-files"].map((db) => ({
      database: db, status: "ok", bytes: projects.get(crm.name)!.sizes[db]!, seconds: 12.5, attempts: 1, error: null, key: key(crm.name, db, start),
    })) : [];
    if (ok) for (const d of dbRuns) addArchive(crm.name, d.database, start, d.bytes);
    pending.push({
      id: 0, project: crm.name, trigger: "schedule", status: ok ? "ok" : "failed", started_at: iso(start), finished_at: iso(start + (ok ? 31_000 : 20_000)),
      error: ok ? null : "mongodump exited with code 1: Failed: error connecting to db server: server selection timeout",
      warnings: [], db_ok: ok ? 2 : 0, db_failed: 0, bytes: dbRuns.reduce((x, d) => x + d.bytes, 0), databases: dbRuns,
    });
  }

  // MOCK_MANY=N — N extra small projects to see the overview at scale; every 9th one is overdue, every 7th failed.
  const many = Number(process.env.MOCK_MANY ?? 0);
  for (let i = 1; i <= many; i++) {
    const name = `svc-${String(i).padStart(3, "0")}`;
    const engine: EngineId = (["mongodb", "postgres", "mariadb"] as const)[i % 3]!;
    projects.set(name, {
      config: { name, engine, databases: "all", interval: "1d", timeout: "1h", retention: { last: 7, daily: 7, weekly: 4, monthly: 3 }, retries: 1, shrink_warn_percent: 20, dump_args: [], globals: false },
      uriSet: true, serverDbs: ["app"], sizes: { app: (5 + (i % 40)) * MB },
    });
    const stale = i % 9 === 0 ? 3 + (i % 4) : 0;
    for (let k = 2 + stale; k >= 0; k--) {
      const start = today0210 - k * day + (today0210 > T0 ? -day : 0) + i * 60_000;
      const status: RunDetail["status"] = k < stale || (k === 0 && i % 7 === 0) ? "failed" : k === 0 && i % 11 === 0 ? "partial" : "ok";
      const bytes = status === "ok" ? projects.get(name)!.sizes.app! : 0;
      if (bytes) addArchive(name, "app", start, bytes);
      pending.push({
        id: 0, project: name, trigger: "schedule", status, started_at: iso(start), finished_at: iso(start + 8_000),
        error: status === "failed" ? "connection refused" : null, warnings: status === "partial" ? ["app: archive 40% smaller than the previous one"] : [],
        db_ok: status === "failed" ? 0 : 1, db_failed: status === "failed" ? 1 : 0, bytes,
        databases: [{ database: "app", status: status === "failed" ? "failed" : "ok", bytes, seconds: 7.1, attempts: 1, error: status === "failed" ? "connection refused" : null, key: bytes ? key(name, "app", start) : null }],
      });
    }
  }

  pending.sort((a, b) => Date.parse(a.started_at) - Date.parse(b.started_at));
  for (const r of pending) r.id = nextRunId++;
  runs = pending;
  for (const p of projects.keys()) prune(p);

  alerts = [
    { id: 0, ts: iso(Date.parse(runs.filter((r) => r.project === "billing" && r.warnings.length)[0]!.finished_at!)), level: "warning", project: "billing", text: "billing/reports: archive smaller than the previous one: 96.1 MB → 61.5 MB (−36.0%)" },
    { id: 0, ts: iso(firstInfra - 9 * day + 600_000), level: "error", project: "infra-staging", text: "infra-staging: S3 error (list): dial tcp 192.168.0.131:9000: connect: no route to host" },
    { id: 0, ts: iso(firstInfra - 15 * day + 300_000), level: "warning", project: "infra-staging", text: "infra-staging/forge-sessions--staging: archive smaller than the previous one: 2.1 GB → 1.2 GB (−42.9%)" },
    { id: 0, ts: iso(Date.parse(runs.filter((r) => r.project === "infra-staging").at(-1)!.finished_at!)), level: "error", project: "infra-staging", text: `infra-staging/${FAILED_DB}: mongodump exited with code 1: Failed: connection closed` },
    { id: 0, ts: iso(T0 - 3 * 3600_000), level: "error", project: "legacy-crm", text: "legacy-crm: no successful backup for 3 days (schedule: every 1d)" },
  ].sort((a, b) => Date.parse(a.ts) - Date.parse(b.ts)).map((a) => ({ ...a, id: nextAlertId++ }));

  // A run already in progress on infra-staging (manual, started 70 s ago).
  startRun("infra-staging", "manual", { startedAt: T0 - 70_000, queueMs: 0, discoverMs: 3000, perDbMs: 9000, retryDb: "forge-sessions--staging" });
}

function startRun(project: string, trigger: "manual" | "schedule", o: Partial<Active> = {}) {
  const p = projects.get(project)!;
  const dbs = p.config.databases === "all" ? p.serverDbs : p.config.databases;
  const id = nextRunId++;
  const a: Active = { project, runId: id, startedAt: o.startedAt ?? now(), queueMs: o.queueMs ?? 2500, discoverMs: o.discoverMs ?? 2000, perDbMs: o.perDbMs ?? 2500, dbs, retryDb: o.retryDb };
  active.set(project, a);
  runs.push({ id, project, trigger, status: "running", started_at: iso(a.startedAt), finished_at: null, error: null, warnings: [], db_ok: 0, db_failed: 0, bytes: 0, databases: [] });
  return id;
}

function progress(a: Active, t = now()) {
  const el = t - a.startedAt;
  if (el < a.queueMs) return { phase: "queued" as const, idx: 0, frac: 0 };
  if (el < a.queueMs + a.discoverMs) return { phase: "discover" as const, idx: 0, frac: 0 };
  const d = el - a.queueMs - a.discoverMs;
  const idx = Math.floor(d / a.perDbMs);
  if (idx < a.dbs.length) {
    const frac = (d % a.perDbMs) / a.perDbMs;
    return { phase: frac < 0.78 ? ("dump" as const) : ("upload" as const), idx, frac };
  }
  if (d < a.dbs.length * a.perDbMs + 1500) return { phase: "prune" as const, idx: a.dbs.length, frac: 0 };
  return { phase: "done" as const, idx: a.dbs.length, frac: 0 };
}

/** Advances simulated runs; called on every request. */
export function tick() {
  const t = now();
  for (const a of [...active.values()]) {
    const run = runs.find((r) => r.id === a.runId)!;
    const p = projects.get(a.project)!;
    const pr = progress(a, t);
    const doneCount = Math.min(pr.idx, a.dbs.length);
    while (run.databases.length < doneCount) {
      const db = a.dbs[run.databases.length]!;
      const size = Math.round((p.sizes[db] ?? (20 + Math.random() * 200) * MB) * (0.99 + Math.random() * 0.03));
      p.sizes[db] = size;
      const ts = a.startedAt + a.queueMs + a.discoverMs + (run.databases.length + 1) * a.perDbMs;
      run.databases.push({ database: db, status: "ok", bytes: size, seconds: a.perDbMs / 1000, attempts: db === a.retryDb ? 2 : 1, error: null, key: key(a.project, db, ts) });
      run.db_ok++;
      run.bytes += size;
      addArchive(a.project, db, ts, size);
    }
    if (pr.phase === "done") {
      if (p.config.globals) {
        const g = globalsArch.get(a.project) ?? [];
        g.push({ ts: a.startedAt + a.queueMs + 1500, size: 14_400, key: `${PREFIX}/${a.project}/@globals/${tsKey(a.startedAt + a.queueMs + 1500)}.sql.gz` });
        globalsArch.set(a.project, g);
      }
      prune(a.project);
      run.status = "ok";
      run.finished_at = iso(t);
      active.delete(a.project);
      alerts.push({ id: nextAlertId++, ts: iso(t), level: "info", project: a.project, text: `Run #${run.id} finished: ${run.db_ok} ${run.db_ok === 1 ? "database" : "databases"} saved` });
      alerts = alerts.slice(-50);
    }
  }
}

function stop(name: string, reason: string) {
  const a = active.get(name);
  if (!a) return;
  const run = runs.find((r) => r.id === a.runId);
  if (run) {
    run.status = "interrupted";
    run.error = reason;
    run.finished_at = iso(now());
  }
  active.delete(name);
}

function runningOp(a: Active): RunningOp {
  const pr = progress(a);
  const inDb = pr.phase === "dump" || pr.phase === "upload";
  const db = inDb ? a.dbs[pr.idx] ?? null : null;
  const dbStart = a.startedAt + a.queueMs + a.discoverMs + pr.idx * a.perDbMs;
  return {
    project: a.project, run_id: a.runId, started_at: iso(a.startedAt), database: db,
    db_index: pr.phase === "queued" || pr.phase === "discover" ? 0 : Math.min(pr.idx + 1, a.dbs.length),
    db_total: pr.phase === "queued" || pr.phase === "discover" ? 0 : a.dbs.length,
    db_started_at: inDb ? iso(dbStart) : null,
    attempt: db && db === a.retryDb && pr.frac > 0.4 ? 2 : 1,
    phase: pr.phase === "done" ? "prune" : pr.phase,
    ...liveDbs(a, pr),
  };
}

/** The run's databases with states, and the bytes of the current dump (previous archive size as the estimate). */
function liveDbs(a: Active, pr: ReturnType<typeof progress>): Pick<RunningOp, "databases" | "bytes_done" | "bytes_expected" | "bytes_per_sec"> {
  if (pr.phase === "queued" || pr.phase === "discover") return {};
  const p = projects.get(a.project)!;
  const run = runs.find((r) => r.id === a.runId);
  const databases = a.dbs.map((db, i) => {
    const r = run?.databases.find((d) => d.database === db);
    if (r) return { database: db, status: r.status as "ok", bytes: r.bytes, seconds: r.seconds, attempts: r.attempts, error: r.error };
    const cur = i === pr.idx && (pr.phase === "dump" || pr.phase === "upload");
    return { database: db, status: cur ? ("running" as const) : ("pending" as const), bytes: 0, seconds: 0, attempts: cur ? 1 : 0, error: null };
  });
  const db = a.dbs[pr.idx];
  const expected = db ? p.sizes[db] ?? 0 : 0;
  const dumpFrac = Math.min(1, pr.frac / 0.78);
  const secs = (dumpFrac * a.perDbMs * 0.78) / 1000;
  const done = pr.phase === "dump" ? Math.round(expected * dumpFrac) : 0;
  return { databases, bytes_done: done, bytes_expected: expected, bytes_per_sec: secs >= 2 ? Math.round(done / secs) : 0 };
}

const summaryOf = ({ databases: _d, ...r }: RunDetail): RunSummary => r;

function projectSummary(p: MockProject): ProjectSummary {
  const name = p.config.name;
  const pr = runs.filter((r) => r.project === name);
  const last = pr.at(-1) ?? null;
  const lastOk = [...pr].reverse().find((r) => r.status === "ok");
  const a = active.get(name);
  const m = archives.get(name);
  let total = 0, count = 0;
  if (m) for (const l of m.values()) { count += l.length; total += l.reduce((s, x) => s + x.size, 0); }
  const g = globalsArch.get(name) ?? [];
  count += g.length; total += g.reduce((s, x) => s + x.size, 0);
  const intervalMs = parseDurationMs(p.config.interval) ?? 86400_000;
  const timeoutMs = parseDurationMs(p.config.timeout) ?? 3600_000;
  const overdueAfter = Math.max(2 * intervalMs, intervalMs + timeoutMs + 3600_000);
  const ref = lastOk ? Date.parse(lastOk.finished_at!) : pr.length ? Date.parse(pr[0]!.started_at) : null;
  const overdueSince = ref != null && ref + overdueAfter <= now() ? iso(ref + overdueAfter) : null;
  const nextDue = lastOk ? Date.parse(lastOk.started_at) + intervalMs : T0 + 12 * 60_000;
  return {
    name, engine: p.config.engine, databases: p.config.databases, interval: p.config.interval, start_at: p.config.start_at ?? null, uri_set: p.uriSet,
    state: a ? (progress(a).phase === "queued" ? "queued" : "running") : "idle",
    last_run: last ? summaryOf(last) : null,
    last_success_at: lastOk?.finished_at ?? null,
    next_due: iso(nextDue),
    total_bytes: total, archive_count: count,
    database_count: p.config.databases === "all" ? (last ? p.serverDbs.length : 0) : p.config.databases.length,
    overdue_since: overdueSince,
    overdue_after: Math.round(overdueAfter / 1000),
  };
}

export function parseDurationMs(s: string): number | null {
  const m = /^(?:(\d+)w)?(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?$/.exec(s);
  if (!m || !s) return null;
  const [w, d, h, mi] = [m[1], m[2], m[3], m[4]].map((x) => (x ? +x : 0)) as [number, number, number, number];
  return ((w * 7 + d) * 1440 + h * 60 + mi) * 60_000;
}

// ---- public API used by the plugin ----
export const db = {
  overview(): Overview {
    tick();
    const empty = process.env.MOCK_EMPTY === "1";
    return {
      projects: empty ? [] : [...projects.values()].map(projectSummary),
      running: empty ? [] : [...active.values()].map(runningOp),
      storage: process.env.MOCK_STORAGE_DOWN === "1"
        ? { kind: "s3", location: "s3://db-backups/atlas", ok: false, error: "dial tcp 192.168.0.131:9000: connect: connection refused" }
        : { kind: "s3", location: "s3://db-backups/atlas", ok: true, error: null,
            disk: { total: 7.27e12, used: 4.86e12, free: 2.41e12, growth_per_day: 9.4e9, days_left: 256, since: "2026-10-01" } },
      alerts: empty ? [] : [...alerts].filter((a) => !dismissed.has(a.id)).reverse().slice(0, 20),
    };
  },
  dismissAlert(id: number) {
    if (!alerts.some((a) => a.id === id)) return false;
    dismissed.add(id);
    return true;
  },
  dismissAllAlerts() {
    for (const a of alerts) dismissed.add(a.id);
  },
  get(name: string) {
    const p = projects.get(name);
    return p ? { config: p.config, revision: revisionOf(p.config), uri_set: p.uriSet } : null;
  },
  names: () => [...projects.keys()],
  create(config: ProjectConfig, uri?: string) {
    projects.set(config.name, { config, uriSet: !!uri, serverDbs: fakeDbs(config.engine), sizes: {} });
    return revisionOf(config);
  },
  update(config: ProjectConfig) {
    const p = projects.get(config.name)!;
    p.config = config;
    prune(config.name);
    return revisionOf(config);
  },
  remove(name: string) {
    stop(name, "cancelled: the project was deleted");
    projects.delete(name);
  },
  cancel(name: string) {
    const a = active.get(name);
    if (!a) return null;
    stop(name, "cancelled from the UI");
    return a.runId;
  },
  setSecret(name: string) {
    projects.get(name)!.uriSet = true;
  },
  isBusy: (name: string) => active.has(name),
  run: (name: string) => startRun(name, "manual"),
  runs(name: string, limit: number, beforeId?: number): RunDetail[] {
    tick();
    return runs.filter((r) => r.project === name && (beforeId == null || r.id < beforeId)).sort((a, b) => b.id - a.id).slice(0, limit);
  },
  allRuns(q: { project: string | null; statuses: string[] | null; since: number | null; limit: number; beforeId?: number }): RunSummary[] {
    tick();
    return runs
      .filter((r) => (!q.project || r.project === q.project) && (!q.statuses || q.statuses.includes(r.status))
        && (q.since == null || Date.parse(r.started_at) >= q.since) && (q.beforeId == null || r.id < q.beforeId))
      .sort((a, b) => b.id - a.id).slice(0, q.limit).map(summaryOf);
  },
  telegram: { chat_id: "123456789", token_set: true } as { chat_id: string; token_set: boolean } | null,
  collections(key: string) {
    // Deterministic fake inventory: one dominant collection, a few medium ones, a long tail.
    let seed = [...key].reduce((h, c) => (h * 31 + c.charCodeAt(0)) >>> 0, 7);
    const rnd = () => ((seed = (seed * 1103515245 + 12345) >>> 0) % 10000) / 10000;
    if (key.includes("audit")) return { key, database: null, collections: null, previous: null };
    const names = ["turn_streams", "agent_turn_queue", "workflow_runs", "chat_messages", "org_memory", "chat_sessions", "egress_log",
      "turn_metrics", "secret_audit", "agents", "agent_memories", "contexts", "workflows", "secrets", "turn_logs", "issues", "jobs", "repositories"];
    const collections = names.map((name, i) => {
      const bytes = Math.round((i === 0 ? 4e9 : 3e8 / (i + 1) ** 1.6) * (0.5 + rnd()));
      return { name, docs: Math.round(bytes / (2000 + rnd() * 50000)), bytes, storage_bytes: Math.round(bytes * 0.35), index_bytes: Math.round(2e5 + rnd() * 5e6) };
    }).sort((a, b) => b.bytes - a.bytes);
    // The previous backup: a little smaller, without the newest collection, with one since dropped.
    const prev = collections.slice(0, -1).map((c) => {
      const k = 0.9 + rnd() * 0.12;
      return { ...c, bytes: Math.round(c.bytes * k), docs: Math.round(c.docs * k), storage_bytes: Math.round(c.storage_bytes * k) };
    });
    prev.push({ name: "tmp_import", docs: 1200, bytes: 3_400_000, storage_bytes: 1_100_000, index_bytes: 40_000 });
    const ts = new Date(Date.now() - 86400e3).toISOString();
    return { key, database: key.split("/")[1] ?? "", collections, previous: { key: `${key}-prev`, ts, bytes: Math.round(collections.reduce((s, c) => s + c.bytes, 0) * 0.1), collections: prev } };
  },

  archives(name: string): ArchivesResponse {
    tick();
    const p = projects.get(name)!;
    const m = archives.get(name) ?? new Map();
    const dbs = [...m.entries()]
      .map(([database, list]) => {
        const arch = applyRetention(list, p.config.retention);
        return { database, total_bytes: arch.reduce((s, a) => s + a.size, 0), archives: arch };
      })
      .filter((d) => d.archives.length)
      .sort((a, b) => a.database.localeCompare(b.database));
    return { databases: dbs, globals: applyRetention(globalsArch.get(name) ?? [], p.config.retention), restore: { command: RESTORE[p.config.engine], by_suffix: RESTORE_BY_SUFFIX[p.config.engine] ?? {} } };
  },
  test(engine: EngineId, uri: string | null, project?: string): TestConnectionResult {
    if (uri && /fail|refused|bad/i.test(uri)) {
      return { ok: false, server_version: null, client_version: null, databases: [], warnings: [], error: "psql exited with code 2: psql: error: connection to server at \"db.example.net\", port 5432 failed: Connection refused" };
    }
    const dbs = project ? projects.get(project)!.serverDbs : fakeDbs(engine);
    const v: Record<EngineId, [string, string, string[]]> = {
      mongodb: ["7.0.14", "100.10.0", []],
      postgres: ["17.2", "17.6", project === "billing" ? [] : ["for a remote server, sslmode=require (or verify-full) is recommended"]],
      mariadb: ["10.6.19", "11.4.3", ["this looks like MySQL, not MariaDB: mariadb-dump usually copes, but verify a restore"]],
      s3import: ["S3 s3.eu-central-1.amazonaws.com/blog-backups", "1.35.0", []],
    };
    const [sv, cv, w] = v[engine];
    return { ok: true, server_version: sv, client_version: cv, databases: dbs, warnings: w, error: null };
  },
  settings(): AppSettings {
    return {
      storage: { kind: "s3", endpoint: "http://192.168.0.131:9000", bucket: "db-backups", prefix: PREFIX },
      notify: { telegram: db.telegram, webhook_set: process.env.MOCK_WEBHOOK === "1" },
      max_parallel: 1,
    };
  },
  /** Databases with archives (statistics are generated for them in mock/stats.ts). */
  statsDatabases(name: string): string[] {
    return [...(archives.get(name)?.keys() ?? [])].sort();
  },
  findArchive(k: string) {
    for (const m of archives.values()) for (const l of m.values()) { const a = l.find((x) => x.key === k); if (a) return a; }
    for (const l of globalsArch.values()) { const a = l.find((x) => x.key === k); if (a) return a; }
    return null;
  },
};

function fakeDbs(engine: EngineId) {
  if (engine === "postgres") return ["app", "app_analytics", "keycloak", "metabase"];
  if (engine === "mariadb") return ["shop", "shop_archive", "wp_blog"];
  if (engine === "s3import") return ["@content", "ghost"];
  return ["app", "app-events", "crm", "sessions"];
}

seed();
