import type { ProjectSummary } from "@/api/types";
import { toDate } from "@/lib/format";
import { useNow } from "@/lib/hooks";
import { RelTime } from "./ui/Time";

/** When the next scheduled run happens: "in 23 h", "now", "overdue · 5 min ago". */
export function NextDue({ p }: { p: Pick<ProjectSummary, "state" | "next_due"> }) {
  const now = useNow(15_000);
  if (p.state === "cancelling") return <span className="text-muted">stopping</span>;
  if (p.state === "running") return <span className="text-accent">running now</span>;
  if (p.state === "queued") return <span className="text-muted">queued</span>;
  const d = toDate(p.next_due);
  if (!d) return <span className="text-faint">—</span>;
  const diff = d.getTime() - now;
  if (diff < -120_000) {
    return (
      <span className="text-warn">
        overdue · <RelTime iso={p.next_due} />
      </span>
    );
  }
  if (diff < 45_000) return <span>now</span>;
  return <RelTime iso={p.next_due} />;
}
