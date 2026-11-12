import { RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";
import { Button } from "@/components/ui/Button";

const CHECK_MS = 60_000;

/** The entry script of a page: Vite names it by content hash, so a new build has a new name. */
function entryOf(html: string): string | null {
  return /<script[^>]+type="module"[^>]+src="([^"]+)"/.exec(html)?.[1] ?? null;
}

const current = () => document.querySelector<HTMLScriptElement>('script[type="module"][src]')?.getAttribute("src") ?? null;

/** After a deploy the open tab still runs the old UI: notice the new build and offer a reload. */
export function UpdateBanner() {
  const [stale, setStale] = useState(false);

  useEffect(() => {
    if (!import.meta.env.PROD) return;
    const mine = current();
    if (!mine) return;
    let stopped = false;
    const check = async () => {
      if (stopped || document.hidden) return;
      try {
        const r = await fetch("/", { cache: "no-store", headers: { Accept: "text/html" } });
        if (!r.ok) return;
        const next = entryOf(await r.text());
        if (next && next !== mine) { setStale(true); stopped = true; }
      } catch { /* offline or restarting: try again later */ }
    };
    const t = window.setInterval(check, CHECK_MS);
    const onVisible = () => !document.hidden && void check();
    document.addEventListener("visibilitychange", onVisible);
    window.addEventListener("focus", onVisible);
    return () => {
      stopped = true;
      window.clearInterval(t);
      document.removeEventListener("visibilitychange", onVisible);
      window.removeEventListener("focus", onVisible);
    };
  }, []);

  if (!stale) return null;
  return (
    <div role="status" className="fixed inset-x-0 bottom-4 z-50 flex justify-center px-4">
      <div className="flex w-full max-w-md items-center gap-3 rounded-lg border border-line bg-surface px-4 py-3 text-sm shadow-pop animate-pop-in">
        <RefreshCw className="h-4 w-4 shrink-0 text-accent" aria-hidden />
        <span className="min-w-0 flex-1">
          <span className="block font-medium">Interface updated</span>
          <span className="block text-xs text-muted">Reload the page to get the new version.</span>
        </span>
        <Button variant="primary" size="sm" onClick={() => window.location.reload()}>Reload</Button>
      </div>
    </div>
  );
}
