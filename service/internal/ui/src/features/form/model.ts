import type { EngineId, EngineMeta, ProjectConfig, Retention } from "@/api/types";
import { localToUtcHM, NAME_RE, parseDuration, utcToLocalHM, validateDuration } from "@/lib/format";

export interface FormValues {
  name: string;
  engine: EngineId;
  dbMode: "all" | "list";
  databases: string[];
  interval: string;
  timeout: string;
  retention: Record<keyof Retention, string>;
  retries: string;
  shrink: string;
  dumpArgs: string[];
  globals: boolean;
  /** Local "HH:MM", "" = no fixed time (interval after the last run). Sent to the server in UTC. */
  start_at: string;
}

export type Errors = Partial<Record<string, string>>;

export const RETENTION_KEYS: (keyof Retention)[] = ["last", "daily", "weekly", "monthly"];

export function defaults(engine: EngineId = "mongodb", meta?: EngineMeta[]): FormValues {
  return {
    name: "",
    engine,
    dbMode: "all",
    databases: [],
    interval: "1d",
    timeout: "6h",
    retention: { last: "7", daily: "7", weekly: "4", monthly: "6" },
    retries: "2",
    shrink: "20",
    dumpArgs: [],
    globals: !!meta?.find((e) => e.id === engine)?.supports_globals,
    start_at: "",
  };
}

export function fromConfig(c: ProjectConfig): FormValues {
  return {
    name: c.name,
    engine: c.engine,
    dbMode: c.databases === "all" ? "all" : "list",
    databases: c.databases === "all" ? [] : [...c.databases],
    interval: c.interval,
    timeout: c.timeout,
    retention: {
      last: String(c.retention.last), daily: String(c.retention.daily),
      weekly: String(c.retention.weekly), monthly: String(c.retention.monthly),
    },
    retries: String(c.retries),
    shrink: String(c.shrink_warn_percent),
    dumpArgs: [...(c.dump_args ?? [])],
    globals: !!c.globals,
    start_at: c.start_at ? utcToLocalHM(c.start_at) : "",
  };
}

const int = (s: string) => (/^\d+$/.test(s.trim()) ? parseInt(s.trim(), 10) : NaN);

export function toConfig(v: FormValues, engine?: EngineMeta): ProjectConfig {
  return {
    name: v.name.trim(),
    engine: v.engine,
    databases: v.dbMode === "all" ? "all" : v.databases,
    interval: v.interval.trim(),
    timeout: v.timeout.trim(),
    retention: {
      last: int(v.retention.last), daily: int(v.retention.daily),
      weekly: int(v.retention.weekly), monthly: int(v.retention.monthly),
    },
    retries: int(v.retries),
    shrink_warn_percent: int(v.shrink),
    dump_args: v.dumpArgs,
    globals: engine?.supports_globals ? v.globals : false,
    start_at: v.start_at ? localToUtcHM(v.start_at) : null,
  };
}

export function retentionNumbers(v: FormValues): Retention | null {
  const r = { last: int(v.retention.last), daily: int(v.retention.daily), weekly: int(v.retention.weekly), monthly: int(v.retention.monthly) };
  return Object.values(r).some(Number.isNaN) ? null : r;
}

export function validateCount(s: string, min: number, max: number): string | null {
  if (!s.trim()) return "Enter a number";
  const n = int(s);
  if (Number.isNaN(n)) return "Whole numbers only";
  if (n < min || n > max) return `From ${min} to ${max}`;
  return null;
}

export function argFlag(arg: string) {
  const i = arg.indexOf("=");
  return i < 0 ? arg : arg.slice(0, i);
}

export function validate(v: FormValues, engine: EngineMeta | undefined, opts: { takenNames?: string[]; checkName?: boolean }): Errors {
  const e: Errors = {};
  if (opts.checkName) {
    const n = v.name.trim();
    if (!n) e.name = "Enter a project name";
    else if (!NAME_RE.test(n)) e.name = "Latin letters, digits, dot, hyphen and underscore; must start with a letter or digit; up to 64 characters";
    else if (opts.takenNames?.includes(n)) e.name = "A project with this name already exists";
  }
  if (v.dbMode === "list" && v.databases.length === 0) e.databases = "Select at least one database or switch to \"All databases\"";
  const iv = validateDuration(v.interval, "Interval");
  if (iv) e.interval = iv;
  const mins = iv ? null : parseDuration(v.interval);
  if (v.start_at && mins && 1440 % mins !== 0 && mins % 1440 !== 0) {
    e.start_at = "With a fixed start time the interval must divide a day evenly (1h, 6h, 12h…) or be a multiple of a day (1d, 1w)";
  }
  const tv = validateDuration(v.timeout, "Timeout");
  if (tv) e.timeout = tv;
  for (const k of RETENTION_KEYS) {
    const err = validateCount(v.retention[k], 0, 10000);
    if (err) e[`retention.${k}`] = err;
  }
  const r = retentionNumbers(v);
  if (r && Object.values(r).every((x) => x === 0)) e.retention = "At least one rule must be greater than zero";
  const rt = validateCount(v.retries, 0, 10);
  if (rt) e.retries = rt;
  const sw = validateCount(v.shrink, 0, 99);
  if (sw) e.shrink_warn_percent = sw;
  if (engine) {
    const bad = v.dumpArgs.filter((a) => !engine.dump_args.includes(argFlag(a)) || !/^--[^=\s]+=.+$/.test(a));
    if (bad.length) e.dump_args = `Not allowed for ${engine.label}: ${bad.join(", ")}`;
  }
  return e;
}

/** Maps a server `field` path (e.g. "retention.daily", "config.interval") to our error keys. */
export function serverFieldKey(field: string | null): string | null {
  if (!field) return null;
  const f = field.replace(/^config\./, "");
  if (f === "shrink_warn_percent" || f === "retries" || f === "interval" || f === "timeout" || f === "name" || f === "databases" || f === "globals" || f === "uri" || f === "start_at") return f;
  if (f.startsWith("retention")) return f.includes(".") ? f : "retention";
  if (f.startsWith("dump_args")) return "dump_args";
  if (f.startsWith("databases")) return "databases";
  return f;
}

export function sameValues(a: FormValues, b: FormValues) {
  return JSON.stringify(a) === JSON.stringify(b);
}

/** Expected URI schemes per engine — a soft hint only; the server is the judge. */
export const URI_SCHEMES: Record<EngineId, string[]> = {
  mongodb: ["mongodb://", "mongodb+srv://"],
  postgres: ["postgresql://", "postgres://"],
  mariadb: ["mariadb://", "mysql://"],
  s3import: ["s3://", "s3+http://"],
};

export const SYSTEM_DBS: Record<EngineId, string> = {
  mongodb: "admin, local and config are skipped",
  postgres: "templates and databases that disallow connections are skipped",
  mariadb: "information_schema, performance_schema, mysql and sys are skipped",
  s3import: "",
};
