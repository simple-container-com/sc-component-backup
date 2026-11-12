import { useMemo, useState } from "react";
import type { CollectionStat } from "@/api/types";
import { describeError } from "@/api/client";
import { useCollections, useObjectHistory } from "@/api/queries";
import { CollectionTrend } from "./CollectionTrend";
import { ErrorState } from "@/components/ui/Empty";
import { SkeletonRows } from "@/components/ui/Skeleton";
import { cn } from "@/lib/cn";
import { formatBytes, formatDateTime, formatInt, plural } from "@/lib/format";

const TOP = 15;
type Sort = "size" | "growth";

interface Row {
  c: CollectionStat;
  /** the same collection in the previous backup; undefined = new since then */
  p?: CollectionStat;
  removed?: boolean;
}

const sum = (cols: CollectionStat[], f: (c: CollectionStat) => number) => cols.reduce((s, c) => s + (f(c) || 0), 0);

/** Signed change: "+12,345" / "−1.2 MB"; nothing when unchanged. */
function Delta({ v, bytes, className }: { v: number; bytes?: boolean; className?: string }) {
  if (!v) return <span className={cn("text-faint", className)}>±0</span>;
  const s = bytes ? formatBytes(Math.abs(v)) : formatInt(Math.abs(v));
  return <span className={cn(v > 0 ? "text-accent" : "text-warn", className)}>{v > 0 ? "+" : "−"}{s}</span>;
}

function pct(now: number, before: number) {
  if (!before) return null;
  const p = ((now - before) / before) * 100;
  return Math.abs(p) < 0.05 ? null : `${p > 0 ? "+" : "−"}${Math.abs(p) < 10 ? Math.abs(p).toFixed(1) : Math.round(Math.abs(p))}%`;
}

/** What a MongoDB archive consists of — collections recorded at dump time, largest first — and what changed
 *  since the previous backup of the same database. */
export function Collections({ project, archiveKey, archiveSize }: { project: string; archiveKey: string; archiveSize?: number }) {
  const { data, isLoading, error, refetch } = useCollections(project, archiveKey, true);
  const [all, setAll] = useState(false);
  const [sort, setSort] = useState<Sort>("size");
  const cols = data?.collections;
  const prev = data?.previous ?? null;
  // An inventory cut short by the deadline lists only some collections: "new" / "deleted" can't be told apart then.
  const incomplete = !!(data?.partial || prev?.partial);
  const hist = useObjectHistory(project, data?.database, !!cols);

  const rows = useMemo<Row[]>(() => {
    if (!cols) return [];
    const before = new Map((prev?.collections ?? []).map((c) => [c.name, c]));
    const now = new Set(cols.map((c) => c.name));
    const list: Row[] = cols.map((c) => ({ c, p: before.get(c.name) }));
    if (prev && !incomplete) for (const p of prev.collections) if (!now.has(p.name)) list.push({ c: { ...p, docs: 0, bytes: 0, storage_bytes: 0, index_bytes: 0 }, p, removed: true });
    if (sort === "growth" && prev) {
      const g = (r: Row) => Math.abs(r.c.bytes - (r.p?.bytes ?? 0));
      list.sort((a, b) => g(b) - g(a) || Math.abs(b.c.docs - (b.p?.docs ?? 0)) - Math.abs(a.c.docs - (a.p?.docs ?? 0)));
    }
    return list;
  }, [cols, prev, sort, incomplete]);

  if (isLoading) return <SkeletonRows rows={3} />;
  if (error) return <ErrorState {...describeError(error)} onRetry={() => void refetch()} />;
  if (!cols) {
    return (
      <p className="text-xs text-muted">
        No contents recorded for this archive: collection statistics are saved with every new dump starting from this update.
      </p>
    );
  }

  const total = sum(cols, (c) => c.bytes);
  const disk = sum(cols, (c) => c.storage_bytes);
  const idx = sum(cols, (c) => c.index_bytes);
  const docs = sum(cols, (c) => c.docs);
  const added = incomplete ? 0 : rows.filter((r) => !r.p && !r.removed).length;
  const removed = rows.filter((r) => r.removed).length;
  const shown = all ? rows : rows.slice(0, TOP);
  const grid = prev
    ? "grid-cols-[minmax(0,1fr)_84px_76px] sm:grid-cols-[minmax(0,1fr)_96px_88px_76px_76px]"
    : "grid-cols-[minmax(0,1fr)_72px_72px] sm:grid-cols-[minmax(0,1fr)_90px_80px_80px_80px]";

  return (
    <div>
      <p className="mb-1 text-xs text-muted tnum">
        {cols.length} {plural(cols.length, ["collection", "collections"])} · {formatInt(docs)} {plural(docs, ["document", "documents"])} ·
        data {formatBytes(total)} uncompressed · on server disk {formatBytes(disk)} · indexes {formatBytes(idx)}
      </p>
      {prev ? (
        <ChangeSummary
          since={prev.ts}
          items={[
            { label: "documents", v: docs - sum(prev.collections, (c) => c.docs), p: pct(docs, sum(prev.collections, (c) => c.docs)) },
            { label: "data", v: total - sum(prev.collections, (c) => c.bytes), p: pct(total, sum(prev.collections, (c) => c.bytes)), bytes: true },
            { label: "on disk", v: disk - sum(prev.collections, (c) => c.storage_bytes), p: pct(disk, sum(prev.collections, (c) => c.storage_bytes)), bytes: true },
            { label: "indexes", v: idx - sum(prev.collections, (c) => c.index_bytes), p: pct(idx, sum(prev.collections, (c) => c.index_bytes)), bytes: true },
            ...(archiveSize != null && prev.bytes
              ? [{ label: "archive", v: archiveSize - prev.bytes, p: pct(archiveSize, prev.bytes), bytes: true }]
              : []),
          ]}
          added={added}
          removed={removed}
        />
      ) : null}
      {prev && incomplete ? (
        <p className="mb-2 text-xs text-warn">
          The contents of {data?.partial ? "this" : "the previous"} archive were not fully recorded (ran out of time): totals are understated, and new or deleted collections cannot be determined.
        </p>
      ) : prev ? null : (
        <p className="mb-2 text-xs text-faint">
          Changes will appear from the next backup: earlier archives of this database have no recorded contents.
        </p>
      )}
      {prev && (
        <div className="mb-1.5 flex gap-1 text-xs" role="group" aria-label="Collection order">
          {(["size", "growth"] as const).map((s) => (
            <button
              key={s}
              type="button"
              aria-pressed={sort === s}
              onClick={() => setSort(s)}
              className={cn("rounded-md border px-2 py-0.5", sort === s ? "border-line bg-sunken text-fg" : "border-transparent text-muted hover:text-fg")}
            >
              {s === "size" ? "Largest" : "Most changed"}
            </button>
          ))}
        </div>
      )}
      <div role="table" aria-label="Collections" className="text-xs">
        <div role="row" className={cn("grid gap-x-3 pb-1 text-faint", grid)}>
          <span role="columnheader">Collection</span>
          <span role="columnheader" className="text-right">Documents</span>
          <span role="columnheader" className="text-right">Data</span>
          <span role="columnheader" className="hidden text-right sm:block">On disk</span>
          <span role="columnheader" className="hidden text-right sm:block">Indexes</span>
        </div>
        {shown.map(({ c, p, removed: gone }) => {
          const share = total ? c.bytes / total : 0;
          return (
            <div role="row" key={c.name + (gone ? "-removed" : "")} className={cn("relative grid items-center gap-x-3 py-1", grid)}>
              <span role="cell" className="relative min-w-0">
                {!gone && <span className="absolute inset-y-[-2px] left-0 rounded-sm bg-accent/15" style={{ width: `${Math.max(share * 100, 0.5)}%` }} aria-hidden />}
                <span className={cn("relative flex min-w-0 items-center gap-1.5 px-1", gone && "text-faint")}>
                  <CollectionTrend name={c.name} history={hist.data} loading={hist.isLoading}>
                    <span className={cn("block cursor-default truncate rounded font-mono text-[12px] underline decoration-line decoration-dotted underline-offset-4 hover:decoration-muted", gone && "line-through")}>{c.name}</span>
                  </CollectionTrend>
                  {prev && !incomplete && !p && !gone && <span className="shrink-0 rounded bg-accent/15 px-1 text-[10px] text-accent">new</span>}
                  {gone && <span className="shrink-0 rounded bg-warn/15 px-1 text-[10px] text-warn">deleted</span>}
                </span>
              </span>
              <Cell value={gone ? "—" : formatInt(c.docs)} delta={prev ? c.docs - (p?.docs ?? 0) : null} />
              <Cell
                value={gone ? "—" : formatBytes(c.bytes)}
                extra={!gone && share >= 0.05 ? `${Math.round(share * 100)}%` : undefined}
                strong={share >= 0.25}
                delta={prev ? c.bytes - (p?.bytes ?? 0) : null}
                bytes
              />
              <Cell value={gone ? "—" : formatBytes(c.storage_bytes)} delta={prev ? c.storage_bytes - (p?.storage_bytes ?? 0) : null} bytes desktop />
              <Cell value={gone ? "—" : formatBytes(c.index_bytes)} delta={prev ? c.index_bytes - (p?.index_bytes ?? 0) : null} bytes desktop />
            </div>
          );
        })}
      </div>
      {rows.length > TOP && (
        <button type="button" onClick={() => setAll((a) => !a)} className="mt-1.5 text-xs text-muted hover:text-fg">
          {all ? "Show fewer" : `Show all ${rows.length}`}
        </button>
      )}
    </div>
  );
}

function Cell({ value, delta, bytes, desktop, extra, strong }: {
  value: string; delta: number | null; bytes?: boolean; desktop?: boolean; extra?: string; strong?: boolean;
}) {
  return (
    <span role="cell" className={cn("text-right tnum leading-tight", desktop && "hidden sm:block")}>
      <span className={cn("block whitespace-nowrap", strong ? "font-medium text-fg" : "text-muted")}>
        {value}
        {extra && <span className="ml-1 font-normal text-faint">{extra}</span>}
      </span>
      {delta != null && <Delta v={delta} bytes={bytes} className="block whitespace-nowrap text-[11px]" />}
    </span>
  );
}

function ChangeSummary({ since, items, added, removed }: {
  since: string; items: { label: string; v: number; p: string | null; bytes?: boolean }[]; added: number; removed: number;
}) {
  return (
    <div className="mb-2 rounded-md bg-sunken px-2 py-1.5 text-xs tnum">
      <span className="text-faint">Since the previous backup ({formatDateTime(since)}): </span>
      <span className="inline-flex flex-wrap gap-x-3 gap-y-0.5">
        {items.map((it) => (
          <span key={it.label} className="whitespace-nowrap">
            <span className="text-muted">{it.label} </span>
            <Delta v={it.v} bytes={it.bytes} />
            {it.p && <span className="ml-1 text-faint">{it.p}</span>}
          </span>
        ))}
        {(added > 0 || removed > 0) && (
          <span className="whitespace-nowrap text-muted">
            collections {added > 0 && <span className="text-accent">+{added} {plural(added, ["new", "new"])}</span>}
            {added > 0 && removed > 0 && " · "}
            {removed > 0 && <span className="text-warn">−{removed} {plural(removed, ["deleted", "deleted"])}</span>}
          </span>
        )}
      </span>
    </div>
  );
}
