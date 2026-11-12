import { ArrowDown, ArrowLeft, ArrowUp, BarChart3, Info } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import { describeError } from "@/api/client";
import { useObjectStats, useStats } from "@/api/queries";
import type {
  EngineId, ObjectMetric, ObjectStats, ProjectStats, StatsDays, StatsDelta, StatsEntity, StatsMetric, StatsSummary,
} from "@/api/types";
import {
  ColumnChart, REST_COLOR, Sparkline, TimeChart, VIZ_SLOTS, formatDay, niceByteTicks, niceTicks, vizColor,
  type TimeSeries,
} from "@/components/charts/Charts";
import { Empty, ErrorState } from "@/components/ui/Empty";
import { Segmented } from "@/components/ui/Segmented";
import { Select } from "@/components/ui/Select";
import { Skeleton } from "@/components/ui/Skeleton";
import { Tooltip } from "@/components/ui/Tooltip";
import { cn } from "@/lib/cn";
import { formatBytes, formatDateTime, formatDuration, formatInt, LOCALE, plural } from "@/lib/format";

// ---- labels: every number says what it measures ----

const METRIC_LABEL: Record<StatsMetric | ObjectMetric, string> = {
  archive: "Archives",
  data: "Data",
  disk: "On disk",
  records: "Records",
};
const METRIC_TITLE: Record<StatsMetric | ObjectMetric, string> = {
  archive: "Size of one project copy",
  data: "Data size on the database server",
  disk: "Data and indexes on server disk",
  records: "Documents and rows",
};
const METRIC_HINT: Record<StatsMetric | ObjectMetric, string> = {
  archive: "Sum of the latest archive of each database in MinIO: the space one project copy takes on the NAS.",
  data: "From the server catalog: uncompressed data for MongoDB, table size without indexes for PostgreSQL and MariaDB.",
  disk: "Tables or collections including indexes on the database server disk.",
  records: "MongoDB documents are exact; PostgreSQL and MariaDB rows are estimates from server statistics.",
};

const compact = new Intl.NumberFormat(LOCALE, { notation: "compact", maximumFractionDigits: 1 });
const pctFmt = new Intl.NumberFormat(LOCALE, { maximumFractionDigits: 1 });

function formatter(unit: "bytes" | "count") {
  return unit === "bytes" ? (v: number) => formatBytes(Math.round(v)) : (v: number) => (Math.abs(v) < 10_000 ? formatInt(Math.round(v)) : compact.format(v));
}

function signed(v: number, fmt: (v: number) => string) {
  if (v === 0) return "0";
  return `${v > 0 ? "+" : "−"}${fmt(Math.abs(v))}`;
}

const DAYS_OPTIONS = [
  { value: "7", label: "7 d" },
  { value: "30", label: "30 d" },
  { value: "90", label: "90 d" },
  { value: "0", label: "all" },
] as const;

const PERIOD_TEXT: Record<StatsDays, string> = { 7: "over 7 days", 30: "over 30 days", 90: "over 90 days", 0: "all time" };

/** Filters live in the URL so a view can be shared and survives a reload. */
function useStatsParams() {
  const [params, setParams] = useSearchParams();
  const daysRaw = Number(params.get("days") ?? "30");
  const days = ([7, 30, 90, 0] as const).find((d) => d === daysRaw) ?? 30;
  const m = params.get("metric");
  const metric: StatsMetric = m === "data" || m === "records" ? m : "archive";
  const om = params.get("ometric");
  const objectMetric: ObjectMetric = om === "disk" || om === "records" ? om : "data";
  const view = params.get("view") === "stacked" ? "stacked" : "total";
  const set = (patch: Record<string, string | null>) =>
    setParams((p) => {
      const n = new URLSearchParams(p);
      for (const [k, v] of Object.entries(patch)) if (v == null) n.delete(k); else n.set(k, v);
      return n;
    }, { replace: true });
  return { days, metric, objectMetric, view, filter: params.get("filter"), db: params.get("db"), set } as const;
}

// ---- KPI tiles ----

function Kpi({ label, value, sub, hint }: { label: string; value: ReactNode; sub?: ReactNode; hint?: string }) {
  return (
    <div className="min-w-0 rounded-md border border-line bg-surface px-4 py-3">
      <div className="flex items-center gap-1 text-xs text-muted">
        <span className="leading-tight">{label}</span>
        {hint && (
          <Tooltip content={hint}>
            <span tabIndex={0} className="rounded text-faint hover:text-fg" aria-label={hint}><Info className="h-3 w-3" aria-hidden /></span>
          </Tooltip>
        )}
      </div>
      <div className="mt-1 min-w-0 break-words text-lg font-semibold sm:text-xl">{value}</div>
      {sub && <div className="mt-0.5 text-xs text-muted">{sub}</div>}
    </div>
  );
}

function Delta({ d, fmt, className, stack }: { d: StatsDelta | null | undefined; fmt: (v: number) => string; className?: string; stack?: boolean }) {
  if (!d) return <span className={cn("text-faint", className)}>—</span>;
  const Icon = d.abs > 0 ? ArrowUp : d.abs < 0 ? ArrowDown : null;
  const pct = d.pct != null && `${d.pct > 0 ? "+" : d.pct < 0 ? "−" : ""}${pctFmt.format(Math.abs(d.pct))}%`;
  return (
    <span className={cn("tnum", stack ? "inline-flex flex-col" : "inline-flex flex-wrap items-baseline gap-x-1", className)}>
      <span className="inline-flex items-center gap-0.5 whitespace-nowrap">
        {Icon && <Icon className="h-3 w-3 shrink-0 text-faint" aria-hidden />}
        {signed(d.abs, fmt)}
      </span>
      {pct && <span className={cn("whitespace-nowrap text-faint", stack && "text-xs")}>{stack ? pct : `(${pct})`}</span>}
    </span>
  );
}

const lowerFirst = (t: string) => t.charAt(0).toLowerCase() + t.slice(1);

function since(iso: string | null) {
  return iso ? formatDateTime(iso).replace(/,.*$/, "") : null;
}

function KpiRow({ s, metric, fmt, sinceIso, what }: { s: StatsSummary; metric: StatsMetric | ObjectMetric; fmt: (v: number) => string; sinceIso: string | null; what: string }) {
  const f = s.forecast;
  const left = Math.max(0, s.forecast_min_points - s.observed_days);
  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
      <Kpi
        label={`${METRIC_TITLE[metric]} now`}
        hint={METRIC_HINT[metric]}
        value={s.current == null ? "—" : <>{s.approx && "≈ "}{fmt(s.current)}</>}
        sub={s.current == null ? "no measurements for this metric" : what + (s.partial ? " · inventory incomplete" : "")}
      />
      <Kpi label="Change over 7 days" value={<Delta d={s.delta7} fmt={fmt} stack />} sub="compared with a week ago" />
      <Kpi label="Change over 30 days" value={<Delta d={s.delta30} fmt={fmt} stack />} sub="compared with a month ago" />
      {f ? (
        <Kpi
          label="Forecast in 30 days"
          hint={`Robust linear estimate from ${f.points} ${plural(f.points, ["day", "days"])} of observations over the last 90 days; range is the 10–90% of slopes.`}
          value={<span className="tnum">{fmt(f.h30.value)}</span>}
          sub={<>{fmt(f.h30.low)} to {fmt(f.h30.high)} · in 90 days ≈ {fmt(f.h90.value)}</>}
        />
      ) : (
        <Kpi
          label="Forecast"
          value={<span className="text-base font-medium text-muted">not yet</span>}
          sub={s.current == null
            ? "no measurements for this metric yet"
            : sinceIso
              ? <>collecting since {since(sinceIso)}; the forecast appears after {s.forecast_min_points} days of observations{left ? ` (${left} to go)` : ""}</>
              : `appears after ${s.forecast_min_points} days of observations`}
        />
      )}
    </div>
  );
}

// ---- timeline ----

/** Colour follows the entity: its slot comes from its place among all names, so a filter does not repaint it. */
function colorSlots(all: string[], shown: string[]) {
  const sorted = [...all].sort((a, b) => a.localeCompare(b));
  const taken = new Set<number>();
  const out = new Map<string, number>();
  for (const name of [...shown].sort((a, b) => a.localeCompare(b))) {
    let slot = Math.max(0, sorted.indexOf(name)) % VIZ_SLOTS;
    for (let k = 0; k < VIZ_SLOTS && taken.has(slot); k++) slot = (slot + 1) % VIZ_SLOTS;
    taken.add(slot);
    out.set(name, slot);
  }
  return out;
}

function Legend({ items }: { items: { name: string; color: string }[] }) {
  return (
    <ul className="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
      {items.map((i) => (
        <li key={i.name} className="flex min-w-0 items-center gap-1.5">
          <span aria-hidden className="h-2.5 w-2.5 shrink-0 rounded-sm" style={{ background: i.color }} />
          <span className="max-w-[12rem] truncate">{i.name}</span>
        </li>
      ))}
    </ul>
  );
}

function Card({ title, sub, actions, children, className }: { title: ReactNode; sub?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={cn("min-w-0 rounded-md border border-line bg-surface px-4 py-3", className)}>
      <div className="mb-3 flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <h3 className="text-sm font-medium">{title}</h3>
          {sub && <p className="text-xs text-muted">{sub}</p>}
        </div>
        {actions}
      </div>
      {children}
    </section>
  );
}

function Timeline({ data, view, onView, colors, fmt, what }: {
  data: ProjectStats | ObjectStats; view: "total" | "stacked"; onView: (v: "total" | "stacked") => void;
  colors: Map<string, number>; fmt: (v: number) => string; what: string;
}) {
  const tl = data.timeline;
  const multi = tl.series.length > 1;
  const mode = multi ? view : "total";
  const series: TimeSeries[] = tl.series.map((s) => ({
    name: s.rest ? `${s.name} (${s.count})` : s.name,
    values: s.values,
    color: s.rest ? REST_COLOR : vizColor(colors.get(s.name) ?? 0),
  }));
  return (
    <Card
      title={`${METRIC_TITLE[data.metric]} by day`}
      sub={`${what} · daily value is the last measurement, or the previous one if none`}
      actions={multi ? (
        <Segmented
          size="sm" ariaLabel="Chart view" value={view} onChange={onView}
          options={[{ value: "total", label: "Total" }, { value: "stacked", label: "objects" in data ? "By object" : "By database" }]}
        />
      ) : undefined}
    >
      <div className={cn("transition-opacity")}>
        <TimeChart
          days={tl.days} total={tl.total} series={series} mode={mode} format={fmt}
          ticks={data.unit === "bytes" ? niceByteTicks : niceTicks}
          ariaLabel={`${METRIC_TITLE[data.metric]} by day ${PERIOD_TEXT[data.days]}`}
        />
      </div>
      {mode === "stacked" && <Legend items={series.map((s) => ({ name: s.name, color: s.color }))} />}
    </Card>
  );
}

// ---- tables ----

type SortKey = "name" | "current" | "delta7" | "delta30";
type Sort = { key: SortKey; desc: boolean };

function SortHeader({ k, sort, onSort, children, className }: { k: SortKey; sort: Sort; onSort: (k: SortKey) => void; children: ReactNode; className?: string }) {
  const on = sort.key === k;
  const Arrow = sort.desc ? ArrowDown : ArrowUp;
  return (
    <span role="columnheader" aria-sort={on ? (sort.desc ? "descending" : "ascending") : "none"} className={className}>
      <button type="button" onClick={() => onSort(k)} className={cn("inline-flex items-center gap-1 rounded hover:text-fg", on && "text-fg")}>
        {children}
        {on ? <Arrow className="h-3 w-3" aria-hidden /> : <ArrowDown className="h-3 w-3 opacity-0" aria-hidden />}
      </button>
    </span>
  );
}

function useSorted(items: StatsEntity[]) {
  const [sort, setSort] = useState<Sort>({ key: "current", desc: true });
  const onSort = (key: SortKey) => setSort((s) => ({ key, desc: s.key === key ? !s.desc : key !== "name" }));
  const sorted = useMemo(() => {
    const val = (e: StatsEntity): number | string =>
      sort.key === "name" ? e.name : sort.key === "current" ? e.current ?? -Infinity : (sort.key === "delta7" ? e.delta7 : e.delta30)?.abs ?? -Infinity;
    return [...items].sort((a, b) => {
      const x = val(a), y = val(b);
      const c = typeof x === "string" ? x.localeCompare(y as string) : (x as number) - (y as number);
      return (sort.desc ? -c : c) || a.name.localeCompare(b.name);
    });
  }, [items, sort]);
  return { sorted, sort, onSort };
}

const ROW = "grid grid-cols-[minmax(0,1fr)_auto] gap-x-3 sm:grid-cols-[minmax(0,1fr)_96px_112px_112px_112px] lg:grid-cols-[minmax(0,1fr)_96px_112px_120px_120px_96px]";

function EntityTable({ items, fmt, colors, onOpen, label, last, footer }: {
  items: StatsEntity[]; fmt: (v: number) => string; colors?: Map<string, number>; onOpen?: (name: string) => void;
  label: string; last: "dump" | "kind"; footer?: ReactNode;
}) {
  const { sorted, sort, onSort } = useSorted(items);
  return (
    <div className="card overflow-hidden">
      <div role="row" className={cn(ROW, "hidden border-b border-line bg-sunken/60 px-3 py-2 text-xs text-muted sm:grid")}>
        <SortHeader k="name" sort={sort} onSort={onSort}>{label}</SortHeader>
        <span role="columnheader">Trend</span>
        <SortHeader k="current" sort={sort} onSort={onSort} className="text-right">Now</SortHeader>
        <SortHeader k="delta7" sort={sort} onSort={onSort} className="text-right">7 days</SortHeader>
        <SortHeader k="delta30" sort={sort} onSort={onSort} className="text-right">30 days</SortHeader>
        <span role="columnheader" className="hidden text-right lg:block">{last === "dump" ? "Dump" : "Type"}</span>
      </div>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-line bg-sunken/60 px-3 py-2 text-xs text-muted sm:hidden">
        <span className="mr-1">Sort:</span>
        <SortHeader k="current" sort={sort} onSort={onSort}>size</SortHeader>
        <SortHeader k="delta7" sort={sort} onSort={onSort}>7 d growth</SortHeader>
        <SortHeader k="name" sort={sort} onSort={onSort}>name</SortHeader>
      </div>
      <ul className="divide-y divide-line">
        {sorted.map((e) => {
          const slot = colors?.get(e.name);
          const name = (
            <span className="flex min-w-0 items-center gap-2">
              {slot != null && <span aria-hidden className="h-2.5 w-2.5 shrink-0 rounded-sm" style={{ background: vizColor(slot) }} />}
              <span className="truncate font-mono text-[13px]">{e.name}</span>
              {e.partial && <span className="shrink-0 text-2xs text-faint">incomplete</span>}
            </span>
          );
          return (
            <li key={e.name} className={cn(ROW, "items-center px-3 py-2 text-sm", onOpen && "hover:bg-sunken/60")}>
              {onOpen ? (
                <button type="button" onClick={() => onOpen(e.name)} className="min-w-0 rounded text-left hover:text-accent" aria-label={`${e.name}: collections and tables`}>
                  {name}
                </button>
              ) : name}
              <span className="hidden sm:block"><Sparkline values={e.spark} label={`Trend of ${e.name}`} /></span>
              <span className="text-right font-medium tnum">
                {e.current == null ? "—" : <>{e.approx && "≈ "}{fmt(e.current)}</>}
                <span className="block text-xs font-normal text-muted sm:hidden"><Delta d={e.delta7} fmt={fmt} /> over 7 d</span>
              </span>
              <span className="hidden text-right text-sm text-muted sm:block"><Delta d={e.delta7} fmt={fmt} stack className="items-end" /></span>
              <span className="hidden text-right text-sm text-muted sm:block"><Delta d={e.delta30} fmt={fmt} stack className="items-end" /></span>
              <span className="hidden text-right text-sm text-muted lg:block">
                {last === "dump" ? formatDuration(e.dump_seconds) : e.kind === "collection" ? "collection" : e.kind === "table" ? "table" : (e.kind ?? "—")}
              </span>
            </li>
          );
        })}
        {footer}
      </ul>
    </div>
  );
}

// ---- project view ----

function LastSeven({ data }: { data: ProjectStats }) {
  const total = data.last7.reduce((s, d) => s + d.bytes, 0);
  const n = data.last7.reduce((s, d) => s + d.archives, 0);
  return (
    <Card title="Uploaded in the last 7 days" sub={`${formatBytes(total)} in ${n} ${plural(n, ["archive", "archives"])} · new archives in storage by day`}>
      <ColumnChart
        ariaLabel="Uploaded to storage by day over the last 7 days"
        format={(v) => formatBytes(v)}
        labelEvery={1}
        columns={data.last7.map((d) => ({
          key: d.day, label: formatDay(d.day), value: d.bytes,
          detail: `${d.archives} ${plural(d.archives, ["archive", "archives"])}`,
        }))}
      />
    </Card>
  );
}

function Health({ data }: { data: ProjectStats }) {
  const failed = data.health.reduce((s, d) => s + d.failed, 0);
  const ok = data.health.reduce((s, d) => s + d.ok, 0);
  const failDays = data.health.filter((d) => d.failed > 0);
  return (
    <Card
      title="Dump duration by day"
      sub={<>sum across databases, excluding upload to storage · {PERIOD_TEXT[data.days]}: {ok} successful, <span className={cn(failed > 0 && "font-medium text-danger")}>{failed} {plural(failed, ["failure", "failures"])}</span></>}
    >
      <ColumnChart
        ariaLabel="Dump duration by day"
        format={(v) => formatDuration(v)}
        columns={data.health.map((d) => ({
          key: d.day, label: formatDay(d.day), value: d.dump_seconds ?? 0, tone: "muted" as const,
          detail: d.failed ? `failed: ${d.failed}` : d.ok ? `succeeded: ${d.ok}` : "no runs",
        }))}
      />
      {failDays.length > 0 && (
        <p className="mt-2 text-xs text-muted">
          Failures: {failDays.slice(-6).map((d) => `${formatDay(d.day)} (${d.failed})`).join(", ")}{failDays.length > 6 ? " …" : ""}
        </p>
      )}
    </Card>
  );
}

function ProjectView({ name, p }: { name: string; p: ReturnType<typeof useStatsParams> }) {
  const all = useStats(name, { days: p.days, metric: p.metric, database: null });
  const filtered = useStats(name, { days: p.days, metric: p.metric, database: p.filter }, !!p.filter);
  const q = p.filter ? filtered : all;
  const data = q.data;
  const names = useMemo(() => (all.data?.databases ?? []).map((d) => d.name), [all.data]);
  const colors = useMemo(
    () => colorSlots(names, (data?.timeline.series ?? []).filter((s) => !s.rest).map((s) => s.name)),
    [names, data],
  );

  if (q.isLoading) return <Loading />;
  if (q.error) return <ErrorState {...describeError(q.error)} onRetry={() => void q.refetch()} />;
  if (!data) return null;
  if (!data.since) {
    return (
      <div className="card">
        <Empty icon={<BarChart3 className="h-5 w-5" />} title="No statistics yet">
          Collection starts with the first successful run: archive size, data size and record count for each database.
        </Empty>
      </div>
    );
  }
  const fmt = formatter(data.unit);
  const nDbs = data.summary.databases || names.length;
  const what = p.filter ? `database ${p.filter}` : `${nDbs} ${plural(nDbs, ["database", "databases"])}`;
  const dbOptions = [{ value: "", label: "All databases", text: "all" }, ...names.map((n) => ({ value: n, label: n, text: n }))];

  return (
    <div className={cn("space-y-4 transition-opacity", q.isFetching && q.isPlaceholderData && "opacity-60")}>
      <Filters p={p}>
        {names.length > 1 && (
          <Select
            ariaLabel="Database"
            className="h-8 w-full text-xs sm:w-56"
            value={p.filter ?? ""}
            onChange={(v) => p.set({ filter: v || null })}
            options={dbOptions}
          />
        )}
        <Segmented
          size="sm" ariaLabel="Metric" value={p.metric}
          onChange={(v) => p.set({ metric: v === "archive" ? null : v })}
          options={(["archive", "data", "records"] as const).map((m) => ({ value: m, label: METRIC_LABEL[m] }))}
        />
      </Filters>
      <KpiRow s={data.summary} metric={data.metric} fmt={fmt} sinceIso={data.since} what={what} />
      {data.summary.current == null && data.metric !== "archive" ? (
        <p className="rounded-md border border-line bg-surface px-4 py-3 text-sm text-muted">
          No measurements for this metric yet: {data.metric === "data" ? "data size" : "record count"} is read from the server catalog after every new dump.
        </p>
      ) : (
        <Timeline data={data} view={p.view} onView={(v) => p.set({ view: v === "total" ? null : v })} colors={colors} fmt={fmt} what={what} />
      )}
      <div className="grid gap-4 lg:grid-cols-2">
        <LastSeven data={data} />
        <Health data={data} />
      </div>
      <div>
        <h3 className="mb-2 text-sm font-medium">
          Databases <span className="font-normal text-muted">· {lowerFirst(METRIC_TITLE[data.metric])}; click a database to see its collections and tables</span>
        </h3>
        {data.databases.length ? (
          <EntityTable
            items={data.databases} fmt={fmt} colors={p.view === "stacked" ? colors : undefined} label="Database" last="dump"
            onOpen={(db) => p.set({ db })}
          />
        ) : (
          <p className="rounded-md border border-line bg-surface px-4 py-3 text-sm text-muted">No databases with measurements for this metric.</p>
        )}
      </div>
    </div>
  );
}

// ---- drill-down: objects of one database ----

function ObjectsView({ name, engine, p }: { name: string; engine?: EngineId; p: ReturnType<typeof useStatsParams> }) {
  const db = p.db ?? "";
  const q = useObjectStats(name, { database: db, days: p.days, metric: p.objectMetric });
  const data = q.data;
  const noun = engine === "mongodb" ? "Collections" : engine === "postgres" || engine === "mariadb" ? "Tables" : "Collections and tables";
  const names = useMemo(() => (data?.objects ?? []).map((o) => o.name), [data]);
  const colors = useMemo(() => colorSlots(names, (data?.timeline.series ?? []).filter((s) => !s.rest).map((s) => s.name)), [names, data]);

  const back = (
    <button type="button" onClick={() => p.set({ db: null })} className="inline-flex items-center gap-1 rounded text-sm text-muted hover:text-fg">
      <ArrowLeft className="h-3.5 w-3.5" aria-hidden /> All databases
    </button>
  );
  const header = (
    <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
      {back}
      <h3 className="min-w-0 break-all font-mono text-base font-medium">{db}</h3>
      <span className="text-sm text-muted">{noun.toLowerCase()}</span>
    </div>
  );

  if (q.isLoading) return <div className="space-y-4">{header}<Loading /></div>;
  if (q.error) return <div className="space-y-4">{header}<ErrorState {...describeError(q.error)} onRetry={() => void q.refetch()} /></div>;
  if (!data) return null;
  const fmt = formatter(data.unit);

  if (!data.since) {
    return (
      <div className="space-y-4">
        {header}
        <div className="card">
          <Empty icon={<BarChart3 className="h-5 w-5" />} title={`${noun}: no inventory yet`}>
            {engine === "s3import"
              ? "An S3 import has no access to a database server: only archive sizes are known."
              : "The inventory of collections and tables is read from the server catalog after every new dump: it will appear after the next successful run."}
          </Empty>
        </div>
      </div>
    );
  }
  const what = `${data.summary.objects ?? 0} ${plural(data.summary.objects ?? 0, ["object", "objects"])}`;
  return (
    <div className={cn("space-y-4 transition-opacity", q.isFetching && q.isPlaceholderData && "opacity-60")}>
      {header}
      <Filters p={p}>
        <Segmented
          size="sm" ariaLabel="Metric" value={p.objectMetric}
          onChange={(v) => p.set({ ometric: v === "data" ? null : v })}
          options={(["data", "disk", "records"] as const).map((m) => ({ value: m, label: METRIC_LABEL[m] }))}
        />
      </Filters>
      <p className="text-xs text-muted">Archive size is not split by collection: this shows size on the database server and record count.</p>
      <KpiRow s={data.summary} metric={data.metric} fmt={fmt} sinceIso={data.since} what={what} />
      <Timeline data={data} view={p.view} onView={(v) => p.set({ view: v === "total" ? null : v })} colors={colors} fmt={fmt} what={what} />
      {data.leaders.length > 0 && (
        <Card title={`Fastest growing ${PERIOD_TEXT[data.days]}`} sub={METRIC_TITLE[data.metric]}>
          <ol className="space-y-1.5 text-sm">
            {data.leaders.map((l, i) => (
              <li key={l.name} className="flex items-baseline gap-3">
                <span className="w-4 shrink-0 text-right text-xs text-faint tnum">{i + 1}</span>
                <span className="min-w-0 flex-1 truncate font-mono text-[13px]">{l.name}</span>
                <span className="whitespace-nowrap text-muted tnum">{signed(l.delta, fmt)}</span>
                <span className="hidden w-24 text-right tnum sm:block">{l.current == null ? "—" : fmt(l.current)}</span>
              </li>
            ))}
          </ol>
        </Card>
      )}
      <EntityTable
        items={data.objects} fmt={fmt} colors={p.view === "stacked" ? colors : undefined} label={noun.replace(/ and tables/, "")} last="kind"
        footer={data.rest.count > 0 ? (
          <li className="px-3 py-2 text-sm text-muted">
            {data.rest.count} more {plural(data.rest.count, ["object", "objects"])}{data.rest.current != null ? ` — ${fmt(data.rest.current)}` : ""}
          </li>
        ) : undefined}
      />
    </div>
  );
}

// ---- shell ----

function Filters({ p, children }: { p: ReturnType<typeof useStatsParams>; children?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <Segmented
        size="sm" ariaLabel="Period" value={String(p.days)}
        onChange={(v) => p.set({ days: v === "30" ? null : v })}
        options={DAYS_OPTIONS.map((o) => ({ value: o.value, label: o.label }))}
      />
      {children}
    </div>
  );
}

function Loading() {
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">{[0, 1, 2, 3].map((i) => <Skeleton key={i} className="h-[76px]" />)}</div>
      <Skeleton className="h-[280px] w-full" />
    </div>
  );
}

export function StatisticsTab({ name, engine }: { name: string; engine?: EngineId }) {
  const p = useStatsParams();
  return p.db ? <ObjectsView name={name} engine={engine} p={p} /> : <ProjectView name={name} p={p} />;
}
