import * as D from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

export function Dialog({
  open, onOpenChange, title, description, children, footer, className,
}: {
  open: boolean; onOpenChange: (o: boolean) => void; title: ReactNode; description?: ReactNode;
  children?: ReactNode; footer?: ReactNode; className?: string;
}) {
  return (
    <D.Root open={open} onOpenChange={onOpenChange}>
      <D.Portal>
        <D.Overlay className="fixed inset-0 z-40 animate-fade-in bg-[rgb(10_14_20/0.45)] backdrop-blur-[1px]" />
        <D.Content
          className={cn(
            "fixed left-1/2 top-[12vh] z-50 w-[calc(100vw-32px)] max-w-md -translate-x-1/2 animate-pop-in rounded-lg border border-line bg-surface shadow-pop focus:outline-none",
            className,
          )}
        >
          <div className="flex items-start justify-between gap-4 px-5 pt-5">
            <div className="min-w-0">
              <D.Title className="text-lg font-semibold">{title}</D.Title>
              {description ? <D.Description className="mt-1 text-sm text-muted">{description}</D.Description> : <D.Description className="sr-only">{typeof title === "string" ? title : ""}</D.Description>}
            </div>
            <D.Close className="-mr-1.5 -mt-1 rounded p-1.5 text-faint hover:bg-sunken hover:text-fg" aria-label="Close">
              <X className="h-4 w-4" />
            </D.Close>
          </div>
          {children && <div className="px-5 pt-4">{children}</div>}
          {footer && <div className="mt-5 flex flex-wrap justify-end gap-2 border-t border-line bg-sunken/50 px-5 py-3 rounded-b-lg">{footer}</div>}
          {!footer && <div className="h-5" />}
        </D.Content>
      </D.Portal>
    </D.Root>
  );
}
