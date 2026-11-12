// Types mirror docs/api.md (HTTP API v1). Do not add fields that are not in the contract.

export type EngineId = "mongodb" | "postgres" | "mariadb" | "s3import";

export interface Session {
  authenticated: boolean;
  csrf: string | null;
  password_set: boolean;
}

export interface EngineMeta {
  id: EngineId;
  label: string;
  uri_example: string;
  dump_args: string[];
  supports_globals: boolean;
}

export interface Meta {
  version: string;
  /** commit and time of the deployed source; null when run from a checkout */
  build?: { commit: string; deployed_at: string | null } | null;
  engines: EngineMeta[];
}

export type RunStatus = "running" | "ok" | "partial" | "failed" | "interrupted";
export type ProjectState = "idle" | "queued" | "running" | "cancelling";
export type Phase = "queued" | "discover" | "dump" | "upload" | "prune";
export type Trigger = "schedule" | "manual";
export type Databases = "all" | string[];

export interface RunSummary {
  id: number;
  project: string;
  trigger: Trigger;
  status: RunStatus;
  started_at: string;
  finished_at: string | null;
  error: string | null;
  warnings: string[];
  db_ok: number;
  db_failed: number;
  bytes: number;
}

export interface DbRun {
  database: string;
  status: string;
  bytes: number;
  seconds: number;
  attempts: number;
  error: string | null;
  key: string | null;
}

export interface RunDetail extends RunSummary {
  databases: DbRun[];
}

export interface ProjectSummary {
  name: string;
  engine: EngineId;
  databases: Databases;
  interval: string;
  start_at?: string | null;
  uri_set: boolean;
  state: ProjectState;
  last_run: RunSummary | null;
  last_success_at: string | null;
  next_due: string | null;
  total_bytes: number;
  archive_count: number;
  database_count: number;
  /** UTC ISO time since which there is no successful backup within the allowed window; null: fine. */
  overdue_since?: string | null;
  /** The allowed window in seconds: max(2·interval, interval + timeout + 1h). */
  overdue_after?: number;
}

/** A database of a run in progress: finished ones carry the result, the current one is "running". */
export interface RunningDb {
  database: string;
  status: "pending" | "running" | "ok" | "unchanged" | "failed";
  bytes: number;
  seconds: number;
  attempts: number;
  error: string | null;
}

export interface RunningOp {
  project: string;
  run_id: number;
  started_at: string;
  database: string | null;
  db_index: number;
  db_total: number;
  db_started_at: string | null;
  attempt: number;
  phase: Phase;
  state?: ProjectState;
  databases?: RunningDb[];
  /** Live size of the current dump/download (spool file). */
  bytes_done?: number;
  /** Likely final size: the previous archive of this database, or the source object for an import; 0 = unknown. */
  bytes_expected?: number;
  bytes_per_sec?: number;
}

/** The NAS volume holding the spool and MinIO's data; forecast from one sample per day. */
export interface DiskStatus {
  total: number;
  used: number;
  free: number;
  /** average bytes/day over the samples; null until two weeks are recorded */
  growth_per_day: number | null;
  days_left: number | null;
  /** first recorded day (YYYY-MM-DD) */
  since: string | null;
}

export interface StorageStatus {
  kind: string;
  location: string;
  ok: boolean;
  error: string | null;
  disk?: DiskStatus | null;
}

export type AlertLevel = "error" | "warning" | "info" | string;

export interface Alert {
  id: number;
  ts: string;
  level: AlertLevel;
  project: string | null;
  text: string;
}

export interface Overview {
  projects: ProjectSummary[];
  running: RunningOp[];
  storage: StorageStatus;
  alerts: Alert[];
}

export interface Retention {
  last: number;
  daily: number;
  weekly: number;
  monthly: number;
}

export interface ProjectConfig {
  name: string;
  engine: EngineId;
  databases: Databases;
  interval: string;
  timeout: string;
  retention: Retention;
  retries: number;
  shrink_warn_percent: number;
  dump_args: string[];
  globals: boolean;
  /** "HH:MM" UTC; the UI shows and edits it in local time. Null: interval after the last run. */
  start_at?: string | null;
}

export interface ProjectResponse {
  config: ProjectConfig;
  revision: string;
  uri_set: boolean;
}

export type KeptBy = keyof Retention;

export interface Archive {
  key: string;
  ts: string;
  size: number;
  kept_by: KeptBy[];
}

export interface DatabaseArchives {
  database: string;
  total_bytes: number;
  archives: Archive[];
}

export interface ArchivesResponse {
  databases: DatabaseArchives[];
  globals: Archive[];
  /** {key} is the archive key; by_suffix (s3import) overrides command for archives with that suffix. */
  restore: { command: string; by_suffix?: Record<string, string> };
}

export interface TestConnectionResult {
  ok: boolean;
  server_version: string | null;
  client_version: string | null;
  databases: string[];
  warnings: string[];
  error: string | null;
}

/** GET /api/runs: newest first, across every project. */
export interface RunsResponse {
  runs: RunSummary[];
  projects: string[];
}

export type TelegramChatType = "private" | "group" | "supergroup" | "channel";

export interface TelegramChat {
  id: string;
  type: TelegramChatType;
  name: string;
  username: string;
}

export interface TelegramCheck {
  bot: { username: string; name: string };
  chats: TelegramChat[];
}

export interface AppSettings {
  storage: { kind: string; endpoint?: string; bucket?: string; prefix?: string; [k: string]: unknown };
  notify: { telegram: { chat_id: string; token_set: boolean } | null; webhook_set: boolean };
  max_parallel: number;
}

/** A collection of a dumped MongoDB database, recorded at dump time. */
export interface CollectionStat {
  name: string;
  docs: number;
  /** uncompressed data size */
  bytes: number;
  storage_bytes: number;
  index_bytes: number;
}

export interface CollectionsResponse {
  key: string;
  /** the database's name as stats know it (archive keys percent-encode it) */
  database: string | null;
  collections: CollectionStat[] | null;
  /** the listing stopped early (deadline): collections may be missing, sums are lower bounds */
  partial?: boolean;
  /** The newest earlier archive of the same database with a recorded inventory; null when there is none. */
  previous: { key: string; ts: string; bytes: number | null; collections: CollectionStat[]; partial?: boolean } | null;
}

/** Day-by-day values of every collection / table of a database; null: no inventory that day. */
export interface ObjectHistory {
  tz: string;
  database: string;
  days: string[];
  objects: Record<string, { records: (number | null)[]; data: (number | null)[]; disk: (number | null)[]; index: (number | null)[] }>;
}

// ---- Statistics (docs/api.md § Statistics) ----

/** archive: archive size in storage; data: data size from the server's catalog; records: documents / rows. */
export type StatsMetric = "archive" | "data" | "records";
/** Objects (collections / tables) have no archive size of their own: disk = data + indexes on the source. */
export type ObjectMetric = "data" | "disk" | "records";
/** 0: everything recorded. */
export type StatsDays = 7 | 30 | 90 | 0;

export interface StatsDelta { abs: number; pct: number | null }

export interface ForecastPoint { days: number; value: number; low: number; high: number }

export interface StatsForecast {
  points: number;
  slope_per_day: number;
  h30: ForecastPoint;
  h90: ForecastPoint;
}

export interface StatsSummary {
  current: number | null;
  delta7: StatsDelta | null;
  delta30: StatsDelta | null;
  forecast: StatsForecast | null;
  observed_days: number;
  forecast_min_points: number;
  approx: boolean;
  partial: boolean;
  databases?: number;
  objects?: number;
}

export interface StatsSeries { name: string; values: (number | null)[]; rest?: boolean; count?: number }

export interface StatsTimeline { days: string[]; total: (number | null)[]; series: StatsSeries[] }

export interface StatsEntity {
  name: string;
  current: number | null;
  delta7: StatsDelta | null;
  delta30: StatsDelta | null;
  delta_period: number | null;
  spark: (number | null)[];
  last_ts?: string;
  dump_seconds?: number | null;
  approx?: boolean;
  partial?: boolean;
  // objects only
  kind?: string;
  records?: number | null;
  data_bytes?: number | null;
  disk_bytes?: number | null;
  index_bytes?: number | null;
}

interface StatsBase {
  days: StatsDays;
  unit: "bytes" | "count";
  tz: string;
  /** first measurement (UTC ISO), null when nothing was recorded yet */
  since: string | null;
  today: string;
  period_from: string;
  database: string | null;
  summary: StatsSummary;
  timeline: StatsTimeline;
}

export interface ProjectStats extends StatsBase {
  metric: StatsMetric;
  last7: { day: string; bytes: number; archives: number }[];
  databases: StatsEntity[];
  health: { day: string; dump_seconds: number | null; ok: number; failed: number }[];
}

export interface ObjectStats extends StatsBase {
  metric: ObjectMetric;
  note: string | null;
  objects: StatsEntity[];
  rest: { count: number; current: number | null };
  leaders: { name: string; delta: number; current: number | null }[];
}
