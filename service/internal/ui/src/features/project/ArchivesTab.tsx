import { ArrowDown, ArrowUp, ChevronRight, Download, Layers, FileArchive, KeyRound, Search, Terminal } from "lucide-react";
import { useMemo, useState } from "react";
import { describeError, downloadUrl } from "@/api/client";
import { useArchives } from "@/api/queries";
import type { Archive, ArchivesResponse, EngineId, Retention } from "@/api/types";
import { Badge } from "@/components/ui/Badge";
import { buttonClass } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";
import { Empty, ErrorState } from "@/components/ui/Empty";
import { SkeletonRows } from "@/components/ui/Skeleton";
import { RelTime } from "@/components/ui/Time";
import { Tooltip } from "@/components/ui/Tooltip";
import { cn } from "@/lib/cn";
import { Collections } from "./Collections";
import { formatBytes, formatDateTime, formatExact, KEPT_BY_LABEL, keptByExplanation, plural } from "@/lib/format";

type Restore = ArchivesResponse["restore"];
type SortKey = "name" | "created" | "count" | "size";
type Sort = { key: SortKey; desc: boolean };
const SORT_STORAGE = "archives-sort";

function loadSort(): Sort {
  try {
    const v = JSON.parse(localStorage.getItem(SORT_STORAGE) ?? "null") as Sort | null;
    if (v && ["name", "created", "count", "size"].includes(v.key)) return { key: v.key, desc: !!v.desc };
  } catch { /* storage unavailable */ }
  return { key: "name", desc: false };
}

/** Column header that sorts: first click on a number column sorts largest/newest first. */
function SortHeader({ k, sort, onSort, children, className }: { k: SortKey; sort: Sort; onSort: (k: SortKey) => void; children: React.ReactNode; className?: string }) {
  const on = sort.key === k;
  const Arrow = sort.desc ? ArrowDown : ArrowUp;
  return (
    <span role="columnheader" aria-sort={on ? (sort.desc ? "descending" : "ascending") : "none"} className={className}>
      <button type="button" onClick={() => onSort(k)} className={cn("inline-flex items-center gap-1 rounded hover:text-fg", on && "text-fg")}>
        {children}
        {on ? <Arrow className="h-3 w-3" aria-hidden /> : <ArrowDown className="h-3 w-3 opacity-0" aria-hidden />}
      </button>
    </span>
  );
}

/** The command for one archive: a per-suffix variant (s3import: .sql.gz vs .tar.gz) or the common one. */
function restoreFor(restore: Restore, key: string) {
  const bySuffix = Object.entries(restore.by_suffix ?? {}).find(([suffix]) => key.endsWith(suffix));
  return (bySuffix?.[1] ?? restore.command).split("{key}").join(key);
}

function Template({ text }: { text: string }) {
  return (
    <>
      {text.split("{key}").map((part, i, arr) => (
        <span key={i}>{part}{i < arr.length - 1 && <span className="rounded-sm bg-accent-soft px-0.5 text-accent">{"{key}"}</span>}</span>
      ))}
    </>
  );
}

function KeptBy({ rules, retention }: { rules: Archive["kept_by"]; retention?: Retention }) {
  if (!rules.length) return <span className="text-xs text-faint">will be deleted</span>;
  return (
    <span className="flex flex-wrap gap-1">
      {rules.map((r) => (
        <Tooltip key={r} content={keptByExplanation(r, retention)}>
          <span tabIndex={0} className="rounded">
            <Badge tone={r === "last" ? "accent" : "neutral"}>{KEPT_BY_LABEL[r] ?? r}</Badge>
          </span>
        </Tooltip>
      ))}
    </span>
  );
}

/**
 * One grid for the header, the database rows and the archive rows inside them, so every
 * nested cell sits exactly under its column: chevron · name/date · newest/age · count · size · actions.
 * Below `sm` the table collapses to chevron · name/date · size(+actions).
 */
const GRID = "grid grid-cols-[16px_minmax(0,1fr)_auto] gap-x-3 sm:grid-cols-[16px_minmax(0,1fr)_120px_96px_88px_92px]";

function SizeDelta({ size, prev }: { size: number; prev?: Archive }) {
  const delta = prev && prev.size > 0 ? (size - prev.size) / prev.size : null;
  return (
    <span className="whitespace-nowrap tnum">
      {delta != null && Math.abs(delta) >= 0.1 && (
        <Tooltip content="Compared with the previous archive">
          <span tabIndex={0} className={cn("mr-1.5 rounded text-xs", delta < 0 ? "text-warn" : "text-faint")}>
            {delta > 0 ? "+" : "−"}{Math.round(Math.abs(delta) * 100)}%
          </span>
        </Tooltip>
      )}
      {formatBytes(size)}
    </span>
  );
}

function ArchiveActions({ a, restore, label, inventory }: { a: Archive; restore: Restore; label: string; inventory?: { open: boolean; toggle: () => void } }) {
  const name = a.key.split("/").pop() ?? a.key;
  return (
    <span className="flex justify-end gap-0.5">
      {inventory && (
        <Tooltip content={inventory.open ? "Hide collections" : "Collections: documents and size"}>
          <button
            type="button"
            onClick={inventory.toggle}
            aria-expanded={inventory.open}
            aria-label={`Collections of the ${label} archive from ${formatDateTime(a.ts)}`}
            className={cn(buttonClass("ghost", "icon-sm"), inventory.open && "text-accent")}
          >
            <Layers className="h-3.5 w-3.5" />
          </button>
        </Tooltip>
      )}
      <CopyButton text={restoreFor(restore, a.key)} label={`Copy the restore command for ${label}, ${formatDateTime(a.ts)}`} tooltip="Copy the restore command" />
      <Tooltip content={`Download ${name}`}>
        <a href={downloadUrl(a.key)} download={name} className={buttonClass("ghost", "icon-sm")} aria-label={`Download the ${label} archive from ${formatDateTime(a.ts)}`}>
          <Download className="h-3.5 w-3.5" />
        </a>
      </Tooltip>
    </span>
  );
}

function ArchiveRows({ archives, restore, retention, label, project }: { archives: Archive[]; restore: Restore; retention?: Retention; label: string; project?: string }) {
  const [inv, setInv] = useState<Set<string>>(new Set());
  const toggle = (k: string) => setInv((s) => { const n = new Set(s); if (n.has(k)) n.delete(k); else n.add(k); return n; });
  return (
    <ul className="divide-y divide-line/60">
      {archives.map((a, i) => (
        <li key={a.key}>
        <div className={cn(GRID, "items-center px-3 py-1.5 text-sm")}>
          <span aria-hidden />
          <div className="row-span-2 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 py-0.5 sm:row-span-1">
            <span className="whitespace-nowrap sm:min-w-[7.5rem]">
              <Tooltip content={formatExact(a.ts)}>
                <span tabIndex={0} className="tnum rounded">{formatDateTime(a.ts)}</span>
              </Tooltip>
              <span className="ml-2 text-xs text-faint sm:hidden"><RelTime iso={a.ts} /></span>
            </span>
            <KeptBy rules={a.kept_by} retention={retention} />
          </div>
          <span className="hidden text-muted sm:block"><RelTime iso={a.ts} /></span>
          <span className="hidden sm:block" aria-hidden />
          {/* phone: size above the actions in the last column; desktop: their own columns */}
          <span className="col-start-3 row-start-1 text-right sm:col-start-5 sm:row-start-auto"><SizeDelta size={a.size} prev={archives[i + 1]} /></span>
          <span className="col-start-3 row-start-2 sm:col-start-6 sm:row-start-auto">
            <ArchiveActions a={a} restore={restore} label={label} inventory={project ? { open: inv.has(a.key), toggle: () => toggle(a.key) } : undefined} />
          </span>
        </div>
        {project && inv.has(a.key) && (
          <div className="mb-2 ml-10 mr-3 rounded-md border border-line bg-surface px-3 py-2.5">
            <Collections project={project} archiveKey={a.key} archiveSize={a.size} />
          </div>
        )}
        </li>
      ))}
    </ul>
  );
}

function Group({ title, subtitle, total, archives, restore, retention, open, onToggle, icon, pinned, project }: {
  title: string; subtitle?: string; total: number; archives: Archive[]; restore: Restore; retention?: Retention; project?: string;
  open: boolean; onToggle: () => void; icon?: React.ReactNode; pinned?: boolean;
}) {
  const newest = archives[0];
  const id = `grp-${title.replace(/[^a-zA-Z0-9_-]/g, "_")}`;
  return (
    <li className={cn(pinned && "bg-accent-soft/20")}>
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        aria-controls={id}
        className={cn(GRID, "w-full items-center px-3 py-2.5 text-left hover:bg-sunken/60")}
      >
        <ChevronRight className={cn("h-4 w-4 text-faint transition-transform", open && "rotate-90")} aria-hidden />
        <span className="flex min-w-0 items-center gap-2">
          {icon}
          <span className="truncate font-mono text-[13px] font-medium">{title}</span>
          {subtitle && <span className="hidden truncate text-xs text-muted md:inline">{subtitle}</span>}
          <span className="shrink-0 text-xs text-faint tnum sm:hidden">· {archives.length}</span>
        </span>
        <span className="hidden text-sm text-muted sm:block">{newest ? <RelTime iso={newest.ts} /> : "—"}</span>
        <span className="hidden text-sm text-muted tnum sm:block">{archives.length} {plural(archives.length, ["archive", "archives"])}</span>
        <span className="text-right text-sm font-medium tnum">{formatBytes(total)}</span>
        <span className="hidden sm:block" aria-hidden />
      </button>
      {open && (
        <div id={id} className="border-t border-line/60 bg-sunken/40">
          {archives.length ? <ArchiveRows archives={archives} restore={restore} retention={retention} label={title} project={project} /> : <p className={cn(GRID, "px-3 py-3 text-sm text-muted")}><span /><span>No archives</span></p>}
        </div>
      )}
    </li>
  );
}

export function ArchivesTab({ name, engine, retention, globalsEnabled }: { name: string; engine?: EngineId; retention?: Retention; globalsEnabled?: boolean }) {
  const { data, isLoading, error, refetch } = useArchives(name);
  const [q, setQ] = useState("");
  const [openSet, setOpenSet] = useState<Set<string> | null>(null);

  const dbs = useMemo(() => data?.databases ?? [], [data]);
  const [sort, setSort] = useState<Sort>(loadSort);
  const onSort = (key: SortKey) => {
    const next = { key, desc: sort.key === key ? !sort.desc : key !== "name" };
    setSort(next);
    try { localStorage.setItem(SORT_STORAGE, JSON.stringify(next)); } catch { /* storage unavailable */ }
  };
  const filtered = useMemo(() => {
    const list = q ? dbs.filter((d) => d.database.toLowerCase().includes(q.toLowerCase())) : [...dbs];
    const val = (d: (typeof dbs)[number]): number | string =>
      sort.key === "size" ? d.total_bytes
      : sort.key === "count" ? d.archives.length
      : sort.key === "created" ? (d.archives[0] ? Date.parse(d.archives[0].ts) : 0)
      : d.database;
    list.sort((a, b) => {
      const x = val(a), y = val(b);
      const c = typeof x === "string" ? x.localeCompare(y as string) : x - (y as number);
      return (sort.desc ? -c : c) || a.database.localeCompare(b.database);
    });
    return list;
  }, [dbs, q, sort]);
  const defaultOpen = useMemo(() => new Set(dbs.length <= 3 ? dbs.map((d) => d.database) : []), [dbs]);
  const opened = openSet ?? defaultOpen;
  const toggle = (k: string) => {
    const n = new Set(opened);
    if (n.has(k)) n.delete(k); else n.add(k);
    setOpenSet(n);
  };

  if (isLoading) return <SkeletonRows rows={6} />;
  if (error) return <ErrorState {...describeError(error)} onRetry={() => void refetch()} />;
  if (!data) return null;

  const total = dbs.reduce((s, d) => s + d.total_bytes, 0) + data.globals.reduce((s, a) => s + a.size, 0);
  const count = dbs.reduce((s, d) => s + d.archives.length, 0) + data.globals.length;
  const restore = data.restore;
  const variants = Object.entries(restore.by_suffix ?? {});

  if (dbs.length === 0 && data.globals.length === 0) {
    return (
      <div className="card">
        <Empty icon={<FileArchive className="h-5 w-5" />} title="No archives yet">
          They appear after the first successful run, on schedule or via "Run now".
        </Empty>
      </div>
    );
  }

  const allOpen = filtered.length > 0 && filtered.every((d) => opened.has(d.database));
  const showGlobals = data.globals.length > 0 || (engine === "postgres" && globalsEnabled);

  return (
    <div className="space-y-5">
      <div className="rounded-md border border-line bg-surface px-4 py-3">
        <div className="flex items-center gap-2 text-sm font-medium"><Terminal className="h-4 w-4 text-faint" aria-hidden /> Restore</div>
        <p className="mt-1 text-xs text-muted">
          Each archive has a copy button: the command comes with the key already filled in.{" "}
          {variants.length
            ? <>The command depends on the archive type. Replace <code className="text-fg">&lt;target_db&gt;</code> and <code className="text-fg">&lt;target_dir&gt;</code> with the database and directory you are restoring into.</>
            : <>Replace <code className="text-fg">&lt;target&gt;</code> with the connection string of the server you are restoring into.</>}
        </p>
        {variants.length ? (
          <dl className="mt-2 space-y-1.5">
            {variants.map(([suffix, cmd]) => (
              <div key={suffix}>
                <dt className="font-mono text-xs text-faint">{suffix}</dt>
                <dd className="mt-0.5 whitespace-pre-wrap break-words rounded bg-sunken px-3 py-2 font-mono text-xs text-muted"><Template text={cmd} /></dd>
              </div>
            ))}
          </dl>
        ) : (
          <pre className="mt-2 whitespace-pre-wrap break-words rounded bg-sunken px-3 py-2 font-mono text-xs text-muted"><Template text={restore.command} /></pre>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <p className="mr-auto text-sm text-muted tnum">
          {dbs.length} {plural(dbs.length, ["database", "databases"])}, {count} {plural(count, ["archive", "archives"])}, {formatBytes(total)}
        </p>
        <div className="flex items-center gap-2">
          {dbs.length > 6 && (
            <div className="relative min-w-0 flex-1 sm:flex-none">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-faint" aria-hidden />
              <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Filter by database" aria-label="Filter by database" className="field h-8 w-40 pl-8 text-xs sm:w-48" />
            </div>
          )}
          <button type="button" className={buttonClass("ghost", "sm")} onClick={() => setOpenSet(allOpen ? new Set() : new Set([...opened, ...filtered.map((d) => d.database)]))}>
            {allOpen ? "Collapse all" : "Expand all"}
          </button>
        </div>
      </div>

      <div className="card overflow-hidden">
        <div role="row" className={cn(GRID, "hidden border-b border-line bg-sunken/60 px-3 py-2 text-xs text-muted sm:grid")}>
          <span />
          <SortHeader k="name" sort={sort} onSort={onSort}>Database · archive</SortHeader>
          <SortHeader k="created" sort={sort} onSort={onSort}>Created</SortHeader>
          <SortHeader k="count" sort={sort} onSort={onSort}>Archives</SortHeader>
          <SortHeader k="size" sort={sort} onSort={onSort} className="text-right">Size</SortHeader>
          <span />
        </div>
        <div className="flex items-center gap-1 border-b border-line bg-sunken/60 px-3 py-2 text-xs text-muted sm:hidden">
          <span className="mr-1">Sort:</span>
          <SortHeader k="name" sort={sort} onSort={onSort}>name</SortHeader>
          <SortHeader k="size" sort={sort} onSort={onSort} className="ml-2">size</SortHeader>
          <SortHeader k="created" sort={sort} onSort={onSort} className="ml-2">date</SortHeader>
        </div>
        <ul className="divide-y divide-line">
          {showGlobals && !q && (
            <Group
              title="@globals"
              subtitle="roles, grants, tablespaces"
              icon={<KeyRound className="h-3.5 w-3.5 text-faint" aria-hidden />}
              total={data.globals.reduce((s, a) => s + a.size, 0)}
              archives={data.globals}
              restore={restore}
              retention={retention}
              open={opened.has("@globals")}
              onToggle={() => toggle("@globals")}
              pinned
            />
          )}
          {filtered.map((d) => (
            <Group key={d.database} project={engine === "mongodb" ? name : undefined} title={d.database} total={d.total_bytes} archives={d.archives} restore={restore} retention={retention} open={opened.has(d.database)} onToggle={() => toggle(d.database)} />
          ))}
          {filtered.length === 0 && <li className="px-4 py-6 text-center text-sm text-muted">No databases match "{q}"</li>}
        </ul>
      </div>
    </div>
  );
}
