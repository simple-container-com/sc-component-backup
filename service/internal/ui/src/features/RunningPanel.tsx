import { ChevronRight } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import type { Phase, RunningOp } from "@/api/types";
import { cn } from "@/lib/cn";
import { formatClock, plural, secondsBetween } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { Badge } from "@/components/ui/Badge";
import { CurrentDbProgress, RunDatabases } from "@/features/RunDatabases";
import { StopRunButton } from "@/features/StopRunButton";

export const PHASE_LABEL: Record<Phase, string> = {
  queued: "Queued",
  discover: "Finding databases",
  dump: "Dump",
  upload: "Uploading to storage",
  prune: "Pruning archives",
};

/** One cell per database: done / current / pending. The run reads like a tape filling up. */
export function DbTrack({ index, total, phase, className }: { index: number; total: number; phase: Phase; className?: string }) {
  if (phase === "queued" || phase === "discover" || total <= 0) {
    return (
      <div className={cn("relative h-2.5 overflow-hidden rounded-sm bg-line/70", className)} aria-hidden>
        {phase === "discover" && <div className="stripes absolute inset-0 animate-stripe bg-accent/50" />}
      </div>
    );
  }
  const done = phase === "prune" ? total : Math.max(0, index - 1);
  return (
    <div className={cn("flex h-2.5 gap-[2px]", className)} aria-hidden>
      {Array.from({ length: total }, (_, i) => (
        <span
          key={i}
          className={cn(
            "min-w-[2px] flex-1 rounded-[1px]",
            i < done ? "bg-accent" : i === done && phase !== "prune" ? "stripes animate-stripe bg-accent/70" : "bg-line/80",
          )}
        />
      ))}
    </div>
  );
}

function OpRow({ op }: { op: RunningOp }) {
  const now = useNow(1000);
  const [open, setOpen] = useState(false);
  const hasList = (op.databases?.length ?? 0) > 0;
  const elapsed = secondsBetween(op.started_at, now) ?? 0;
  const dbElapsed = secondsBetween(op.db_started_at, now);
  const showDb = op.phase === "dump" || op.phase === "upload";
  const pct = op.db_total > 0 ? Math.round(((op.phase === "prune" ? op.db_total : op.db_index - 1) / op.db_total) * 100) : 0;
  return (
    <div className="px-4 py-3.5">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Link to={`/projects/${encodeURIComponent(op.project)}/history`} className="font-medium hover:text-accent">
          {op.project}
        </Link>
        <span className="font-mono text-xs text-faint">#{op.run_id}</span>
        {op.state === "cancelling"
          ? <Badge tone="warn">Stopping</Badge>
          : <Badge tone="accent">{PHASE_LABEL[op.phase] ?? op.phase}</Badge>}
        {op.attempt > 1 && <Badge tone="warn">attempt {op.attempt}</Badge>}
        <span className="ml-auto font-mono text-sm tnum text-muted" aria-label="Elapsed time">{formatClock(elapsed)}</span>
        <StopRunButton name={op.project} runId={op.run_id} cancelling={op.state === "cancelling"} />
      </div>
      <div
        className="mt-2.5"
        role="progressbar"
        aria-label={`Progress of ${op.project}`}
        aria-valuemin={0}
        aria-valuemax={op.db_total || 100}
        aria-valuenow={op.db_total ? Math.max(0, op.db_index - 1) : undefined}
        aria-valuetext={op.db_total ? `Database ${op.db_index} of ${op.db_total}` : PHASE_LABEL[op.phase]}
      >
        <DbTrack index={op.db_index} total={op.db_total} phase={op.phase} />
      </div>
      <div className="mt-2 flex flex-wrap items-baseline gap-x-2 text-xs text-muted">
        {op.phase === "queued" ? (
          <span>Waiting for a free slot: only a limited number of runs execute at once</span>
        ) : op.phase === "discover" ? (
          <span>Listing databases on the server…</span>
        ) : (
          <>
            {op.db_total > 0 && (
              <span className="tnum">
                Database <span className="font-medium text-fg">{Math.min(op.db_index, op.db_total)}</span> of {op.db_total}
              </span>
            )}
            {showDb && op.database && (
              <>
                <span className="text-faint">—</span>
                <span className="truncate font-mono text-fg">{op.database}</span>
                {dbElapsed != null && <span className="tnum text-faint">{formatClock(dbElapsed)}</span>}
              </>
            )}
            <span className="ml-auto tnum text-faint">{pct}%</span>
          </>
        )}
        {hasList && (
          <button
            type="button"
            onClick={() => setOpen((o) => !o)}
            aria-expanded={open}
            className="inline-flex items-center gap-1 rounded text-muted hover:text-fg"
          >
            <ChevronRight className={cn("h-3.5 w-3.5 transition-transform", open && "rotate-90")} aria-hidden />
            {open ? "Hide databases" : "All databases"}
          </button>
        )}
      </div>
      <CurrentDbProgress op={op} className="mt-2" />
      {open && hasList && <RunDatabases op={op} className="mt-2 max-h-72 overflow-y-auto" />}
    </div>
  );
}

export function RunningPanel({ running }: { running: RunningOp[] }) {
  if (!running.length) return null;
  return (
    <section aria-labelledby="running-h" className="card mb-6 overflow-hidden border-accent/30">
      <div className="flex items-center gap-2 border-b border-line bg-accent-soft/50 px-4 py-2">
        <span className="relative flex h-2 w-2">
          <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-accent opacity-60" />
          <span className="relative inline-flex h-2 w-2 rounded-full bg-accent" />
        </span>
        <h2 id="running-h" className="text-sm font-medium">Running now</h2>
        <span className="text-xs text-muted">{running.length > 1 ? `${running.length} ${plural(running.length, ["run", "runs"])}` : ""}</span>
      </div>
      <div className="divide-y divide-line" aria-live="polite">
        {running.map((op) => <OpRow key={`${op.project}-${op.run_id}`} op={op} />)}
      </div>
    </section>
  );
}
