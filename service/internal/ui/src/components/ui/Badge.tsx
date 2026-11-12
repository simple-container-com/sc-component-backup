import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

export type Tone = "neutral" | "accent" | "ok" | "warn" | "danger" | "idle";

const tones: Record<Tone, string> = {
  neutral: "bg-sunken text-muted border-line",
  accent: "bg-accent-soft text-accent border-accent/20",
  ok: "bg-ok-soft text-ok border-ok/20",
  warn: "bg-warn-soft text-warn border-warn/25",
  danger: "bg-danger-soft text-danger border-danger/20",
  idle: "bg-idle-soft text-idle border-idle/20",
};

export function Badge({ tone = "neutral", children, className, icon }: { tone?: Tone; children: ReactNode; className?: string; icon?: ReactNode }) {
  return (
    <span className={cn("inline-flex h-5 items-center gap-1 whitespace-nowrap rounded border px-1.5 text-2xs font-medium", tones[tone], className)}>
      {icon}
      {children}
    </span>
  );
}

export function Dot({ tone, pulse, className }: { tone: Tone; pulse?: boolean; className?: string }) {
  const bg: Record<Tone, string> = {
    neutral: "bg-faint", accent: "bg-accent", ok: "bg-ok", warn: "bg-warn", danger: "bg-danger", idle: "bg-idle",
  };
  return <span aria-hidden className={cn("inline-block h-2 w-2 shrink-0 rounded-full", bg[tone], pulse && "animate-pulse2", className)} />;
}
