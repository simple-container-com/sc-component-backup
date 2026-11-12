import * as T from "@radix-ui/react-toast";
import { AlertTriangle, CheckCircle2, Info, X, XCircle } from "lucide-react";
import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { cn } from "@/lib/cn";
import { describeError } from "@/api/client";

type Kind = "success" | "error" | "info" | "warning";
interface ToastItem { id: number; kind: Kind; title: string; detail?: string }

interface Ctx {
  toast: (t: Omit<ToastItem, "id">) => void;
  success: (title: string, detail?: string) => void;
  error: (e: unknown, fallbackTitle?: string) => void;
}

const ToastCtx = createContext<Ctx | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const toast = useCallback((t: Omit<ToastItem, "id">) => {
    setItems((xs) => [...xs.slice(-3), { ...t, id: Date.now() + Math.random() }]);
  }, []);
  const value = useMemo<Ctx>(
    () => ({
      toast,
      success: (title, detail) => toast({ kind: "success", title, detail }),
      error: (e, fallbackTitle) => {
        const d = describeError(e);
        toast({ kind: "error", title: fallbackTitle ?? d.title, detail: fallbackTitle ? [d.title, d.detail].filter(Boolean).join(": ") : d.detail });
      },
    }),
    [toast],
  );
  const icon = { success: <CheckCircle2 className="h-4 w-4 text-ok" />, error: <XCircle className="h-4 w-4 text-danger" />, info: <Info className="h-4 w-4 text-accent" />, warning: <AlertTriangle className="h-4 w-4 text-warn" /> };
  return (
    <ToastCtx.Provider value={value}>
      <T.Provider swipeDirection="right" duration={5000} label="Notification">
        {children}
        {items.map((t) => (
          <T.Root
            key={t.id}
            duration={t.kind === "error" ? 8000 : 4500}
            onOpenChange={(o) => { if (!o) setItems((xs) => xs.filter((x) => x.id !== t.id)); }}
            className={cn(
              "group flex w-full items-start gap-3 rounded-md border border-line bg-surface p-3 pr-2 shadow-pop",
              "data-[state=open]:animate-pop-in data-[state=closed]:animate-fade-in data-[swipe=move]:translate-x-[var(--radix-toast-swipe-move-x)] data-[swipe=end]:hidden",
            )}
          >
            <span className="mt-0.5">{icon[t.kind]}</span>
            <div className="min-w-0 flex-1">
              <T.Title className="text-sm font-medium">{t.title}</T.Title>
              {t.detail && <T.Description className="mt-0.5 break-words text-xs text-muted">{t.detail}</T.Description>}
            </div>
            <T.Close className="rounded p-1 text-faint hover:bg-sunken hover:text-fg" aria-label="Close notification">
              <X className="h-3.5 w-3.5" />
            </T.Close>
          </T.Root>
        ))}
        <T.Viewport className="fixed bottom-0 right-0 z-[60] flex w-full max-w-sm flex-col gap-2 p-4 outline-none" />
      </T.Provider>
    </ToastCtx.Provider>
  );
}

export function useToast() {
  const c = useContext(ToastCtx);
  if (!c) throw new Error("useToast outside ToastProvider");
  return c;
}
