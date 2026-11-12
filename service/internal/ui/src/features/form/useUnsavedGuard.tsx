import { useCallback, useEffect, useRef } from "react";
import { useBlocker, type BlockerFunction } from "react-router-dom";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";

/**
 * Blocks in-app navigation and tab close while `when` is true.
 * Call `allow()` right before an intentional navigation (e.g. after saving).
 */
export function useUnsavedGuard(when: boolean) {
  const whenRef = useRef(when);
  whenRef.current = when;
  const blocker = useBlocker(
    useCallback<BlockerFunction>(({ currentLocation, nextLocation }) => whenRef.current && currentLocation.pathname !== nextLocation.pathname, []),
  );
  useEffect(() => {
    if (!when) return;
    const h = (e: BeforeUnloadEvent) => { e.preventDefault(); e.returnValue = ""; };
    window.addEventListener("beforeunload", h);
    return () => window.removeEventListener("beforeunload", h);
  }, [when]);
  const allow = useCallback(() => { whenRef.current = false; }, []);
  const dialog = (
    <Dialog
      open={blocker.state === "blocked"}
      onOpenChange={(o) => { if (!o && blocker.state === "blocked") blocker.reset(); }}
      title="Leave without saving?"
      description="Changes on this page will be lost."
      footer={
        <>
          <Button onClick={() => blocker.reset?.()}>Stay</Button>
          <Button variant="danger" onClick={() => blocker.proceed?.()}>Leave and discard changes</Button>
        </>
      }
    />
  );
  return { allow, dialog };
}
