// Vite dev-server middleware that serves a fake /api for `npm run dev:mock`.
import { randomBytes } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type { Plugin } from "vite";
import type { ProjectConfig, StatsDays, StatsMetric } from "../src/api/types";
import { db, META, parseDurationMs, revisionOf } from "./data";
import { objectStats, projectStats } from "./stats";

const PASSWORD = process.env.MOCK_PASSWORD ?? "admin";
const sessions = new Map<string, string>(); // sid → csrf
const failures: number[] = [];
let tgToken: string | null = "123456789:mock-stored-token";
let tgChecks = 0;

function send(res: ServerResponse, status: number, body?: unknown) {
  res.statusCode = status;
  if (body === undefined) return res.end();
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(body));
}
const err = (res: ServerResponse, status: number, error: string, field?: string) => send(res, status, field ? { error, field } : { error });

async function readJson(req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  if (!chunks.length) return null;
  try { return JSON.parse(Buffer.concat(chunks).toString("utf8")); } catch { return undefined; }
}

function sid(req: IncomingMessage) {
  const m = /(?:^|;\s*)mock_sid=([^;]+)/.exec(req.headers.cookie ?? "");
  return m && sessions.has(m[1]!) ? m[1]! : null;
}

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

function validateConfig(c: unknown): { error: string; field: string } | null {
  if (!c || typeof c !== "object") return { error: "config must be an object", field: "config" };
  const x = c as ProjectConfig;
  if (typeof x.name !== "string" || !NAME_RE.test(x.name)) return { error: "invalid project name", field: "name" };
  const eng = META.engines.find((e) => e.id === x.engine);
  if (!eng) return { error: "unknown engine", field: "engine" };
  if (!(x.databases === "all" || (Array.isArray(x.databases) && x.databases.length > 0 && x.databases.every((d) => typeof d === "string" && d))))
    return { error: "databases must be \"all\" or a non-empty list", field: "databases" };
  for (const f of ["interval", "timeout"] as const) {
    const ms = typeof x[f] === "string" ? parseDurationMs(x[f]) : null;
    if (ms == null || ms < 60_000) return { error: `invalid duration: ${String(x[f])}`, field: f };
  }
  const r = x.retention;
  if (!r) return { error: "retention required", field: "retention" };
  for (const k of ["last", "daily", "weekly", "monthly"] as const)
    if (!Number.isInteger(r[k]) || r[k] < 0) return { error: "must be an integer >= 0", field: `retention.${k}` };
  if (r.last + r.daily + r.weekly + r.monthly === 0) return { error: "at least one retention rule must be > 0", field: "retention" };
  if (!Number.isInteger(x.retries) || x.retries < 0 || x.retries > 10) return { error: "retries must be 0..10", field: "retries" };
  if (!Number.isInteger(x.shrink_warn_percent) || x.shrink_warn_percent < 0 || x.shrink_warn_percent > 99)
    return { error: "shrink_warn_percent must be 0..99", field: "shrink_warn_percent" };
  if (!Array.isArray(x.dump_args)) return { error: "dump_args must be a list", field: "dump_args" };
  for (const a of x.dump_args) {
    const flag = String(a).split("=")[0]!;
    if (!/^--[^=\s]+=.+$/.test(String(a)) || !eng.dump_args.includes(flag)) return { error: `argument not allowed for ${eng.id}: ${String(a)}`, field: "dump_args" };
  }
  return null;
}

async function handle(req: IncomingMessage, res: ServerResponse) {
  const url = new URL(req.url ?? "/", "http://mock");
  const path = url.pathname;
  const method = req.method ?? "GET";
  const s = sid(req);

  // Small artificial latency so loading states are visible.
  await new Promise((r) => setTimeout(r, 80 + Math.random() * 160));

  if (path === "/api/session" && method === "GET") {
    return send(res, 200, { authenticated: !!s, csrf: s ? sessions.get(s)! : null, password_set: process.env.MOCK_NO_PASSWORD !== "1" });
  }
  if (path === "/api/login" && method === "POST") {
    const t = Date.now();
    while (failures.length && failures[0]! < t - 60_000) failures.shift();
    if (failures.length >= 5) return err(res, 429, "too many attempts");
    const body = (await readJson(req)) as { password?: string } | null;
    if (process.env.MOCK_NO_PASSWORD === "1" || !body || body.password !== PASSWORD) {
      failures.push(t);
      return err(res, 401, "invalid password");
    }
    const id = randomBytes(16).toString("hex");
    sessions.set(id, randomBytes(16).toString("hex"));
    res.setHeader("Set-Cookie", `mock_sid=${id}; Path=/; HttpOnly; SameSite=Strict`);
    return send(res, 204);
  }

  if (!s) return err(res, 401, "unauthorized");
  if (method !== "GET" && req.headers["x-csrf-token"] !== sessions.get(s)) return err(res, 403, "csrf token missing or invalid");

  if (path === "/api/logout" && method === "POST") {
    sessions.delete(s);
    res.setHeader("Set-Cookie", "mock_sid=; Path=/; Max-Age=0");
    return send(res, 204);
  }
  if (path === "/api/meta" && method === "GET") return send(res, 200, META);
  if (path === "/api/overview" && method === "GET") return send(res, 200, db.overview());
  if (path === "/api/settings" && method === "GET") return send(res, 200, db.settings());
  if (path === "/api/alerts/dismiss-all" && method === "POST") {
    db.dismissAllAlerts();
    return send(res, 204);
  }
  const am = /^\/api\/alerts\/(\d+)\/dismiss$/.exec(path);
  if (am && method === "POST") return db.dismissAlert(Number(am[1])) ? send(res, 204) : err(res, 404, `no notification #${am[1]}`);
  if (path === "/api/settings/test-notify" && method === "POST") return send(res, 200, { ok: true });

  // Telegram: a token containing "bad" is rejected; chats appear on the second "Find chat" (to show the empty hint).
  if (path === "/api/settings/telegram/check" && method === "POST") {
    const b = ((await readJson(req)) ?? {}) as { token?: string };
    const token = b.token?.trim() || tgToken;
    if (!token) return err(res, 422, "bot token is not saved yet — paste it", "token");
    if (/bad/i.test(token) || !/^\d+:[\w-]{10,}$/.test(token)) return err(res, 422, "Telegram: Unauthorized — check the token", "token");
    tgChecks++;
    return send(res, 200, {
      bot: { username: "nas_backup_bot", name: "NAS backup" },
      chats: tgChecks < 2 ? [] : [
        { id: "123456789", type: "private", name: "Vitaly Nikitin", username: "vit" },
        { id: "-1001987654321", type: "supergroup", name: "Ops alerts", username: "" },
      ],
    });
  }
  if (path === "/api/settings/telegram" && method === "PUT") {
    const b = ((await readJson(req)) ?? {}) as { token?: string; chat_id?: string };
    const chat = String(b.chat_id ?? "").trim();
    if (!/^-?\d{1,20}$|^@[A-Za-z0-9_]{5,32}$/.test(chat)) return err(res, 422, "chat_id must be a number (negative for groups and channels) or @channel_name", "chat_id");
    const token = b.token?.trim() || tgToken;
    if (!token) return err(res, 422, "bot token is not saved yet — paste it", "token");
    if (chat === "-1") return err(res, 422, "could not send a message to this chat: Bad Request: chat not found", "chat_id");
    tgToken = token;
    db.telegram = { chat_id: chat, token_set: true };
    return send(res, 200, { ok: true });
  }
  if (path === "/api/settings/telegram" && method === "DELETE") {
    tgToken = null;
    tgChecks = 0;
    db.telegram = null;
    return send(res, 204);
  }

  if (path === "/api/runs" && method === "GET") {
    const st = url.searchParams.get("status");
    const all = ["ok", "partial", "failed", "interrupted", "running"];
    const statuses = !st ? null : st === "problem" ? ["partial", "failed", "interrupted"] : st.split(",").map((x) => x.trim());
    if (statuses && statuses.some((x) => !all.includes(x))) return err(res, 422, "status must be ok, partial, failed, interrupted, running or problem", "status");
    const sinceRaw = url.searchParams.get("since");
    const since = sinceRaw ? Date.parse(sinceRaw) : null;
    if (sinceRaw && Number.isNaN(since)) return err(res, 422, "since must be an ISO 8601 date", "since");
    const limit = Math.min(200, Math.max(1, Number(url.searchParams.get("limit") ?? 50) || 50));
    const before = url.searchParams.get("before_id");
    return send(res, 200, {
      runs: db.allRuns({ project: url.searchParams.get("project") || null, statuses, since, limit, beforeId: before ? Number(before) : undefined }),
      projects: db.names().sort(),
    });
  }

  if (path === "/api/test-connection" && method === "POST") {
    const b = (await readJson(req)) as { engine?: string; uri?: string; project?: string } | null;
    if (!b) return err(res, 400, "invalid json");
    await new Promise((r) => setTimeout(r, 700));
    if (b.project) {
      const p = db.get(b.project);
      if (!p) return err(res, 404, "project not found");
      if (!p.uri_set) return err(res, 422, "connection string is not set", "uri");
      return send(res, 200, db.test(p.config.engine, null, b.project));
    }
    if (!b.engine || !META.engines.some((e) => e.id === b.engine)) return err(res, 422, "unknown engine", "engine");
    if (!b.uri) return err(res, 422, "uri required", "uri");
    return send(res, 200, db.test(b.engine as ProjectConfig["engine"], b.uri));
  }

  if (path === "/api/download" && method === "GET") {
    const k = url.searchParams.get("key") ?? "";
    const a = db.findArchive(k);
    if (!a) return err(res, 404, "archive not found");
    const name = k.split("/").pop() ?? "archive";
    res.statusCode = 200;
    res.setHeader("Content-Type", "application/octet-stream");
    res.setHeader("Content-Disposition", `attachment; filename="${name}"`);
    return res.end(`mock archive ${k} (${a.size} bytes in the real service)\n`);
  }

  if (path === "/api/projects" && method === "POST") {
    const b = (await readJson(req)) as { config?: ProjectConfig; uri?: string } | null;
    if (!b) return err(res, 400, "invalid json");
    const v = validateConfig(b.config);
    if (v) return err(res, 422, v.error, v.field);
    if (db.get(b.config!.name)) return err(res, 409, "project already exists", "name");
    if (b.uri && /^bad/i.test(b.uri)) return err(res, 422, "connection string scheme does not match engine", "uri");
    return send(res, 201, { revision: db.create(b.config!, b.uri) });
  }

  const oh = /^\/api\/projects\/([^/]+)\/stats\/object-history$/.exec(path);
  if (oh && method === "GET") {
    // Every collection grows towards its current size with some noise; a gap where a dump was missed.
    const database = url.searchParams.get("database") ?? "";
    const n = Number(url.searchParams.get("days") ?? 30);
    const cols = db.collections(`${oh[1]}/${database}/x`).collections ?? [];
    const days = Array.from({ length: n }, (_, i) => new Date(Date.now() - (n - 1 - i) * 86400e3).toISOString().slice(0, 10));
    const objects: Record<string, Record<string, (number | null)[]>> = {};
    cols.forEach((c, j) => {
      const rate = j % 5 === 3 ? -0.004 : 0.002 + (j % 4) * 0.004;
      const f = (v: number) => days.map((_, i) => (i === 9 ? null : Math.round(v / (1 + rate) ** (n - 1 - i) * (1 + Math.sin(i + j) * 0.01))));
      objects[c.name] = { records: f(c.docs), data: f(c.bytes), disk: f(c.storage_bytes), index: f(c.index_bytes) };
    });
    return send(res, 200, { tz: url.searchParams.get("tz") || "UTC", database, days, objects });
  }

  const sm = /^\/api\/projects\/([^/]+)\/stats(\/objects)?$/.exec(path);
  if (sm && method === "GET") {
    const name = decodeURIComponent(sm[1]!);
    const p = db.get(name);
    if (!p) return err(res, 404, `no project "${name}"`);
    const days = Number(url.searchParams.get("days") ?? 30);
    if (![7, 30, 90, 0].includes(days)) return err(res, 422, "period: 7, 30, 90 or 0 (all time)", "days");
    const tz = url.searchParams.get("tz") || "UTC";
    if (sm[2]) {
      const database = url.searchParams.get("database");
      if (!database) return err(res, 422, "database required", "database");
      const metric = url.searchParams.get("metric") ?? "data";
      if (!["archive", "data", "disk", "records"].includes(metric)) return err(res, 422, "metric: data, disk, records", "metric");
      return send(res, 200, objectStats(name, p.config.engine, database, { days: days as StatsDays, metric, tz }));
    }
    const metric = url.searchParams.get("metric") ?? "archive";
    if (!["archive", "data", "records"].includes(metric)) return err(res, 422, "metric: archive, data, records", "metric");
    return send(res, 200, projectStats(name, p.config.engine, db.statsDatabases(name), {
      days: days as StatsDays, metric: metric as StatsMetric, database: url.searchParams.get("database") || null, tz,
    }));
  }

  const m = /^\/api\/projects\/([^/]+)(?:\/(secret|run|cancel|runs|archives|collections))?$/.exec(path);
  if (m) {
    const name = decodeURIComponent(m[1]!);
    const sub = m[2];
    const p = db.get(name);
    if (!p) return err(res, 404, "project not found");
    if (!sub && method === "GET") return send(res, 200, p);
    if (!sub && method === "PUT") {
      const b = (await readJson(req)) as { config?: ProjectConfig; revision?: string } | null;
      if (!b) return err(res, 400, "invalid json");
      if (b.revision !== revisionOf(p.config)) return err(res, 409, "config was modified by someone else");
      const v = validateConfig(b.config);
      if (v) return err(res, 422, v.error, v.field);
      if (b.config!.name !== name) return err(res, 422, "renaming is not supported", "name");
      if (b.config!.engine !== p.config.engine) return err(res, 422, "engine cannot be changed", "engine");
      return send(res, 200, { revision: db.update(b.config!) });
    }
    if (!sub && method === "DELETE") {
      if (url.searchParams.get("revision") !== p.revision) return err(res, 409, "config was modified by someone else");
      db.remove(name);
      return send(res, 204);
    }
    if (sub === "secret" && method === "PUT") {
      const b = (await readJson(req)) as { uri?: string } | null;
      if (!b?.uri) return err(res, 422, "uri required", "uri");
      db.setSecret(name);
      return send(res, 204);
    }
    if (sub === "run" && method === "POST") {
      if (db.isBusy(name)) return err(res, 409, "run already in progress or queued");
      if (!p.uri_set) return err(res, 422, "connection string is not set", "uri");
      return send(res, 202, { run_id: db.run(name) });
    }
    if (sub === "cancel" && method === "POST") {
      const id = db.cancel(name);
      return id == null ? err(res, 409, `${name} has no active run`) : send(res, 202, { run_id: id });
    }
    if (sub === "runs" && method === "GET") {
      const limit = Math.min(200, Math.max(1, Number(url.searchParams.get("limit") ?? 50) || 50));
      const before = url.searchParams.get("before_id");
      return send(res, 200, { runs: db.runs(name, limit, before ? Number(before) : undefined) });
    }
    if (sub === "archives" && method === "GET") return send(res, 200, db.archives(name));
    if (sub === "collections" && method === "GET") return send(res, 200, db.collections(url.searchParams.get("key") ?? ""));
  }

  return err(res, 404, "not found");
}

export function mockApi(): Plugin {
  return {
    name: "db-backup-mock-api",
    configureServer(server) {
      server.config.logger.info(`\n  [mock] fake /api enabled — password: ${process.env.MOCK_NO_PASSWORD === "1" ? "(not set)" : PASSWORD}\n`);
      server.middlewares.use((req, res, next) => {
        if (!req.url?.startsWith("/api/")) return next();
        handle(req, res).catch((e: unknown) => {
          server.config.logger.error(`[mock] ${String(e)}`);
          if (!res.headersSent) err(res, 500, "mock server error");
        });
      });
    },
  };
}
