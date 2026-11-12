import { Tooltip } from "./Tooltip";
import { formatDateTime, formatExact, formatRelative } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { cn } from "@/lib/cn";

/** Relative time with the exact local timestamp in a tooltip. */
export function RelTime({ iso, className, mode = "relative" }: { iso: string | null | undefined; className?: string; mode?: "relative" | "absolute" }) {
  const now = useNow(15_000);
  if (!iso) return <span className={cn("text-faint", className)}>—</span>;
  return (
    <Tooltip content={formatExact(iso)}>
      <time dateTime={iso} tabIndex={0} className={cn("tnum cursor-default rounded-sm", className)}>
        {mode === "relative" ? formatRelative(iso, now) : formatDateTime(iso)}
      </time>
    </Tooltip>
  );
}
