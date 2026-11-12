/**
 * Small inline-SVG charts (no chart library): a time chart (total line or stacked areas) with a crosshair
 * tooltip, a column chart with per-column tooltips, and a sparkline. Colours come from the theme tokens
 * (--accent, --viz-N, --line, --muted), so light and dark themes both work.
 */
import { useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { cn } from "@/lib/cn";
import { LOCALE } from "@/lib/format";

export const VIZ_SLOTS = 8;
export const vizColor = (slot: number) => `rgb(var(--viz-${(slot % VIZ_SLOTS) + 1}))`;
/** "others" and other folded series: a neutral, never a ninth hue. */
export const REST_COLOR = "rgb(var(--faint))";

/** Width of an element, following resizes. */
export function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [w, setW] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setW(el.clientWidth);
    const ro = new ResizeObserver(() => setW(el.clientWidth));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, w] as const;
}

/** 0 and 3–5 round ticks up to at least `max`. */
export function niceTicks(max: number, count = 4): number[] {
  if (!(max > 0)) return [0, 1];
  const raw = max / count;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw) ?? raw;
  const out: number[] = [];
  for (let v = 0; v < max + step * 0.999; v += step) out.push(v);
  return out;
}

/** For bytes: ticks on powers of 1024 read better (1 GB, 2 GB …). */
export function niceByteTicks(max: number, count = 4): number[] {
  if (!(max > 0)) return [0, 1024];
  const units = [1, 1024, 1024 ** 2, 1024 ** 3, 1024 ** 4, 1024 ** 5];
  const unit = [...units].reverse().find((u) => max / u >= 1) ?? 1;
  return niceTicks(max / unit, count).map((t) => t * unit);
}

const dayFmt = new Intl.DateTimeFormat(LOCALE, { day: "numeric", month: "short", timeZone: "UTC" });
const dayLong = new Intl.DateTimeFormat(LOCALE, { day: "numeric", month: "long", year: "numeric", weekday: "short", timeZone: "UTC" });
/** "2026-11-08" (a calendar day, already local) → "8 Nov". */
export const formatDay = (d: string) => dayFmt.format(new Date(`${d}T00:00:00Z`));
export const formatDayLong = (d: string) => dayLong.format(new Date(`${d}T00:00:00Z`));

export interface TimeSeries { name: string; values: (number | null)[]; color: string }

const PAD = { top: 12, right: 12, bottom: 24, left: 56 };

/** Beside the pointer on the roomier side, clamped inside the chart (measured, so a wide one never spills). */
function Tip({ x, width, children }: { x: number; width: number; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const [left, setLeft] = useState<number | null>(null);
  useLayoutEffect(() => {
    const w = ref.current?.offsetWidth ?? 0;
    const want = x > width / 2 ? x - 10 - w : x + 10;
    setLeft(Math.max(0, Math.min(want, width - w)));
  }, [x, width, children]);
  return (
    <div
      ref={ref}
      className="pointer-events-none absolute top-1 z-10 min-w-[10rem] max-w-[min(16rem,100%)] rounded-md border border-line bg-surface px-2.5 py-2 text-xs shadow-pop"
      style={{ left: left ?? 0, visibility: left == null ? "hidden" : undefined }}
      role="status"
    >
      {children}
    </div>
  );
}

function KeyLine({ color }: { color: string }) {
  return <span aria-hidden className="mt-[7px] inline-block h-0.5 w-3 shrink-0 rounded" style={{ background: color }} />;
}

/**
 * Daily values over time. mode "total": one line (the sum) over a light wash; "stacked": series stacked,
 * bottom first. Crosshair snaps to the nearest day; the tooltip lists every series of that day.
 */
export function TimeChart({
  days, total, series, mode, format, height = 220, ariaLabel, ticks = niceTicks,
}: {
  days: string[]; total: (number | null)[]; series: TimeSeries[]; mode: "total" | "stacked";
  format: (v: number) => string; height?: number; ariaLabel: string; ticks?: (max: number) => number[];
}) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const n = days.length;
  const iw = Math.max(0, width - PAD.left - PAD.right);
  const ih = height - PAD.top - PAD.bottom;

  const stacks = useMemo(() => {
    if (mode !== "stacked") return [];
    const base = new Array<number>(n).fill(0);
    return series.map((s) => {
      const lo = [...base];
      const hi = s.values.map((v, i) => (base[i] = (base[i] ?? 0) + (v ?? 0)));
      return { ...s, lo, hi };
    });
  }, [series, mode, n]);

  const max = Math.max(0, ...total.map((v) => v ?? 0), ...(stacks.at(-1)?.hi ?? []));
  const yt = ticks(max);
  const top = yt.at(-1) || 1;
  const x = (i: number) => PAD.left + (n <= 1 ? iw / 2 : (i / (n - 1)) * iw);
  const y = (v: number) => PAD.top + ih - (v / top) * ih;

  const linePath = (vals: (number | null)[]) => {
    let d = "";
    let pen = false;
    vals.forEach((v, i) => {
      if (v == null) { pen = false; return; }
      d += `${pen ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`;
      pen = true;
    });
    return d;
  };
  const areaPath = (vals: (number | null)[]) => {
    // one closed shape per run of non-null values
    const runs: number[][] = [];
    vals.forEach((v, i) => {
      if (v == null) return;
      const last = runs.at(-1);
      if (last && last.at(-1) === i - 1) last.push(i); else runs.push([i]);
    });
    return runs.map((r) => {
      const a = r.map((i) => `${x(i).toFixed(1)},${y(vals[i] ?? 0).toFixed(1)}`).join("L");
      return `M${x(r[0]!).toFixed(1)},${y(0)}L${a}L${x(r.at(-1)!).toFixed(1)},${y(0)}Z`;
    }).join("");
  };
  const band = (lo: number[], hi: number[]) => {
    if (!n) return "";
    const up = hi.map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`).join("L");
    const down = lo.map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`).reverse().join("L");
    return `M${up}L${down}Z`;
  };

  const xLabels = useMemo(() => {
    if (!n || !iw) return [];
    const every = Math.max(1, Math.ceil(n / Math.max(2, Math.floor(iw / 72))));
    const idx: number[] = [];
    for (let i = n - 1; i >= 0; i -= every) idx.push(i);
    return idx.reverse();
  }, [n, iw]);

  const onMove = (e: React.PointerEvent<SVGRectElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const px = e.clientX - r.left;
    setHover(n <= 1 ? 0 : Math.max(0, Math.min(n - 1, Math.round((px / r.width) * (n - 1)))));
  };
  const lastIdx = (() => { for (let i = n - 1; i >= 0; i--) if (total[i] != null) return i; return -1; })();
  const h = hover;

  return (
    <div ref={ref} className="relative w-full" style={{ height }}>
      {width > 0 && (
        <svg width={width} height={height} role="img" aria-label={ariaLabel} className="block select-none">
          {yt.map((t) => (
            <g key={t}>
              <line x1={PAD.left} x2={PAD.left + iw} y1={y(t)} y2={y(t)} stroke="rgb(var(--line))" strokeWidth={1} shapeRendering="crispEdges" />
              <text x={PAD.left - 8} y={y(t)} dy="0.32em" textAnchor="end" className="fill-faint text-[11px] tnum">{format(t)}</text>
            </g>
          ))}
          {xLabels.map((i) => (
            <text key={i} x={x(i)} y={height - 6} textAnchor={i === 0 && n > 1 ? "start" : i === n - 1 && n > 1 ? "end" : "middle"} className="fill-faint text-[11px]">
              {formatDay(days[i]!)}
            </text>
          ))}
          {mode === "stacked"
            ? stacks.map((s) => (
              <g key={s.name}>
                <path d={band(s.lo, s.hi)} fill={s.color} fillOpacity={0.8} />
                {/* surface gap between bands */}
                <path d={linePath(s.hi)} fill="none" stroke="rgb(var(--surface))" strokeWidth={1.5} strokeLinejoin="round" />
              </g>
            ))
            : (
              <>
                <path d={areaPath(total)} fill="rgb(var(--accent))" fillOpacity={0.1} />
                <path d={linePath(total)} fill="none" stroke="rgb(var(--accent))" strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
                {lastIdx >= 0 && h == null && (
                  <circle cx={x(lastIdx)} cy={y(total[lastIdx] ?? 0)} r={4} fill="rgb(var(--accent))" stroke="rgb(var(--surface))" strokeWidth={2} />
                )}
              </>
            )}
          {h != null && (
            <>
              <line x1={x(h)} x2={x(h)} y1={PAD.top} y2={PAD.top + ih} stroke="rgb(var(--line-strong))" strokeWidth={1} />
              {total[h] != null && mode === "total" && (
                <circle cx={x(h)} cy={y(total[h] ?? 0)} r={4} fill="rgb(var(--accent))" stroke="rgb(var(--surface))" strokeWidth={2} />
              )}
            </>
          )}
          <rect
            x={PAD.left} y={PAD.top} width={iw} height={ih} fill="transparent"
            onPointerMove={onMove} onPointerDown={onMove} onPointerLeave={() => setHover(null)}
          />
        </svg>
      )}
      {h != null && days[h] && (
        <Tip x={x(h)} width={width}>
          <div className="mb-1 text-muted">{formatDayLong(days[h])}</div>
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-muted">Total</span>
            <span className="font-semibold text-fg tnum">{total[h] == null ? "no data" : format(total[h])}</span>
          </div>
          {mode === "stacked" && series.length > 1 && (
            <ul className="mt-1 space-y-0.5 border-t border-line pt-1">
              {[...series].reverse().map((s) => (
                <li key={s.name} className="flex items-start gap-1.5">
                  <KeyLine color={s.color} />
                  <span className="min-w-0 flex-1 truncate text-muted">{s.name}</span>
                  <span className="text-fg tnum">{s.values[h] == null ? "—" : format(s.values[h])}</span>
                </li>
              ))}
            </ul>
          )}
        </Tip>
      )}
    </div>
  );
}

export interface Column { key: string; label: string; value: number; tone?: "accent" | "danger" | "muted"; detail?: ReactNode }

const TONE: Record<NonNullable<Column["tone"]>, string> = {
  accent: "rgb(var(--accent))",
  danger: "rgb(var(--danger))",
  muted: "rgb(var(--faint))",
};

/** Columns from one baseline, ≤24px wide with a rounded cap; each column is its own hover / focus target. */
export function ColumnChart({ columns, format, height = 160, ariaLabel, labelEvery }: {
  columns: Column[]; format: (v: number) => string; height?: number; ariaLabel: string; labelEvery?: number;
}) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const pad = { top: 18, bottom: 22 };
  const ih = height - pad.top - pad.bottom;
  const max = Math.max(0, ...columns.map((c) => c.value)) || 1;
  const slot = columns.length ? width / columns.length : 0;
  const bw = Math.max(2, Math.min(24, slot - 4));
  const every = labelEvery ?? Math.max(1, Math.ceil(columns.length / Math.max(1, Math.floor(width / 56))));
  const r = Math.min(4, bw / 2);

  return (
    <div ref={ref} className="relative w-full" style={{ height }}>
      {width > 0 && (
        <svg width={width} height={height} role="img" aria-label={ariaLabel} className="block select-none">
          <line x1={0} x2={width} y1={pad.top + ih} y2={pad.top + ih} stroke="rgb(var(--line))" strokeWidth={1} shapeRendering="crispEdges" />
          {columns.map((c, i) => {
            const hgt = c.value > 0 ? Math.max(2, (c.value / max) * ih) : 0;
            const cx = i * slot + slot / 2;
            const x0 = cx - bw / 2;
            const y0 = pad.top + ih - hgt;
            const rr = Math.min(r, hgt);
            const d = hgt > 0
              ? `M${x0},${pad.top + ih}V${y0 + rr}Q${x0},${y0} ${x0 + rr},${y0}H${x0 + bw - rr}Q${x0 + bw},${y0} ${x0 + bw},${y0 + rr}V${pad.top + ih}Z`
              : "";
            const showLabel = i % every === (columns.length - 1) % every;
            return (
              <g key={c.key}>
                {d && <path d={d} fill={TONE[c.tone ?? "accent"]} fillOpacity={hover === i ? 1 : 0.85} />}
                {showLabel && <text x={Math.max(22, Math.min(width - 22, cx))} y={height - 6} textAnchor="middle" className="fill-faint text-[11px]">{c.label}</text>}
                <rect
                  x={i * slot} y={0} width={slot} height={pad.top + ih} fill="transparent" tabIndex={0}
                  aria-label={`${c.label}: ${format(c.value)}`}
                  onPointerEnter={() => setHover(i)} onPointerLeave={() => setHover(null)}
                  onFocus={() => setHover(i)} onBlur={() => setHover(null)}
                  className="outline-none"
                />
              </g>
            );
          })}
        </svg>
      )}
      {hover != null && columns[hover] && (
        <Tip x={hover * slot + slot / 2} width={width}>
          <div className="mb-0.5 text-muted">{columns[hover].label}</div>
          <div className="font-semibold text-fg tnum">{format(columns[hover].value)}</div>
          {columns[hover].detail && <div className="mt-0.5 text-muted">{columns[hover].detail}</div>}
        </Tip>
      )}
    </div>
  );
}

/** A tiny trend line; gaps where the value is unknown, the last point marked. */
export function Sparkline({ values, width = 88, height = 24, className, label }: {
  values: (number | null)[]; width?: number; height?: number; className?: string; label?: string;
}) {
  const nums = values.filter((v): v is number => v != null);
  if (nums.length < 2) return <span className={cn("inline-block text-xs text-faint", className)} style={{ width }}>—</span>;
  const min = Math.min(...nums), max = Math.max(...nums);
  const span = max - min || 1;
  const n = values.length;
  const px = (i: number) => 3 + (i / Math.max(1, n - 1)) * (width - 6);
  const py = (v: number) => 3 + (height - 6) * (1 - (v - min) / span);
  let d = "";
  let pen = false;
  let last = -1;
  values.forEach((v, i) => {
    if (v == null) { pen = false; return; }
    d += `${pen ? "L" : "M"}${px(i).toFixed(1)},${py(v).toFixed(1)}`;
    pen = true;
    last = i;
  });
  return (
    <svg width={width} height={height} className={cn("block shrink-0", className)} role="img" aria-label={label ?? "trend"}>
      <path d={d} fill="none" stroke="rgb(var(--muted))" strokeOpacity={0.7} strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
      <circle cx={px(last)} cy={py(values[last] ?? 0)} r={2.5} fill="rgb(var(--accent))" />
    </svg>
  );
}
