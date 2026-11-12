import { AlertTriangle, CalendarClock, ChevronRight, History, MousePointerClick } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { describeError } from "@/api/client";
import { useRuns } from "@/api/queries";
import type { DbRun, RunDetail, RunningOp } from "@/api/types";
import { StatusBadge, STATUS } from "@/components/Status";
import { Badge, Dot, type Tone } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Empty, ErrorState } from "@/components/ui/Empty";
import { Segmented } from "@/components/ui/Segmented";
import { SkeletonRows } from "@/components/ui/Skeleton";
import { Tooltip } from "@/components/ui/Tooltip";
import { DbTrack, PHASE_LABEL } from "@/features/RunningPanel";
import { cn } from "@/lib/cn";
import { formatBytes, formatDateTime, formatDuration, formatExact, plural, secondsBetween } from "@/lib/format";
import { useNow } from "@/lib/hooks";

const DB_TONE: Record<string, Tone> = { ok: "ok", unchanged: "neutral", failed: "danger", partial: "warn", running: "accent", interrupted: "idle", skipped: "neutral" };
const DB_STATUS_LABEL: Record<string, string> = { ok: "Saved", unchanged: "Unchanged: a fresh archive already exists", failed: "Failed", partial: "Partial", running: "Running", interrupted: "Interrupted", skipped: "Skipped" };
/** Database results that are not a problem ("unchanged": a fresh archive is already stored — s3import source archive, or a scheduled retry). */
const isOk = (d: DbRun) => d.status === "ok" || d.status === "unchanged";

function Duration({ run }: { run: RunDetail }) {
  const now = useNow(run.finished_at ? 60_000 : 1000);
  const s = secondsBetween(run.started_at, run.finished_at ?? now);
  return <span className="tnum">{formatDuration(s)}</span>;
}

/**
 * Shared by the header, the run rows and the per-database rows inside an expanded run, so the
 * nested numbers sit under the outer columns: Duration ← time, Databases ← attempts, Size ← size.
 */
const GRID = "grid grid-cols-[16px_minmax(0,1fr)_auto] gap-x-3 md:grid-cols-[16px_110px_minmax(0,1fr)_110px_90px_90px]";

function DbTable({ dbs }: { dbs: DbRun[] }) {
  const sorted = [...dbs].sort((a, b) => Number(!isOk(b)) - Number(!isOk(a)));
  if (!sorted.length) return <p className="py-1 pl-10 pr-3 text-sm text-muted">No per-database data.</p>;
  return (
    <div role="table" aria-label="Run databases" className="text-sm">
      <div role="row" className={cn(GRID, "hidden px-3 pb-1 text-xs text-faint md:grid")}>
        <span aria-hidden />
        <span role="columnheader" className="md:col-span-2">Database</span>
        <span role="columnheader">Time</span>
        <span role="columnheader">Attempts</span>
        <span role="columnheader" className="text-right">Size</span>
      </div>
      <div className="divide-y divide-line/60">
        {sorted.map((d) => (
          <div role="row" key={d.database} className={cn(GRID, "items-start px-3 py-1.5")}>
            <span aria-hidden />
            <div role="cell" className="min-w-0 md:col-span-2">
              <div className="flex min-w-0 items-center gap-2">
                <Tooltip content={DB_STATUS_LABEL[d.status] ?? d.status}>
                  <span className="flex shrink-0"><Dot tone={DB_TONE[d.status] ?? "neutral"} /></span>
                </Tooltip>
                <span className="truncate font-mono text-[12.5px]">{d.database}</span>
                {d.status === "unchanged" && <span className="shrink-0 text-xs text-faint">unchanged</span>}
                <span className="shrink-0 text-xs text-faint tnum md:hidden">{formatDuration(d.seconds)}</span>
                {d.attempts > 1 && <span className="md:hidden"><Badge tone="warn">{d.attempts} {plural(d.attempts, ["attempt", "attempts"])}</Badge></span>}
              </div>
              {d.error && <p className="mt-1 break-words pl-4 font-mono text-xs text-danger">{d.error}</p>}
            </div>
            <span role="cell" className="hidden text-muted tnum md:block">{formatDuration(d.seconds)}</span>
            <span role="cell" className="hidden md:block">{d.attempts > 1 ? <Badge tone="warn">{d.attempts}</Badge> : <span className="text-muted tnum">{d.attempts}</span>}</span>
            <span role="cell" className="text-right tnum">{d.status === "ok" ? formatBytes(d.bytes) : d.status === "unchanged" ? <span className="text-muted">{formatBytes(d.bytes)}</span> : <span className="text-faint">—</span>}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function RunRow({ run, open, onToggle, op }: { run: RunDetail; open: boolean; onToggle: () => void; op?: RunningOp }) {
  const total = run.db_ok + run.db_failed;
  const live = run.status === "running";
  return (
    <li>
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        className={cn(GRID, "w-full items-center gap-y-1 px-3 py-2.5 text-left hover:bg-sunken/60")}
      >
        <ChevronRight className={cn("h-4 w-4 text-faint transition-transform", open && "rotate-90")} aria-hidden />
        <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 md:contents">
          <span className="md:order-none"><StatusBadge status={run.status} /></span>
          <span className="flex min-w-0 items-center gap-2 text-sm">
            <span className="min-w-[2.75rem] shrink-0 font-mono text-xs text-faint tnum">#{run.id}</span>
            <Tooltip content={formatExact(run.started_at)}>
              <span tabIndex={0} className="whitespace-nowrap tnum rounded">{formatDateTime(run.started_at)}</span>
            </Tooltip>
            <Tooltip content={run.trigger === "manual" ? "Started manually" : "Scheduled"}>
              <span tabIndex={0} className="hidden rounded text-faint sm:inline" aria-label={run.trigger === "manual" ? "manual" : "scheduled"}>
                {run.trigger === "manual" ? <MousePointerClick className="h-3.5 w-3.5" /> : <CalendarClock className="h-3.5 w-3.5" />}
              </span>
            </Tooltip>
            {run.warnings.length > 0 && (
              <Tooltip content={`${run.warnings.length} ${plural(run.warnings.length, ["warning", "warnings"])}`}>
                <span tabIndex={0} className="rounded text-warn"><AlertTriangle className="h-3.5 w-3.5" aria-label="has warnings" /></span>
              </Tooltip>
            )}
          </span>
        </span>
        <span className="text-right text-sm text-muted md:text-left"><Duration run={run} /></span>
        <span className="col-start-2 text-sm tnum text-muted md:col-start-auto">
          {live && op ? `${Math.max(0, op.db_index - 1)}/${op.db_total}` : (
            <>
              <span className="text-fg">{run.db_ok}</span>/{total}<span className="md:hidden"> {plural(total, ["database", "databases"])}</span>
              {run.db_failed > 0 && <span className="ml-1.5 text-danger">−{run.db_failed}</span>}
            </>
          )}
        </span>
        <span className="text-right text-sm tnum md:text-right">{run.bytes ? formatBytes(run.bytes) : <span className="text-faint">—</span>}</span>
      </button>
      {open && (
        <div className="space-y-3 border-t border-line/60 bg-sunken/40 py-3">
          {live && op && (
            <div className="ml-10 mr-3 rounded-md border border-accent/25 bg-surface px-3 py-2.5">
              <div className="mb-2 flex flex-wrap gap-x-3 text-xs text-muted">
                <span className="font-medium text-accent">{PHASE_LABEL[op.phase]}</span>
                {op.database && <span className="font-mono text-fg">{op.database}</span>}
                {op.attempt > 1 && <span className="text-warn">attempt {op.attempt}</span>}
              </div>
              <DbTrack index={op.db_index} total={op.db_total} phase={op.phase} />
            </div>
          )}
          {run.error && (
            <div role="alert" className="ml-10 mr-3 rounded-md border border-danger/25 bg-danger-soft/50 px-3 py-2">
              <p className="text-xs font-medium text-danger">Run error</p>
              <p className="mt-0.5 break-words font-mono text-xs">{run.error}</p>
            </div>
          )}
          {run.warnings.length > 0 && (
            <ul className="space-y-1 pl-10 pr-3">
              {run.warnings.map((w, i) => (
                <li key={i} className="flex gap-2 text-xs text-warn"><AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" aria-hidden /><span className="break-words">{w}</span></li>
              ))}
            </ul>
          )}
          <DbTable dbs={run.databases} />
          <p className="pl-10 pr-3 text-xs text-faint">
            {STATUS[run.status]?.hint}. {run.finished_at ? <>Finished {formatDateTime(run.finished_at)}.</> : null}
          </p>
        </div>
      )}
    </li>
  );
}

export function HistoryTab({ name, live, running }: { name: string; live: boolean; running?: RunningOp }) {
  const q = useRuns(name, live);
  const [filter, setFilter] = useState<"all" | "problems">("all");
  const [open, setOpen] = useState<Set<number>>(new Set());
  const runs = useMemo(() => q.data?.pages.flatMap((p) => p.runs) ?? [], [q.data]);
  const shown = filter === "all" ? runs : runs.filter((r) => r.status !== "ok" && r.status !== "running");

  // Open the newest finished run automatically when it is not a clean success.
  const firstDone = runs.find((r) => r.status !== "running");
  useEffect(() => {
    if (firstDone && firstDone.status !== "ok") setOpen((s) => (s.size ? s : new Set([firstDone.id])));
  }, [firstDone?.id]);

  if (q.isLoading) return <SkeletonRows rows={8} />;
  if (q.error) return <ErrorState {...describeError(q.error)} onRetry={() => void q.refetch()} />;
  if (!runs.length) {
    return (
      <div className="card">
        <Empty icon={<History className="h-5 w-5" />} title="No runs yet">
          The first run will follow the schedule. To skip the wait, click "Run now" at the top.
        </Empty>
      </div>
    );
  }

  const toggle = (id: number) => setOpen((s) => { const n = new Set(s); if (n.has(id)) n.delete(id); else n.add(id); return n; });

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-3">
        <Segmented size="sm" ariaLabel="Run filter" value={filter} onChange={setFilter} options={[{ value: "all", label: "All" }, { value: "problems", label: "With problems" }]} />
        <span className="text-xs text-faint tnum">{runs.length} loaded</span>
      </div>
      <div className="card overflow-hidden">
        <div className={cn(GRID, "hidden border-b border-line bg-sunken/60 px-3 py-2 text-xs text-muted md:grid")}>
          <span /><span>Status</span><span>Run</span><span>Duration</span><span>Databases</span><span className="text-right">Size</span>
        </div>
        <ul className="divide-y divide-line">
          {shown.map((r) => (
            <RunRow key={r.id} run={r} open={open.has(r.id)} onToggle={() => toggle(r.id)} op={running && running.run_id === r.id ? running : undefined} />
          ))}
          {shown.length === 0 && <li className="px-4 py-6 text-center text-sm text-muted">No problems among the loaded runs</li>}
        </ul>
      </div>
      {q.hasNextPage && (
        <div className="flex justify-center">
          <Button onClick={() => void q.fetchNextPage()} loading={q.isFetchingNextPage}>Show more</Button>
        </div>
      )}
    </div>
  );
}
