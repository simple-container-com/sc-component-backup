import { AlertTriangle, Bell, HardDrive, Info, Plus, Search, SearchX, ServerCrash, X, XCircle } from "lucide-react";
import { useMemo } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { describeError } from "@/api/client";
import { useDismissAlert, useDismissAllAlerts, useOverview } from "@/api/queries";
import type { DiskStatus, Alert, Overview, ProjectSummary } from "@/api/types";
import { Page } from "@/components/layout/AppShell";
import { EngineMark } from "@/components/Engine";
import { NextDue } from "@/components/NextDue";
import { StatusBadge, projectGroup, statusOf, type ProjectGroup } from "@/components/Status";
import { Badge, Dot } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { TextInput } from "@/components/ui/Field";
import { Segmented } from "@/components/ui/Segmented";
import { LinkButton } from "@/components/ui/LinkButton";
import { Empty, ErrorState } from "@/components/ui/Empty";
import { Skeleton } from "@/components/ui/Skeleton";
import { RelTime } from "@/components/ui/Time";
import { Tooltip } from "@/components/ui/Tooltip";
import { useToast } from "@/components/ui/Toast";
import { RunNowButton } from "@/features/RunNowButton";
import { RunningPanel } from "@/features/RunningPanel";
import { cn } from "@/lib/cn";
import { LOCALE, formatBytes, formatDateTime, formatInt, humanSchedule, plural } from "@/lib/format";
import { useDocumentTitle, useNow } from "@/lib/hooks";

export function OverviewPage() {
  useDocumentTitle("Overview");
  const { data, isLoading, error, refetch } = useOverview();

  return (
    <Page
      wide
      title="Overview"
      actions={
        data && data.projects.length > 0 ? (
          <LinkButton to="/new" variant="primary" icon={<Plus className="h-4 w-4" aria-hidden />}>Add project</LinkButton>
        ) : null
      }
    >
      {isLoading && <OverviewSkeleton />}
      {error && !data && <ErrorState {...describeError(error)} onRetry={() => void refetch()} />}
      {data && (
        <>
          <StorageStrip data={data} />
          <ProblemsBanner projects={data.projects} />
          <RunningPanel running={data.running} />
          {data.projects.length === 0 ? <FirstProject /> : <ProjectTable projects={data.projects} />}
          <AlertsFeed alerts={data.alerts} />
        </>
      )}
    </Page>
  );
}

function OverviewSkeleton() {
  return (
    <div className="space-y-6">
      <Skeleton className="h-9 w-full" />
      <div className="card divide-y divide-line">
        {[0, 1, 2].map((i) => (
          <div key={i} className="flex items-center gap-4 px-4 py-4">
            <Skeleton className="h-7 w-7" />
            <div className="flex-1 space-y-2"><Skeleton className="h-4 w-40" /><Skeleton className="h-3 w-28" /></div>
            <Skeleton className="hidden h-5 w-24 md:block" />
            <Skeleton className="h-7 w-24" />
          </div>
        ))}
      </div>
    </div>
  );
}

function StorageStrip({ data }: { data: Overview }) {
  const totalBytes = data.projects.reduce((s, p) => s + p.total_bytes, 0);
  const totalArchives = data.projects.reduce((s, p) => s + p.archive_count, 0);
  const st = data.storage;
  return (
    <div className={cn("mb-6 flex flex-wrap items-center gap-x-5 gap-y-2 rounded-md border px-4 py-2.5 text-sm", st.ok ? "border-line bg-surface" : "border-danger/30 bg-danger-soft/60")}>
      <div className="flex min-w-0 items-center gap-2">
        <HardDrive className="h-4 w-4 shrink-0 text-faint" aria-hidden />
        <span className="truncate font-mono text-xs">{st.location}</span>
        <Dot tone={st.ok ? "ok" : "danger"} />
        <span className={st.ok ? "text-muted" : "font-medium text-danger"}>{st.ok ? "available" : "unavailable"}</span>
      </div>
      {!st.ok && st.error && <span className="w-full font-mono text-xs text-danger sm:w-auto">{st.error}</span>}
      <div className="ml-auto flex items-center gap-5 text-muted">
        <span className="tnum"><span className="font-medium text-fg">{formatBytes(totalBytes)}</span> in storage</span>
        <span className="tnum"><span className="font-medium text-fg">{formatInt(totalArchives)}</span> {plural(totalArchives, ["archive", "archives"])}</span>
      </div>
      {st.disk && <DiskMeter d={st.disk} />}
    </div>
  );
}

/** Fill of the NAS volume and, after two weeks of daily samples, how long the free space lasts at the current rate. */
function DiskMeter({ d }: { d: DiskStatus }) {
  const pct = d.total ? d.used / d.total : 0;
  const low = d.free / Math.max(1, d.total) < 0.1 || (d.days_left != null && d.days_left < 30);
  const warn = !low && (pct >= 0.8 || (d.days_left != null && d.days_left < 90));
  const tone = low ? "bg-danger" : warn ? "bg-warn" : "bg-accent";
  return (
    <div className="w-full border-t border-line/60 pt-2">
      <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-xs text-muted">
        <span>NAS disk</span>
        <span className="tnum">
          <span className="font-medium text-fg">{formatBytes(d.used)}</span> of {formatBytes(d.total)} used ({Math.round(pct * 100)}%)
        </span>
        <span className={cn("tnum", low ? "font-medium text-danger" : warn ? "text-warn" : "")}>{formatBytes(d.free)} free</span>
        <span className="ml-auto tnum">
          {d.growth_per_day == null
            ? `forecast available after 2 weeks of measurements${d.since ? ` (since ${formatDay(d.since)})` : ""}`
            : d.growth_per_day <= 0
              ? "usage is not growing"
              : <>+{formatBytes(d.growth_per_day)}/day · <span className={cn(low ? "font-medium text-danger" : warn ? "text-warn" : "text-fg")}>lasts ~{humanDays(d.days_left ?? 0)}</span></>}
        </span>
      </div>
      <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-line/70" role="meter" aria-label="NAS disk usage" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct * 100)}>
        <div className={cn("h-full rounded-full", tone)} style={{ width: `${Math.min(100, pct * 100)}%` }} />
      </div>
    </div>
  );
}

function formatDay(ymd: string) {
  const d = new Date(`${ymd}T12:00:00Z`);
  return d.toLocaleDateString(LOCALE, { day: "numeric", month: "short" });
}

function humanDays(n: number) {
  if (n >= 730) return `${Math.round(n / 365)} ${plural(Math.round(n / 365), ["year", "years"])}`;
  if (n >= 60) return `${Math.round(n / 30)} months`;
  return `${n} ${plural(n, ["day", "days"])}`;
}

function FirstProject() {
  return (
    <div className="card relative mb-8 overflow-hidden px-6 py-14 text-center">
      <div className="mx-auto mb-5 flex w-fit items-center -space-x-2" aria-hidden>
        <EngineMark engine="mongodb" size="lg" className="ring-4 ring-surface" />
        <EngineMark engine="postgres" size="lg" className="ring-4 ring-surface" />
        <EngineMark engine="mariadb" size="lg" className="ring-4 ring-surface" />
      </div>
      <h2 className="text-xl font-semibold tracking-tight">Add your first project</h2>
      <p className="mx-auto mt-2 max-w-md text-sm text-muted">
        A project is one MongoDB, PostgreSQL or MariaDB server. On a schedule, db-backup dumps each of its databases,
        stores the archives and removes old ones automatically.
      </p>
      <LinkButton to="/new" variant="primary" className="mt-6" icon={<Plus className="h-4 w-4" aria-hidden />}>Add project</LinkButton>
    </div>
  );
}

type Filter = "all" | ProjectGroup;
const FILTERS: Filter[] = ["all", "problem", "active", "ok"];
const FILTER_LABEL: Record<Filter, string> = { all: "All", problem: "Problems", active: "Running", ok: "OK" };

/** "no successful backup for 3 days" from the last success; hours under two days. */
function overdueText(lastSuccess: string | null, now: number) {
  if (!lastSuccess) return "no successful backup yet";
  const h = Math.max(0, (now - Date.parse(lastSuccess)) / 3600_000);
  return h < 48 ? `no successful backup for ${Math.max(1, Math.floor(h))} h` : `no successful backup for ${Math.floor(h / 24)} days`;
}

function windowText(sec: number) {
  const h = sec / 3600;
  return h >= 48 && Number.isInteger(h / 24) ? `${h / 24} days` : `${Math.round(h)} h`;
}

function sortProjects(projects: ProjectSummary[]) {
  return projects
    .map((p) => ({ p, ...projectGroup(p) }))
    .sort((a, b) => a.rank - b.rank || a.p.name.localeCompare(b.p.name))
    .map(({ p, group }) => ({ p, group }));
}

/** A one-line summary of projects that need attention, above everything else. */
function ProblemsBanner({ projects }: { projects: ProjectSummary[] }) {
  const problems = sortProjects(projects).filter((x) => x.group === "problem").map((x) => x.p);
  const [, setParams] = useSearchParams();
  if (!problems.length) return null;
  const n = problems.length;
  const shown = problems.slice(0, 6);
  return (
    <div role="status" className="mb-6 flex flex-wrap items-baseline gap-x-2 gap-y-1 rounded-md border border-warn/30 bg-warn-soft/60 px-4 py-2.5 text-sm">
      <AlertTriangle className="h-4 w-4 shrink-0 self-center text-warn" aria-hidden />
      <span className="font-medium">
        {n} {plural(n, ["project needs", "projects need"])} attention:
      </span>
      {shown.map((p, i) => (
        <span key={p.name}>
          <Link to={`/projects/${encodeURIComponent(p.name)}/history`} className={cn("font-medium hover:underline", p.overdue_since || p.last_run?.status === "failed" ? "text-danger" : "text-warn")}>{p.name}</Link>
          {i < shown.length - 1 && <span className="text-faint">,</span>}
        </span>
      ))}
      {n > shown.length && (
        <button
          type="button"
          className="text-muted underline underline-offset-2 hover:text-fg"
          onClick={() => setParams((sp) => { const x = new URLSearchParams(sp); x.set("f", "problem"); x.delete("q"); return x; }, { replace: true })}
        >
          and {n - shown.length} more
        </button>
      )}
    </div>
  );
}

function ProjectTable({ projects }: { projects: ProjectSummary[] }) {
  const nav = useNavigate();
  const now = useNow(60_000);
  const [params, setParams] = useSearchParams();
  const f = params.get("f");
  const filter: Filter = FILTERS.includes(f as Filter) ? (f as Filter) : "all";
  const q = params.get("q") ?? "";
  const setParam = (k: string, v: string | null) =>
    setParams((sp) => { const x = new URLSearchParams(sp); if (v) x.set(k, v); else x.delete(k); return x; }, { replace: true });

  const sorted = useMemo(() => sortProjects(projects), [projects]);
  const counts = useMemo(() => {
    const c: Record<Filter, number> = { all: sorted.length, problem: 0, active: 0, ok: 0 };
    for (const x of sorted) c[x.group]++;
    return c;
  }, [sorted]);
  const needle = q.trim().toLowerCase();
  const shown = sorted.filter((x) => (filter === "all" || x.group === filter) && (!needle || x.p.name.toLowerCase().includes(needle)));

  return (
    <section aria-labelledby="projects-h" className="mb-8">
      <div className="mb-2 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div className="flex items-baseline gap-2">
          <h2 id="projects-h" className="text-base font-semibold">Projects</h2>
          <span className="text-sm text-faint tnum">{projects.length}</span>
        </div>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <Segmented<Filter>
            size="sm"
            ariaLabel="Project filter"
            value={filter}
            onChange={(v) => setParam("f", v === "all" ? null : v)}
            options={FILTERS.filter((k) => k === "all" || k === filter || counts[k] > 0).map((k) => ({
              value: k,
              label: (
                <>
                  {FILTER_LABEL[k]}
                  <span className={cn("tnum font-normal", k === "problem" && counts.problem > 0 ? "text-danger" : "text-faint")}>{counts[k]}</span>
                </>
              ),
            }))}
          />
          <TextInput
            value={q}
            onChange={(e) => setParam("q", e.target.value || null)}
            onKeyDown={(e) => { if (e.key === "Escape" && q) { e.preventDefault(); setParam("q", null); } }}
            placeholder="Search by name"
            aria-label="Search projects by name"
            autoComplete="off"
            spellCheck={false}
            leading={<Search className="h-3.5 w-3.5" aria-hidden />}
            className="h-7 w-44 text-xs"
          />
        </div>
      </div>
      <div className="card overflow-hidden">
        <div className="hidden grid-cols-[minmax(0,2.2fr)_minmax(0,1.6fr)_minmax(0,1fr)_minmax(0,0.9fr)_112px] gap-4 border-b border-line bg-sunken/60 px-4 py-2 text-xs text-muted md:grid">
          <span>Project</span><span>Last run</span><span>Next run</span><span className="text-right">Size</span><span />
        </div>
        {shown.length === 0 && (
          <Empty icon={<SearchX className="h-5 w-5" />} title="Nothing found" className="py-8">
            <button type="button" className="underline underline-offset-2 hover:text-fg" onClick={() => setParams(new URLSearchParams(), { replace: true })}>Reset filters</button>
          </Empty>
        )}
        <ul className="divide-y divide-line">
          {shown.map(({ p }) => {
            const st = statusOf(p);
            const href = `/projects/${encodeURIComponent(p.name)}`;
            const lastAt = p.last_run?.finished_at ?? p.last_run?.started_at ?? null;
            return (
              <li
                key={p.name}
                onClick={(e) => { if (!(e.target as HTMLElement).closest("a,button")) nav(href); }}
                className="group grid cursor-pointer grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-2 px-4 py-3 transition-colors hover:bg-sunken/50 md:grid-cols-[minmax(0,2.2fr)_minmax(0,1.6fr)_minmax(0,1fr)_minmax(0,0.9fr)_112px]"
              >
                <div className="flex min-w-0 items-center gap-3">
                  <EngineMark engine={p.engine} />
                  <div className="min-w-0">
                    <Link to={href} className="block truncate font-medium hover:text-accent">{p.name}</Link>
                    <div className="truncate text-xs text-muted">
                      {p.databases === "all" ? `${p.database_count} ${plural(p.database_count, ["database", "databases"])}` : `${p.databases.length} selected`}
                      <span className="text-faint"> · </span>
                      {humanSchedule(p.interval, p.start_at)}
                    </div>
                  </div>
                </div>

                <div className="col-start-1 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-sm md:col-start-auto">
                  <Tooltip content={p.last_run?.error ?? undefined}>
                    <span className="inline-flex"><StatusBadge status={st} /></span>
                  </Tooltip>
                  {lastAt && st !== "running" && st !== "queued" && <RelTime iso={lastAt} className="text-muted" />}
                  {p.last_run && p.last_run.status === "partial" && (
                    <span className="text-xs text-muted tnum">{p.last_run.db_failed} of {p.last_run.db_ok + p.last_run.db_failed} not saved</span>
                  )}
                  {st === "overdue" && p.last_run && p.last_run.status !== "ok" && (
                    <StatusBadge status={p.last_run.status} />
                  )}
                  {p.overdue_since ? (
                    <Tooltip content={<>Overdue since {formatDateTime(p.overdue_since)}{p.overdue_after ? ` · allowed without success: ${windowText(p.overdue_after)}` : ""}</>}>
                      <span tabIndex={0} className="w-full rounded text-xs font-medium text-danger">{overdueText(p.last_success_at, now)}</span>
                    </Tooltip>
                  ) : p.last_run && p.last_run.status !== "ok" && p.last_success_at && st !== "running" && (
                    <span className="w-full text-xs text-faint">success <RelTime iso={p.last_success_at} /></span>
                  )}
                  {!p.uri_set && <Badge tone="warn">no connection string</Badge>}
                </div>

                <div className="hidden text-sm md:block"><NextDue p={p} /></div>

                <div className="hidden text-right text-sm md:block">
                  <div className="tnum">{p.archive_count ? formatBytes(p.total_bytes) : <span className="text-faint">—</span>}</div>
                  <div className="text-xs text-faint tnum">{p.archive_count} {plural(p.archive_count, ["archive", "archives"])}</div>
                </div>

                <div className="col-start-2 row-span-2 row-start-1 flex justify-end md:col-start-auto md:row-span-1 md:row-start-auto">
                  <RunNowButton name={p.name} state={p.state} uriSet={p.uri_set} />
                </div>

                <div className="col-span-2 flex gap-4 text-xs text-muted md:hidden">
                  <span>Next run: <NextDue p={p} /></span>
                  <span className="ml-auto tnum">{p.archive_count ? formatBytes(p.total_bytes) : "—"}</span>
                </div>
              </li>
            );
          })}
        </ul>
      </div>
    </section>
  );
}

const LEVEL: Record<string, { icon: JSX.Element; label: string }> = {
  error: { icon: <XCircle className="h-4 w-4 text-danger" aria-hidden />, label: "Error" },
  warning: { icon: <AlertTriangle className="h-4 w-4 text-warn" aria-hidden />, label: "Warning" },
  info: { icon: <Info className="h-4 w-4 text-accent" aria-hidden />, label: "Event" },
};

function AlertsFeed({ alerts }: { alerts: Alert[] }) {
  const dismiss = useDismissAlert();
  const dismissAll = useDismissAllAlerts();
  const { success, error } = useToast();
  const hideAll = () => {
    const n = alerts.length;
    dismissAll.mutate(undefined, {
      onSuccess: () => success(`Dismissed ${n} ${plural(n, ["notification", "notifications"])}`),
      onError: (e) => error(e, "Could not dismiss notifications"),
    });
  };
  return (
    <section aria-labelledby="alerts-h">
      <div className="mb-2 flex items-baseline gap-2">
        <h2 id="alerts-h" className="text-base font-semibold">Notifications</h2>
        {alerts.length > 0 && <span className="text-sm text-faint tnum">latest {Math.min(alerts.length, 20)}</span>}
        {alerts.length > 0 && (
          <Button variant="ghost" size="sm" className="ml-auto self-center" onClick={hideAll} disabled={dismissAll.isPending}>
            Dismiss all
          </Button>
        )}
      </div>
      <div className="card">
        {alerts.length === 0 ? (
          <Empty icon={<Bell className="h-5 w-5" />} title="No notifications" className="py-8">
            Run failures, warnings about sudden archive shrinkage and storage problems will appear here.
          </Empty>
        ) : (
          <ul className="divide-y divide-line">
            {alerts.map((a) => {
              const lv = LEVEL[a.level] ?? { icon: <ServerCrash className="h-4 w-4 text-muted" aria-hidden />, label: a.level };
              return (
                <li key={a.id} className="flex gap-3 px-4 py-2.5">
                  <span className="mt-0.5" title={lv.label}>{lv.icon}<span className="sr-only">{lv.label}</span></span>
                  <div className="min-w-0 flex-1">
                    <p className="break-words text-sm">{a.text}</p>
                    <div className="mt-0.5 flex gap-2 text-xs text-muted">
                      {a.project && <Link to={`/projects/${encodeURIComponent(a.project)}/history`} className="font-medium hover:text-accent">{a.project}</Link>}
                      <RelTime iso={a.ts} />
                    </div>
                  </div>
                  <Tooltip content="Dismiss">
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      className="-mr-1.5 -mt-0.5 text-faint"
                      aria-label={`Dismiss notification: ${a.text}`}
                      onClick={() => dismiss.mutate(a.id, { onError: (e) => error(e, "Could not dismiss notification") })}
                    >
                      <X className="h-3.5 w-3.5" aria-hidden />
                    </Button>
                  </Tooltip>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </section>
  );
}
