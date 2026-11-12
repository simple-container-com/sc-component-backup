import * as P from "@radix-ui/react-popover";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import type { ObjectHistory } from "@/api/types";
import { formatDay, formatDayLong, useWidth } from "@/components/charts/Charts";
import { cn } from "@/lib/cn";
import { formatBytes, formatInt } from "@/lib/format";

type Series = (number | null)[];

const H = 64;
const PAD = 5; // vertical only: the line runs edge to edge, aligned with the text above it

function geometry(values: Series, W: number) {
  const nums = values.filter((v): v is number => v != null);
  const min = Math.min(...nums);
  const max = Math.max(...nums);
  // Leave headroom so a flat line sits mid-chart instead of on the floor.
  const lo = max === min ? min * 0.98 : min - (max - min) * 0.08;
  const hi = max === min ? max * 1.02 + 1 : max + (max - min) * 0.08;
  const n = values.length;
  const x = (i: number) => 1 + (i / Math.max(1, n - 1)) * (W - 2);
  const y = (v: number) => PAD + (H - 2 * PAD) * (1 - (v - lo) / (hi - lo || 1));
  return { x, y, min, max };
}

/** Line + soft area over the days with a value; gaps stay gaps. */
function paths(values: Series, g: ReturnType<typeof geometry>) {
  let line = "";
  let area = "";
  let seg: [number, number][] = [];
  const flush = () => {
    if (seg.length) {
      line += seg.map(([px, py], i) => `${i ? "L" : "M"}${px.toFixed(1)},${py.toFixed(1)}`).join("");
      if (seg.length > 1) area += `M${seg[0]![0].toFixed(1)},${H}` + seg.map(([px, py]) => `L${px.toFixed(1)},${py.toFixed(1)}`).join("") + `L${seg.at(-1)![0].toFixed(1)},${H}Z`;
    }
    seg = [];
  };
  values.forEach((v, i) => (v == null ? flush() : seg.push([g.x(i), g.y(v)])));
  flush();
  return { line, area };
}

function change(values: Series) {
  const first = values.findIndex((v) => v != null);
  let last = -1;
  values.forEach((v, i) => { if (v != null) last = i; });
  if (first < 0 || last <= first) return null;
  const a = values[first]!, b = values[last]!;
  return { abs: b - a, pct: a ? ((b - a) / a) * 100 : null, perDay: (b - a) / (last - first), from: first, to: last, current: b };
}

const signed = (v: number, fmt: (n: number) => string) => (v === 0 ? "±0" : `${v > 0 ? "+" : "−"}${fmt(Math.abs(v))}`);
const pctText = (p: number | null) => (p == null || Math.abs(p) < 0.05 ? "" : `${p > 0 ? "+" : "−"}${Math.abs(p) < 10 ? Math.abs(p).toFixed(1) : Math.round(Math.abs(p))}%`);

function MetricChart({ label, values, days, fmt, hover, setHover, tone }: {
  label: string; values: Series; days: string[]; fmt: (n: number) => string;
  hover: number | null; setHover: (i: number | null) => void; tone: "accent" | "muted";
}) {
  const points = values.filter((v) => v != null).length;
  const c = change(hover == null ? values : values.slice(0, hover + 1));
  const color = tone === "accent" ? "rgb(var(--accent))" : "rgb(var(--muted))";
  const shown = hover != null && values[hover] != null ? values[hover]! : c?.current ?? [...values].reverse().find((v) => v != null) ?? null;
  return (
    <div>
      <div className="mb-1.5 flex items-baseline justify-between gap-3 tnum">
        <span className="text-muted">{label}</span>
        <span className="whitespace-nowrap">
          <span className="font-medium text-fg">{shown == null ? "—" : fmt(shown)}</span>
          {c && (hover == null || values[hover] != null) && (
            <span className={cn("ml-2", c.abs > 0 ? "text-accent" : c.abs < 0 ? "text-warn" : "text-faint")}>
              {signed(c.abs, fmt)} <span className="text-faint">{pctText(c.pct)}</span>
            </span>
          )}
        </span>
      </div>
      {points < 2 ? (
        <div className="flex h-10 items-center justify-center rounded border border-dashed border-line text-[11px] text-faint">
          a single point: the chart appears after the next backups
        </div>
      ) : (
        <Chart values={values} days={days} hover={hover} setHover={setHover} color={color} fmt={fmt} />
      )}
    </div>
  );
}

function Chart({ values, days, hover, setHover, color, fmt }: {
  values: Series; days: string[]; hover: number | null; setHover: (i: number | null) => void; color: string; fmt: (n: number) => string;
}) {
  const [ref, W] = useWidth<HTMLDivElement>();
  return <div ref={ref} className="h-16 w-full">{W > 0 && <Plot W={W} values={values} days={days} hover={hover} setHover={setHover} color={color} fmt={fmt} />}</div>;
}

function Plot({ W, values, days, hover, setHover, color, fmt }: {
  W: number; values: Series; days: string[]; hover: number | null; setHover: (i: number | null) => void; color: string; fmt: (n: number) => string;
}) {
  const g = geometry(values, W);
  const { line, area } = paths(values, g);
  const id = `ct${useId().replace(/:/g, "")}`;
  const onMove = (e: React.PointerEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const i = Math.round(((e.clientX - r.left) / r.width) * (values.length - 1));
    setHover(Math.max(0, Math.min(values.length - 1, i)));
  };
  const hv = hover != null ? values[hover] : null;
  return (
    <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} className="block touch-none select-none overflow-visible" onPointerMove={onMove} onPointerLeave={() => setHover(null)} role="img"
      aria-label={`from ${fmt(g.min)} to ${fmt(g.max)}, ${formatDay(days[0]!)} to ${formatDay(days.at(-1)!)}`}>
      <defs>
        <linearGradient id={id} x1="0" x2="0" y1="0" y2="1">
          <stop offset="0" stopColor={color} stopOpacity={0.28} />
          <stop offset="1" stopColor={color} stopOpacity={0.02} />
        </linearGradient>
      </defs>
      <line x1={0} x2={W} y1={H - 0.5} y2={H - 0.5} stroke="rgb(var(--line))" />
      <path d={area} fill={`url(#${id})`} />
      <path d={line} fill="none" stroke={color} strokeWidth={1.75} strokeLinejoin="round" strokeLinecap="round" />
      {values.map((v, i) => (v != null && (values[i - 1] == null && values[i + 1] == null) ? <circle key={i} cx={g.x(i)} cy={g.y(v)} r={1.75} fill={color} /> : null))}
      {hover != null && (
        <g>
          <line x1={g.x(hover)} x2={g.x(hover)} y1={0} y2={H} stroke="rgb(var(--muted))" strokeOpacity={0.6} strokeDasharray="2 2" />
          {hv != null && <circle cx={g.x(hover)} cy={g.y(hv)} r={3.5} fill={color} stroke="rgb(var(--surface))" strokeWidth={1.5} />}
        </g>
      )}
    </svg>
  );
}

/** Narrow screens: open below the name, there is no room beside it. */
function useNarrow() {
  const q = "(max-width: 640px)";
  const [narrow, setNarrow] = useState(() => typeof window !== "undefined" && window.matchMedia(q).matches);
  useEffect(() => {
    const m = window.matchMedia(q);
    const on = () => setNarrow(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);
  return narrow;
}

/** Hover card of one collection / table: the last 30 days of its data size and document count. Opens on hover
 *  (and stays open while the pointer is on the card, to read the chart), on keyboard focus, and on tap. */
export function CollectionTrend({ name, history, loading, children }: {
  name: string; history: ObjectHistory | undefined; loading: boolean; children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const [hover, setHover] = useState<number | null>(null);
  const timer = useRef<number | undefined>(undefined);
  const narrow = useNarrow();
  const later = (v: boolean, ms: number) => {
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => { setOpen(v); if (!v) setHover(null); }, ms);
  };
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const mouse = (e: React.PointerEvent) => e.pointerType === "mouse";
  const o = history?.objects[name];
  const days = history?.days ?? [];
  const data = change(o?.data ?? []);
  return (
    <P.Root open={open} onOpenChange={(v) => { window.clearTimeout(timer.current); setOpen(v); if (!v) setHover(null); }}>
      <P.Trigger asChild>
        <span
          role="button"
          tabIndex={0}
          className="min-w-0 rounded focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent"
          aria-label={`${name}: trend over 30 days`}
          onPointerEnter={(e) => mouse(e) && later(true, 200)}
          onPointerLeave={(e) => mouse(e) && later(false, 150)}
          onFocus={(e) => e.currentTarget.matches(":focus-visible") && later(true, 0)}
          onBlur={() => later(false, 150)}
        >
          {children}
        </span>
      </P.Trigger>
      <P.Portal>
        <P.Content
          side={narrow ? "bottom" : "right"}
          align="start"
          sideOffset={10}
          collisionPadding={12}
          onOpenAutoFocus={(e) => e.preventDefault()}
          onCloseAutoFocus={(e) => e.preventDefault()}
          onPointerEnter={(e) => mouse(e) && window.clearTimeout(timer.current)}
          onPointerLeave={(e) => mouse(e) && later(false, 150)}
          aria-label={`${name}: trend over 30 days`}
          className="z-50 w-[min(320px,calc(100vw-24px))] animate-fade-in rounded-lg border border-line bg-surface p-3 text-xs text-fg shadow-pop"
        >
          <div className="mb-2.5 flex items-baseline justify-between gap-2">
            <span className="truncate font-mono text-[12px] font-medium">{name}</span>
            <span className={cn("shrink-0 tnum", hover != null ? "text-fg" : "text-faint")}>
              {hover != null && days[hover] ? formatDay(days[hover]) : "30 days"}
            </span>
          </div>
          {loading && !history ? (
            <div className="space-y-2" aria-busy>
              <div className="h-14 animate-pulse rounded bg-sunken" />
              <div className="h-14 animate-pulse rounded bg-sunken" />
            </div>
          ) : !o ? (
            <p className="text-muted">No history for this collection yet: it builds up with every backup.</p>
          ) : (
            <div className="space-y-4">
              <MetricChart label="Data" values={o.data} days={days} fmt={formatBytes} hover={hover} setHover={setHover} tone="accent" />
              <MetricChart label="Documents" values={o.records} days={days} fmt={formatInt} hover={hover} setHover={setHover} tone="muted" />
              <div className="flex items-center justify-between gap-2 border-t border-line pt-2 text-faint tnum">
                {hover != null ? (
                  <span>{formatDayLong(days[hover]!)}{o.data[hover] == null && ": no backup"}</span>
                ) : (
                  <>
                    <span>{formatDay(days[0]!)} — {formatDay(days.at(-1)!)}</span>
                    {data && data.to > data.from && <span>on average {signed(Math.round(data.perDay), formatBytes)}/day</span>}
                  </>
                )}
              </div>
            </div>
          )}
        </P.Content>
      </P.Portal>
    </P.Root>
  );
}
