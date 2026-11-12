import type { EngineId } from "@/api/types";
import { cn } from "@/lib/cn";

const ENGINE: Record<EngineId, { label: string; mono: string; cls: string }> = {
  mongodb: { label: "MongoDB", mono: "Mg", cls: "bg-[#E3F4E7] text-[#13703A] dark:bg-[#15301F] dark:text-[#6FD394]" },
  postgres: { label: "PostgreSQL", mono: "Pg", cls: "bg-[#E4ECF6] text-[#2C5A8C] dark:bg-[#18263A] dark:text-[#86B3E6]" },
  mariadb: { label: "MariaDB", mono: "Ma", cls: "bg-[#F4ECE2] text-[#8A5A2B] dark:bg-[#33261A] dark:text-[#DDB07F]" },
  s3import: { label: "Import from S3", mono: "S3", cls: "bg-sunken text-muted ring-1 ring-inset ring-line" },
};

export function engineLabel(e: EngineId | string) {
  return ENGINE[e as EngineId]?.label ?? e;
}

/** Monogram tile — consistent stand-in for vendor logos (no external assets). */
export function EngineMark({ engine, size = "md", className }: { engine: EngineId; size?: "sm" | "md" | "lg"; className?: string }) {
  const e = ENGINE[engine] ?? { mono: "?", cls: "bg-sunken text-muted", label: engine };
  const sz = size === "sm" ? "h-5 w-5 text-[10px] rounded-[4px]" : size === "lg" ? "h-10 w-10 text-sm rounded-md" : "h-7 w-7 text-xs rounded";
  return (
    <span aria-hidden className={cn("inline-flex shrink-0 items-center justify-center font-mono font-semibold tracking-tight", sz, e.cls, className)}>
      {e.mono}
    </span>
  );
}

export function EngineBadge({ engine }: { engine: EngineId }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-muted">
      <EngineMark engine={engine} size="sm" />
      {engineLabel(engine)}
    </span>
  );
}
