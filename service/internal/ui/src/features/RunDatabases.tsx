import { Check, Circle, Loader2, Minus, X } from "lucide-react";
import { useEffect, useRef } from "react";
import type { RunningDb, RunningOp } from "@/api/types";
import { Tooltip } from "@/components/ui/Tooltip";
import { cn } from "@/lib/cn";
import { formatBytes, formatClock, formatDuration, plural, secondsBetween } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { Badge } from "@/components/ui/Badge";
import { DbTrack, PHASE_LABEL } from "@/features/RunningPanel";

const LABEL: Record<RunningDb["status"], string> = {
  pending: "Pending",
  running: "Dumping now",
  ok: "Saved",
  unchanged: "Unchanged: a fresh archive already exists",
  failed: "Failed",
};

function Icon({ status }: { status: RunningDb["status"] }) {
  const c = "h-3.5 w-3.5 shrink-0";
  if (status === "ok") return <Check className={cn(c, "text-ok")} aria-hidden />;
  if (status === "running") return <Loader2 className={cn(c, "animate-spin text-accent")} aria-hidden />;
  if (status === "failed") return <X className={cn(c, "text-danger")} aria-hidden />;
  if (status === "unchanged") return <Minus className={cn(c, "text-muted")} aria-hidden />;
  return <Circle className={cn(c, "text-line-strong")} aria-hidden />;
}

/** Counters for the run: done / now / ahead / failed. */
export function runCounts(dbs: RunningDb[]) {
  const done = dbs.filter((d) => d.status === "ok" || d.status === "unchanged" || d.status === "failed").length;
  return {
    done,
    failed: dbs.filter((d) => d.status === "failed").length,
    pending: dbs.filter((d) => d.status === "pending").length,
    current: dbs.find((d) => d.status === "running") ?? null,
  };
}

export function RunCountsLine({ dbs, className }: { dbs: RunningDb[]; className?: string }) {
  const c = runCounts(dbs);
  return (
    <div className={cn("flex flex-wrap items-baseline gap-x-4 gap-y-1 text-xs text-muted", className)}>
      <span className="tnum">Done <span className="font-medium text-fg">{c.done}</span> of {dbs.length}</span>
      {c.failed > 0 && <span className="tnum text-danger">{c.failed} {plural(c.failed, ["error", "errors"])}</span>}
      <span className="tnum">{c.pending} {plural(c.pending, ["database", "databases"])} to go</span>
    </div>
  );
}

/** Every database of a running run in run order: done ones with their result, the current one, the rest ahead. */
export function RunDatabases({ op, className }: { op: RunningOp; className?: string }) {
  const now = useNow(1000);
  const box = useRef<HTMLOListElement>(null);
  const dbs = op.databases ?? [];
  const current = dbs.find((d) => d.status === "running")?.database;
  // Keep the current database in view inside the scrolling list (without scrolling the page).
  useEffect(() => {
    const el = box.current;
    const cur = el?.querySelector<HTMLElement>("[data-current]");
    if (el && cur && el.scrollHeight > el.clientHeight) el.scrollTop = Math.max(0, cur.offsetTop - el.clientHeight / 3);
  }, [current]);
  if (!dbs.length) return null;
  const elapsed = secondsBetween(op.db_started_at, now);
  return (
    <ol ref={box} aria-label="Run databases" className={cn("relative grid content-start gap-x-6 sm:grid-cols-2 xl:grid-cols-3", className)}>
      {dbs.map((d) => (
        <li
          key={d.database}
          data-current={d.status === "running" ? "" : undefined}
          className={cn(
            "flex min-w-0 items-center gap-2 rounded px-1.5 py-1 text-sm",
            d.status === "running" && "bg-accent-soft/60",
            d.status === "pending" && "text-faint",
          )}
        >
          <Tooltip content={d.error ? `${LABEL[d.status]}: ${d.error}` : LABEL[d.status]}>
            <span tabIndex={0} className="flex rounded" aria-label={LABEL[d.status]}><Icon status={d.status} /></span>
          </Tooltip>
          <span className={cn("truncate font-mono text-[12.5px]", d.status === "running" && "text-fg", d.status === "failed" && "text-danger")}>
            {d.database}
          </span>
          <span className="ml-auto shrink-0 pl-2 text-xs tnum text-faint">
            {d.status === "running"
              ? <>{op.attempt > 1 && <span className="mr-1.5 text-warn">attempt {op.attempt}</span>}{elapsed != null ? formatClock(elapsed) : ""}</>
              : d.status === "ok" ? <span className="text-muted">{formatBytes(d.bytes)} · {formatDuration(d.seconds)}</span>
              : d.status === "unchanged" ? "unchanged"
              : d.status === "failed" ? <span className="text-danger">failed</span>
              : ""}
          </span>
        </li>
      ))}
    </ol>
  );
}

/** The run in progress on the project page: phase, progress and the list of databases. Shown above the tabs. */
export function LiveRunCard({ op }: { op: RunningOp }) {
  const now = useNow(1000);
  const dbs = op.databases ?? [];
  const elapsed = secondsBetween(op.started_at, now) ?? 0;
  return (
    <section aria-label="Current run" className="card mb-6 overflow-hidden border-accent/30">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-line bg-accent-soft/50 px-4 py-2">
        <h2 className="text-sm font-medium">Run <span className="font-mono text-xs text-faint">#{op.run_id}</span></h2>
        {op.state === "cancelling"
          ? <Badge tone="warn">Stopping</Badge>
          : <Badge tone="accent">{PHASE_LABEL[op.phase] ?? op.phase}</Badge>}
        {dbs.length > 0 && <RunCountsLine dbs={dbs} />}
        <span className="ml-auto font-mono text-sm tnum text-muted" aria-label="Elapsed time">{formatClock(elapsed)}</span>
      </div>
      <div className="px-4 py-3">
        <DbTrack index={op.db_index} total={op.db_total} phase={op.phase} className="mb-3" />
        <CurrentDbProgress op={op} className="mb-3" />
        {dbs.length > 0 ? (
          <RunDatabases op={op} className="max-h-72 overflow-y-auto" />
        ) : (
          <p className="text-xs text-muted">
            {op.phase === "queued" ? "Waiting for a free slot: only a limited number of runs execute at once" : "Listing databases…"}
          </p>
        )}
      </div>
    </section>
  );
}

/**
 * Bytes of the current database: how much is written, against the previous archive's size when there is one,
 * with rate and a rough time left. Shows the dump is moving even when one database takes many minutes.
 */
export function CurrentDbProgress({ op, className }: { op: RunningOp; className?: string }) {
  const done = op.bytes_done ?? 0;
  const expected = op.bytes_expected ?? 0;
  const rate = op.bytes_per_sec ?? 0;
  if (op.phase === "upload") {
    return (
      <div className={cn("text-xs text-muted", className)}>
        Uploading to storage{expected > 0 && <> <span className="tnum text-fg">{formatBytes(expected)}</span></>}
      </div>
    );
  }
  if (op.phase !== "dump") return null;
  const known = expected > 0 && done <= expected * 1.05;
  const pct = known ? Math.min(99, Math.round((done / expected) * 100)) : null;
  const left = known && rate > 0 ? Math.max(0, (expected - done) / rate) : null;
  return (
    <div className={cn("space-y-1.5", className)}>
      {known && (
        <div className="h-1 overflow-hidden rounded-full bg-line/70" aria-hidden>
          <div className="h-full rounded-full bg-accent transition-[width] duration-700" style={{ width: `${pct}%` }} />
        </div>
      )}
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 text-xs text-muted">
        <span className="tnum">
          {done > 0 ? <>written <span className="text-fg">{formatBytes(done)}</span></> : "connecting, no data yet"}
          {known && <> of ~{formatBytes(expected)} <span className="text-fg">({pct}%)</span></>}
        </span>
        {rate > 0 && <span className="tnum">{formatBytes(rate)}/s</span>}
        {left != null && <span className="tnum">~{formatDuration(Math.max(1, Math.round(left)))} left</span>}
        {expected === 0 && done > 0 && <span className="text-faint">final size unknown: no previous archive</span>}
        {expected > 0 && !known && <span className="text-faint">already larger than the previous archive ({formatBytes(expected)})</span>}
      </div>
    </div>
  );
}
