import { Clock, Loader2, Play } from "lucide-react";
import { ApiError } from "@/api/client";
import { useRunNow } from "@/api/queries";
import type { ProjectState } from "@/api/types";
import { Button } from "@/components/ui/Button";
import { Tooltip } from "@/components/ui/Tooltip";
import { useToast } from "@/components/ui/Toast";

export function RunNowButton({ name, state, uriSet, size = "sm", variant = "secondary" }: { name: string; state: ProjectState; uriSet: boolean; size?: "sm" | "md"; variant?: "secondary" | "primary" }) {
  const run = useRunNow();
  const { toast, error } = useToast();
  const busy = state !== "idle";
  const pending = run.isPending && run.variables === name;
  const label = state === "cancelling" ? "Stopping" : state === "running" ? "Running" : state === "queued" ? "Queued" : "Run now";
  const disabledReason = !uriSet ? "Set the connection string first" : busy ? "A run is already in progress or queued" : null;

  const btn = (
    <Button
      size={size}
      variant={busy ? "secondary" : variant}
      disabled={busy || !uriSet}
      loading={pending}
      icon={pending ? undefined : state === "running" || state === "cancelling" ? <Loader2 className="h-3.5 w-3.5 animate-spin text-accent" aria-hidden /> : state === "queued" ? <Clock className="h-3.5 w-3.5" aria-hidden /> : <Play className="h-3.5 w-3.5" aria-hidden />}
      aria-label={`Run backup of ${name} now`}
      onClick={(e) => {
        e.stopPropagation();
        run.mutate(name, {
          onSuccess: (r) => toast({ kind: "success", title: `Run #${r.run_id} queued`, detail: name }),
          onError: (e) => {
            if (e instanceof ApiError && e.status === 409) toast({ kind: "info", title: "Already running", detail: `${name}: a run is in progress or queued` });
            else error(e, "Could not start the run");
          },
        });
      }}
    >
      {label}
    </Button>
  );
  return disabledReason ? <Tooltip content={disabledReason}><span tabIndex={0} className="inline-flex rounded">{btn}</span></Tooltip> : btn;
}
