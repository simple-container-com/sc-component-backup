import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

export function Empty({ icon, title, children, action, className }: { icon?: ReactNode; title: string; children?: ReactNode; action?: ReactNode; className?: string }) {
  return (
    <div className={cn("flex flex-col items-center px-6 py-12 text-center", className)}>
      {icon && <div className="mb-3 flex h-10 w-10 items-center justify-center rounded-md border border-line bg-sunken text-muted">{icon}</div>}
      <h3 className="text-base font-medium">{title}</h3>
      {children && <div className="mt-1 max-w-sm text-sm text-muted">{children}</div>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

export function ErrorState({ title, detail, onRetry }: { title: string; detail?: string; onRetry?: () => void }) {
  return (
    <div role="alert" className="flex flex-col items-start gap-2 rounded-md border border-danger/25 bg-danger-soft/60 px-4 py-3">
      <p className="text-sm font-medium text-danger">{title}</p>
      {detail && <p className="font-mono text-xs text-muted">{detail}</p>}
      {onRetry && (
        <button type="button" onClick={onRetry} className="text-xs font-medium text-fg underline underline-offset-2 hover:text-accent">
          Retry
        </button>
      )}
    </div>
  );
}
