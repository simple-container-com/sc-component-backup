import { AlertTriangle, Check, CircleDashed, Clock, Loader2, OctagonX, PauseCircle, TimerOff } from "lucide-react";
import type { ProjectState, RunStatus } from "@/api/types";
import { Badge, type Tone } from "./ui/Badge";

export type AnyStatus = RunStatus | "queued" | "never" | "overdue";

export const STATUS: Record<AnyStatus, { label: string; tone: Tone; hint: string }> = {
  ok: { label: "OK", tone: "ok", hint: "All databases saved" },
  partial: { label: "Partial", tone: "warn", hint: "Some databases were not saved" },
  failed: { label: "Failed", tone: "danger", hint: "No database was saved" },
  interrupted: { label: "Interrupted", tone: "idle", hint: "The run was stopped: via the Stop button, by deleting the project or by stopping the service" },
  running: { label: "Running", tone: "accent", hint: "The run is in progress right now" },
  queued: { label: "Queued", tone: "neutral", hint: "Waiting for a free slot" },
  never: { label: "Never run", tone: "neutral", hint: "No archives yet" },
  overdue: { label: "Overdue", tone: "danger", hint: "No successful backup for longer than the schedule allows" },
};

function Icon({ s }: { s: AnyStatus }) {
  const c = "h-3 w-3";
  switch (s) {
    case "ok": return <Check className={c} strokeWidth={2.5} aria-hidden />;
    case "partial": return <AlertTriangle className={c} aria-hidden />;
    case "failed": return <OctagonX className={c} aria-hidden />;
    case "interrupted": return <PauseCircle className={c} aria-hidden />;
    case "running": return <Loader2 className={`${c} animate-spin`} aria-hidden />;
    case "queued": return <Clock className={c} aria-hidden />;
    case "never": return <CircleDashed className={c} aria-hidden />;
    case "overdue": return <TimerOff className={c} aria-hidden />;
  }
}

export function StatusBadge({ status, className }: { status: AnyStatus; className?: string }) {
  const s = STATUS[status] ?? STATUS.never;
  return (
    <Badge tone={s.tone} icon={<Icon s={status} />} className={className}>
      {s.label}
    </Badge>
  );
}

/** Effective status for a project: live state wins, then "overdue" (no success within the allowed window), then the last run. */
export function projectStatus(state: ProjectState, last: RunStatus | null | undefined, overdueSince?: string | null): AnyStatus {
  if (state === "running" || state === "cancelling") return "running";
  if (state === "queued") return "queued";
  if (overdueSince) return "overdue";
  return last ?? "never";
}

/** Shorthand for a project summary. */
export const statusOf = (p: { state: ProjectState; last_run: { status: RunStatus } | null; overdue_since?: string | null }) =>
  projectStatus(p.state, p.last_run?.status, p.overdue_since);

export type ProjectGroup = "problem" | "active" | "ok";

/**
 * Overview grouping. A problem: overdue (even while a retry runs), or the last run failed / partial / interrupted.
 * rank orders the list: overdue & failed, then partial & interrupted, then running / queued, then the rest.
 */
export function projectGroup(p: { state: ProjectState; last_run: { status: RunStatus } | null; overdue_since?: string | null }): { group: ProjectGroup; rank: number } {
  const last = p.last_run?.status;
  const live = p.state !== "idle";
  if (p.overdue_since || last === "failed") return { group: "problem", rank: 0 };
  if (last === "partial" || last === "interrupted") return { group: "problem", rank: 1 };
  if (live) return { group: "active", rank: 2 };
  return { group: "ok", rank: 3 };
}
