import { AlertTriangle, CalendarClock, History, MousePointerClick } from "lucide-react";
import { useMemo } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { describeError } from "@/api/client";
import { useAllRuns } from "@/api/queries";
import type { RunSummary } from "@/api/types";
import { Page } from "@/components/layout/AppShell";
import { StatusBadge } from "@/components/Status";
import { Button } from "@/components/ui/Button";
import { Empty, ErrorState } from "@/components/ui/Empty";
import { Segmented } from "@/components/ui/Segmented";
import { Select, type SelectOption } from "@/components/ui/Select";
import { SkeletonRows } from "@/components/ui/Skeleton";
import { Tooltip } from "@/components/ui/Tooltip";
import { cn } from "@/lib/cn";
import { formatBytes, formatDateTime, formatDuration, formatExact, plural, secondsBetween } from "@/lib/format";
import { useDocumentTitle, useNow } from "@/lib/hooks";

type StatusFilter = "all" | "problem" | "ok" | "interrupted";
type Period = "24h" | "7d" | "30d" | "all";

const STATUS_FILTERS: { value: StatusFilter; label: string }[] = [
  { value: "all", label: "All" },
  { value: "problem", label: "Problems" },
  { value: "ok", label: "Successful" },
  { value: "interrupted", label: "Interrupted" },
];
const PERIODS: { value: Period; label: string; ms: number | null }[] = [
  { value: "24h", label: "24 h", ms: 86400e3 },
  { value: "7d", label: "7 days", ms: 7 * 86400e3 },
  { value: "30d", label: "30 days", ms: 30 * 86400e3 },
  { value: "all", label: "All time", ms: null },
];
const DEFAULT_PERIOD: Period = "7d";
const ALL = "";

const GRID = "md:grid md:grid-cols-[104px_minmax(0,1.4fr)_minmax(0,1fr)_88px_72px_88px] md:gap-x-4";

function Duration({ run }: { run: RunSummary }) {
  const now = useNow(run.finished_at ? 60_000 : 1000);
  return <span className="tnum">{formatDuration(secondsBetween(run.started_at, run.finished_at ?? now))}</span>;
}

function RunLine({ run }: { run: RunSummary }) {
  const nav = useNavigate();
  const project = `/projects/${encodeURIComponent(run.project)}`;
  const total = run.db_ok + run.db_failed;
  const manual = run.trigger === "manual";
  const note = [run.error, ...run.warnings].filter(Boolean).join("\n");
  return (
    <li
      onClick={(e) => { if (!(e.target as HTMLElement).closest("a,button")) nav(`${project}/history`); }}
      className={cn(GRID, "flex cursor-pointer flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 text-sm transition-colors hover:bg-sunken/50")}
    >
      <span className="flex"><StatusBadge status={run.status} /></span>
      <span className="flex min-w-0 items-center gap-2">
        <Link to={project} className="truncate font-medium hover:text-accent">{run.project}</Link>
        <Tooltip content={manual ? "Started manually" : "Scheduled"}>
          <span tabIndex={0} className="flex shrink-0 items-center gap-1 rounded text-xs text-faint">
            {manual ? <MousePointerClick className="h-3.5 w-3.5" aria-hidden /> : <CalendarClock className="h-3.5 w-3.5" aria-hidden />}
            <span className="hidden lg:inline">{manual ? "manual" : "scheduled"}</span>
            <span className="sr-only lg:hidden">{manual ? "manual" : "scheduled"}</span>
          </span>
        </Tooltip>
      </span>
      <Tooltip content={formatExact(run.started_at)}>
        <span tabIndex={0} className="w-fit whitespace-nowrap rounded text-muted tnum">{formatDateTime(run.started_at)}</span>
      </Tooltip>
      <span className="text-muted md:text-left"><Duration run={run} /></span>
      <span className="text-muted tnum" title={`${run.db_ok} of ${total} saved`}>
        <span className="text-fg">{run.db_ok}</span>/{total}<span className="md:hidden"> {plural(total, ["database", "databases"])}</span>
        {run.db_failed > 0 && <span className="ml-1.5 text-danger">−{run.db_failed}</span>}
      </span>
      <span className="ml-auto text-right tnum md:ml-0">{run.bytes ? formatBytes(run.bytes) : <span className="text-faint">—</span>}</span>
      {note && (
        <p
          title={note}
          className={cn(
            "flex w-full min-w-0 items-center gap-1.5 text-xs md:col-span-5 md:col-start-2",
            run.error ? "text-danger" : "text-warn",
          )}
        >
          {!run.error && <AlertTriangle className="h-3.5 w-3.5 shrink-0" aria-hidden />}
          <span className="truncate font-mono">{run.error ?? run.warnings[0]}</span>
          {run.warnings.length > (run.error ? 0 : 1) && (
            <span className="shrink-0 text-faint">
              +{run.warnings.length - (run.error ? 0 : 1)} {plural(run.warnings.length - (run.error ? 0 : 1), ["warning", "warnings"])}
            </span>
          )}
        </p>
      )}
    </li>
  );
}

export function HistoryPage() {
  useDocumentTitle("History");
  const [params, setParams] = useSearchParams();
  const project = params.get("project") ?? ALL;
  const status = (STATUS_FILTERS.find((s) => s.value === params.get("status"))?.value ?? "all") as StatusFilter;
  const period = (PERIODS.find((p) => p.value === params.get("period"))?.value ?? DEFAULT_PERIOD) as Period;

  const set = (k: string, v: string | null) =>
    setParams((sp) => { const x = new URLSearchParams(sp); if (v) x.set(k, v); else x.delete(k); return x; }, { replace: true });

  // Fixed per period choice, so the query key does not change on every render.
  const since = useMemo(() => {
    const ms = PERIODS.find((p) => p.value === period)?.ms;
    return ms ? new Date(Date.now() - ms).toISOString() : null;
  }, [period]);

  const q = useAllRuns({ project: project || null, status: status === "all" ? null : status, since });
  const runs = useMemo(() => q.data?.pages.flatMap((p) => p.runs) ?? [], [q.data]);
  const known = q.data?.pages[0]?.projects;

  const projectOptions = useMemo<SelectOption<string>[]>(() => {
    const names = [...(known ?? [])];
    if (project && !names.includes(project)) names.push(project);
    names.sort((a, b) => a.localeCompare(b));
    return [{ value: ALL, label: "All projects", text: "all projects" }, ...names.map((n) => ({ value: n, label: n, text: n }))];
  }, [known, project]);

  const filtered = !!project || status !== "all" || period !== "all";

  return (
    <Page wide title="History" meta="Runs of all projects, newest first.">
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <Select<string>
          ariaLabel="Project"
          value={project}
          onChange={(v) => set("project", v || null)}
          options={projectOptions}
          className="h-8 w-56 text-sm"
        />
        <Segmented<StatusFilter>
          ariaLabel="Run status"
          value={status}
          onChange={(v) => set("status", v === "all" ? null : v)}
          options={STATUS_FILTERS}
        />
        <Segmented<Period>
          ariaLabel="Period"
          value={period}
          onChange={(v) => set("period", v === DEFAULT_PERIOD ? null : v)}
          options={PERIODS.map(({ value, label }) => ({ value, label }))}
        />
        {q.isFetching && !q.isFetchingNextPage && !q.isLoading && <span className="text-xs text-faint">refreshing…</span>}
      </div>

      {q.isLoading && <SkeletonRows rows={10} />}
      {q.error && !q.data && <ErrorState {...describeError(q.error)} onRetry={() => void q.refetch()} />}
      {q.data && (
        runs.length === 0 ? (
          <div className="card">
            <Empty icon={<History className="h-5 w-5" />} title={filtered ? "No runs found" : "No runs yet"}>
              {filtered ? (
                <button type="button" className="underline underline-offset-2 hover:text-fg" onClick={() => setParams(new URLSearchParams({ period: "all" }), { replace: true })}>
                  Show all runs
                </button>
              ) : "Runs of all projects will appear here."}
            </Empty>
          </div>
        ) : (
          <>
            <div className={cn("card overflow-hidden transition-opacity", q.isPlaceholderData && "opacity-60")}>
              <div className={cn(GRID, "hidden border-b border-line bg-sunken/60 px-4 py-2 text-xs text-muted")}>
                <span>Status</span><span>Project</span><span>Started</span><span>Duration</span><span>Databases</span><span className="text-right">Size</span>
              </div>
              <ul className="divide-y divide-line">
                {runs.map((r) => <RunLine key={r.id} run={r} />)}
              </ul>
            </div>
            <div className="mt-4 flex items-center justify-center gap-3">
              {q.hasNextPage ? (
                <Button onClick={() => void q.fetchNextPage()} loading={q.isFetchingNextPage}>Show more</Button>
              ) : (
                <span className="text-xs text-faint tnum">{runs.length} {plural(runs.length, ["run", "runs"])}</span>
              )}
            </div>
          </>
        )
      )}
    </Page>
  );
}
