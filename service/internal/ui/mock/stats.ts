// Fake statistics for `npm run dev:mock`: ~120 days of plausible growth per database, aggregated the way
// db_backup/stats.py does it (carry-forward per database, deltas, Theil–Sen forecast, top 8 + "others").
import type {
  EngineId, ObjectMetric, ObjectStats, ProjectStats, StatsDays, StatsDelta, StatsEntity, StatsForecast, StatsMetric, StatsSeries,
} from "../src/api/types";

const DAY = 86400_000;
const HISTORY_DAYS = 120;
const MAX_POINTS = 400;
const SPARK_POINTS = 30;
const TOP_SERIES = 8;
const TOP_OBJECTS = 100;
const MIN_FORECAST = 14;
const FIT_DAYS = 90;
const STALE_DAYS = 30;

interface Obj { name: string; kind: "collection" | "table"; records: number; data: number; disk: number; index: number }
interface Snap { day: string; ts: string; archive: number; data: number | null; records: number | null; dump: number; objects: Obj[]; approx: boolean }

function hash(s: string) {
  let h = 2166136261;
  for (const c of s) h = Math.imul(h ^ c.charCodeAt(0), 16777619) >>> 0;
  return h;
}
function rng(seed: number) {
  let s = seed || 1;
  return () => ((s = (Math.imul(s, 1103515245) + 12345) >>> 0) % 100000) / 100000;
}

const OBJECT_NAMES: Record<string, string[]> = {
  mongodb: ["events", "sessions", "users", "audit", "messages", "jobs", "metrics", "webhooks", "files.meta", "settings", "tokens", "invoices"],
  postgres: ["public.ledger_entries", "public.invoices", "public.payments", "public.customers", "public.audit_trail",
    "reports.daily_revenue", "public.subscriptions", "public.events_2026_10", "public.events_2026_11", "public.plans"],
  mariadb: ["wp_posts", "wp_postmeta", "orders", "order_items", "customers", "products", "wp_options", "sessions"],
};

const dayIso = (t: number) => new Date(t).toISOString().slice(0, 10);
/** Calendar day of a UTC instant in an IANA zone. */
function localDay(t: number, tz: string) {
  try {
    const p = new Intl.DateTimeFormat("en-CA", { timeZone: tz, year: "numeric", month: "2-digit", day: "2-digit" }).format(new Date(t));
    return p;
  } catch {
    return dayIso(t);
  }
}
const addDays = (d: string, n: number) => dayIso(Date.parse(`${d}T00:00:00Z`) + n * DAY);
const diffDays = (a: string, b: string) => Math.round((Date.parse(`${a}T00:00:00Z`) - Date.parse(`${b}T00:00:00Z`)) / DAY);

const cache = new Map<string, Snap[]>();

/** Daily snapshots of one database, oldest first (deterministic per project/database). */
function snapshots(project: string, engine: EngineId, database: string, now: number): Snap[] {
  const key = `${project}/${database}/${dayIso(now)}`;
  const hit = cache.get(key);
  if (hit) return hit;
  const r = rng(hash(`${project}/${database}`));
  const h = hash(database);
  const startAgo = h % 7 === 0 ? 20 + (h % 30) : HISTORY_DAYS; // a few databases are newer
  const big = database.includes("conductor") ? 1.4e9 : database === "billing" ? 7.5e8 : database === "@content" ? 1.9e8 : 0;
  const base = big || (3 + (h % 80)) * 1.1e6;
  const growth = 0.0005 + r() * 0.009; // per day
  const ratio = engine === "mongodb" ? 5 + r() * 4 : 3 + r() * 2; // data : archive
  const names = OBJECT_NAMES[engine] ?? [];
  const nObj = names.length ? 4 + (h % (names.length - 3)) : 0;
  const weights = Array.from({ length: nObj }, (_, i) => (i === 0 ? 6 : 3 / (i + 1)) * (0.6 + r() * 0.8));
  const objGrowth = weights.map(() => r() * 2);
  const cleanup = h % 5 === 0 ? Math.floor(r() * 80) + 20 : -1; // days ago a big delete happened
  const out: Snap[] = [];
  const t0 = new Date(now);
  t0.setUTCHours(2, 10, 0, 0);
  const last = t0.getTime() > now ? t0.getTime() - DAY : t0.getTime();
  for (let k = startAgo - 1; k >= 0; k--) {
    if (r() < 0.04 && k > 0) continue; // a skipped or failed day
    const t = last - k * DAY + Math.round(r() * 600_000);
    const age = startAgo - 1 - k;
    let archive = base * (1 + growth) ** age * (0.985 + r() * 0.03);
    if (cleanup >= 0 && k < cleanup) archive *= 0.72;
    const data = engine === "s3import" ? null : Math.round(archive * ratio);
    const objects: Obj[] = [];
    if (data != null && nObj) {
      const w = weights.map((x, i) => x * (1 + growth * objGrowth[i]! * age));
      const sum = w.reduce((s, x) => s + x, 0);
      w.forEach((x, i) => {
        const d = Math.round((data * x) / sum);
        const idx = Math.round(d * (0.08 + (i % 3) * 0.05));
        objects.push({
          name: names[i]!, kind: engine === "mongodb" ? "collection" : "table",
          records: Math.round(d / (300 + ((h >> i) % 7) * 400)), data: d, disk: Math.round(d * (engine === "mongodb" ? 0.35 : 1.05)) + idx, index: idx,
        });
      });
    }
    out.push({
      day: dayIso(t), ts: new Date(t).toISOString().replace(/\.\d{3}Z$/, "+00:00"), archive: Math.round(archive), data,
      records: objects.length ? objects.reduce((s, o) => s + o.records, 0) : null, dump: Math.round((archive / 2.2e7 + 1 + r() * 3) * 10) / 10,
      objects, approx: engine === "postgres" || engine === "mariadb",
    });
  }
  cache.set(key, out);
  return out;
}

function carry(obs: Map<string, number>, days: string[], stale: number | null): (number | null)[] {
  const items = [...obs.entries()].sort(([a], [b]) => a.localeCompare(b));
  const out: (number | null)[] = [];
  let i = 0;
  let last: number | null = null;
  let lastDay: string | null = null;
  for (const d of days) {
    while (i < items.length && items[i]![0] <= d) { [lastDay, last] = items[i]!; i++; }
    out.push(lastDay != null && (stale == null || diffDays(d, lastDay) <= stale) ? last : null);
  }
  return out;
}

const sum = (vals: (number | null | undefined)[]) => {
  const v = vals.filter((x): x is number => x != null);
  return v.length ? v.reduce((s, x) => s + x, 0) : null;
};
const pick = (n: number, limit: number) => {
  if (n <= limit) return Array.from({ length: n }, (_, i) => i);
  const step = Math.ceil(n / limit);
  const out: number[] = [];
  for (let i = n - 1; i >= 0; i -= step) out.push(i);
  return out.reverse();
};
const delta = (vals: (number | null)[], back: number): StatsDelta | null => {
  const cur = vals.at(-1);
  const prev = vals[vals.length - 1 - back];
  if (vals.length <= back || cur == null || prev == null) return null;
  return { abs: cur - prev, pct: prev ? Math.round(((cur - prev) / prev) * 10000) / 100 : null };
};
const pct = (sorted: number[], q: number) => {
  const k = (sorted.length - 1) * q;
  const lo = Math.floor(k), hi = Math.ceil(k);
  return sorted[lo]! + (sorted[hi]! - sorted[lo]!) * (k - lo);
};
function forecast(points: [number, number][], lastX: number): StatsForecast | null {
  if (points.length < MIN_FORECAST) return null;
  const slopes: number[] = [];
  points.forEach(([x1, y1], i) => points.slice(i + 1).forEach(([x2, y2]) => { if (x2 !== x1) slopes.push((y2 - y1) / (x2 - x1)); }));
  slopes.sort((a, b) => a - b);
  const slope = pct(slopes, 0.5);
  const intercept = pct(points.map(([x, y]) => y - slope * x).sort((a, b) => a - b), 0.5);
  const lo = pct(slopes, 0.1), hi = pct(slopes, 0.9);
  const base = intercept + slope * lastX;
  const at = (h: number) => ({ days: h, value: Math.max(0, base + slope * h), low: Math.max(0, base + lo * h), high: Math.max(0, base + hi * h) });
  return { points: points.length, slope_per_day: slope, h30: at(30), h90: at(90) };
}

function span(days: StatsDays, first: string | null, today: string) {
  const need = Math.max(days || 0, FIT_DAYS + 1);
  let start = addDays(today, -(need - 1));
  if (days === 0 && first && first < start) start = first;
  const all: string[] = [];
  for (let d = start; d <= today; d = addDays(d, 1)) all.push(d);
  const periodStart = days ? all.length - days : first ? Math.max(0, diffDays(first, start)) : all.length - 1;
  return { all, periodStart };
}

function build(matrix: Map<string, (number | null)[]>, all: string[], periodStart: number, observed: Set<string>, extra: Map<string, Partial<StatsEntity>>) {
  const period = all.slice(periodStart);
  const idx = pick(period.length, MAX_POINTS);
  const sidx = pick(period.length, SPARK_POINTS);
  const cols = all.map((_, i) => sum([...matrix.values()].map((v) => v[i])));
  const fitFrom = Math.max(0, all.length - FIT_DAYS);
  const pts: [number, number][] = [];
  for (let i = fitFrom; i < all.length; i++) if (observed.has(all[i]!) && cols[i] != null) pts.push([i, cols[i]!]);
  const ranked = [...matrix.keys()].sort((a, b) => (matrix.get(b)!.at(-1) ?? 0) - (matrix.get(a)!.at(-1) ?? 0) || a.localeCompare(b));
  const top = ranked.slice(0, TOP_SERIES), rest = ranked.slice(TOP_SERIES);
  const series: StatsSeries[] = top.map((n) => ({ name: n, values: idx.map((i) => matrix.get(n)![periodStart + i] ?? null) }));
  if (rest.length) series.push({ name: "others", values: idx.map((i) => sum(rest.map((n) => matrix.get(n)![periodStart + i]))), rest: true, count: rest.length });
  const entities: StatsEntity[] = [...matrix.entries()].map(([name, vals]) => {
    const p = vals.slice(periodStart);
    const firstV = p.find((v) => v != null) ?? null;
    const cur = vals.at(-1) ?? null;
    return { name, current: cur, delta7: delta(vals, 7), delta30: delta(vals, 30), delta_period: cur == null || firstV == null ? null : cur - firstV, spark: sidx.map((i) => p[i] ?? null), ...extra.get(name) };
  }).sort((a, b) => (b.current ?? 0) - (a.current ?? 0) || a.name.localeCompare(b.name));
  return {
    summary: { current: cols.at(-1) ?? null, delta7: delta(cols, 7), delta30: delta(cols, 30), forecast: forecast(pts, all.length - 1), observed_days: pts.length, forecast_min_points: MIN_FORECAST, approx: false, partial: false },
    timeline: { days: idx.map((i) => period[i]!), total: idx.map((i) => cols[periodStart + i] ?? null), series },
    entities,
  };
}

export function projectStats(project: string, engine: EngineId, databases: string[], q: { days: StatsDays; metric: StatsMetric; database: string | null; tz: string }): ProjectStats {
  const now = Date.now();
  const today = localDay(now, q.tz);
  const dbs = q.database ? databases.filter((d) => d === q.database) : databases;
  const obs = new Map<string, Map<string, number>>();
  const observed = new Set<string>();
  const extra = new Map<string, Partial<StatsEntity>>();
  const uploads = new Map<string, [number, number]>();
  const dumps = new Map<string, number>();
  let first: string | null = null;
  let since: string | null = null;
  for (const db of dbs) {
    for (const s of snapshots(project, engine, db, now)) {
      const d = localDay(Date.parse(s.ts), q.tz);
      if (!first || d < first) { first = d; since = s.ts; }
      const u = uploads.get(d) ?? [0, 0];
      uploads.set(d, [u[0] + s.archive, u[1] + 1]);
      dumps.set(d, (dumps.get(d) ?? 0) + s.dump);
      extra.set(db, { last_ts: s.ts, dump_seconds: s.dump, approx: s.approx && q.metric === "records", partial: false });
      const v = q.metric === "archive" ? s.archive : q.metric === "data" ? s.data : s.records;
      if (v == null) continue;
      if (!obs.has(db)) obs.set(db, new Map());
      obs.get(db)!.set(d, v);
      observed.add(d);
    }
  }
  const { all, periodStart } = span(q.days, first, today);
  const matrix = new Map([...obs.entries()].map(([n, o]) => [n, carry(o, all, STALE_DAYS)]));
  const b = build(matrix, all, periodStart, observed, extra);
  const period = all.slice(periodStart);
  const healthDays = period.slice(-MAX_POINTS);
  const last7 = Array.from({ length: 7 }, (_, i) => addDays(today, i - 6)).map((d) => ({ day: d, bytes: uploads.get(d)?.[0] ?? 0, archives: uploads.get(d)?.[1] ?? 0 }));
  return {
    days: q.days, metric: q.metric, unit: q.metric === "records" ? "count" : "bytes", tz: q.tz, since, today,
    period_from: all[periodStart] ?? today, database: q.database,
    summary: { ...b.summary, databases: [...matrix.values()].filter((v) => v.at(-1) != null).length, approx: q.metric === "records" && (engine === "postgres" || engine === "mariadb") },
    timeline: b.timeline, last7, databases: b.entities,
    health: healthDays.map((d) => {
      const h = hash(`${project}${d}`);
      const failed = h % 23 === 0 ? 1 + (h % 2) : 0;
      return { day: d, dump_seconds: dumps.has(d) ? Math.round(dumps.get(d)! * 10) / 10 : null, ok: uploads.get(d)?.[1] ?? 0, failed };
    }),
  };
}

export function objectStats(project: string, engine: EngineId, database: string, q: { days: StatsDays; metric: string; tz: string }): ObjectStats {
  const now = Date.now();
  const today = localDay(now, q.tz);
  let note: string | null = null;
  let metric = q.metric as ObjectMetric | "archive";
  if (metric === "archive") { metric = "data"; note = "archive size cannot be split by collection — showing data size instead"; }
  const m = metric as ObjectMetric;
  const snaps = snapshots(project, engine, database, now).filter((s) => s.objects.length);
  const obs = new Map<string, Map<string, number>>();
  const extra = new Map<string, Partial<StatsEntity>>();
  const observed = new Set<string>();
  for (const s of snaps) {
    const d = localDay(Date.parse(s.ts), q.tz);
    observed.add(d);
    for (const o of s.objects) {
      if (!obs.has(o.name)) obs.set(o.name, new Map());
      obs.get(o.name)!.set(d, m === "data" ? o.data : m === "disk" ? o.disk : o.records);
      extra.set(o.name, { kind: o.kind, records: o.records, data_bytes: o.data, disk_bytes: o.disk, index_bytes: o.index, last_ts: s.ts, approx: s.approx && m === "records" });
    }
  }
  const first = snaps[0] ? localDay(Date.parse(snaps[0].ts), q.tz) : null;
  const { all, periodStart } = span(q.days, first, today);
  const matrix = new Map([...obs.entries()].map(([n, o]) => [n, carry(o, all, null)]));
  const b = build(matrix, all, periodStart, observed, extra);
  const leaders = b.entities.filter((e) => (e.delta_period ?? 0) > 0).sort((a, b2) => (b2.delta_period ?? 0) - (a.delta_period ?? 0)).slice(0, 5);
  return {
    days: q.days, metric: m, unit: m === "records" ? "count" : "bytes", tz: q.tz, since: snaps[0]?.ts ?? null, today,
    period_from: all[periodStart] ?? today, database, note,
    summary: { ...b.summary, objects: b.entities.length, approx: m === "records" && (engine === "postgres" || engine === "mariadb") },
    timeline: b.timeline, objects: b.entities.slice(0, TOP_OBJECTS),
    rest: { count: Math.max(0, b.entities.length - TOP_OBJECTS), current: sum(b.entities.slice(TOP_OBJECTS).map((e) => e.current)) },
    leaders: leaders.map((e) => ({ name: e.name, delta: e.delta_period!, current: e.current })),
  };
}
