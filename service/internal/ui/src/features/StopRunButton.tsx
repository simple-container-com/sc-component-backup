import { Square } from "lucide-react";
import { useState } from "react";
import { ApiError } from "@/api/client";
import { useCancelRun } from "@/api/queries";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { useToast } from "@/components/ui/Toast";

/** Stops the project's current run after a confirmation. Finished databases stay in storage. */
export function StopRunButton({ name, runId, cancelling, size = "sm" }: { name: string; runId?: number; cancelling: boolean; size?: "sm" | "md" }) {
  const [open, setOpen] = useState(false);
  const cancel = useCancelRun();
  const { toast, error } = useToast();
  const confirm = () =>
    cancel.mutate(name, {
      onSuccess: (r) => { setOpen(false); toast({ kind: "info", title: `Run #${r.run_id} is stopping`, detail: name }); },
      onError: (e) => {
        setOpen(false);
        if (e instanceof ApiError && e.status === 409) toast({ kind: "info", title: "The run has already finished", detail: name });
        else error(e, "Could not stop the run");
      },
    });
  return (
    <>
      <Button
        size={size}
        variant="danger-ghost"
        disabled={cancelling}
        icon={<Square className="h-3 w-3 fill-current" aria-hidden />}
        aria-label={`Stop the run of ${name}`}
        onClick={(e) => { e.stopPropagation(); setOpen(true); }}
      >
        {cancelling ? "Stopping" : "Stop"}
      </Button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={`Stop run${runId ? ` #${runId}` : ""}?`}
        description={<>The dump currently in progress in <span className="font-mono text-fg">{name}</span> will be aborted and deleted. Databases already saved in this run stay in storage. The next run follows the schedule.</>}
        footer={
          <>
            <Button variant="ghost" onClick={() => setOpen(false)}>Keep running</Button>
            <Button variant="danger" loading={cancel.isPending} onClick={confirm}>Stop</Button>
          </>
        }
      />
    </>
  );
}
