import type { Retention } from "@/api/types";

/** English plural: plural(5, ["day", "days"]) → "days". */
export function plural(n: number, forms: readonly [string, string]): string {
  return Math.abs(n) === 1 ? forms[0] : forms[1];
}

/** One locale for numbers and dates: English text, day-month order and a 24-hour clock. */
export const LOCALE = "en-GB";

const numFmt = new Intl.NumberFormat(LOCALE, { maximumFractionDigits: 1 });
const intFmt = new Intl.NumberFormat(LOCALE);

export function formatInt(n: number) {
  return intFmt.format(n);
}

export function formatBytes(bytes: number | null | undefined): string {
  if (bytes == null || !Number.isFinite(bytes)) return "—";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB", "PB"];
  let v = bytes / 1024;
  let i = 0;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  const s = v >= 100 ? Math.round(v).toString() : numFmt.format(v);
  return `${s} ${units[i]}`;
}

export function toDate(iso: string | null | undefined): Date | null {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** "just now", "5 min ago", "in 3 h", "2 days ago". */
export function formatRelative(iso: string | null | undefined, now = Date.now()): string {
  const d = toDate(iso);
  if (!d) return "—";
  const diff = d.getTime() - now;
  const future = diff > 0;
  const s = Math.abs(diff) / 1000;
  let text: string;
  if (s < 45) return future ? "in a few seconds" : "just now";
  if (s < 3600) {
    const m = Math.max(1, Math.round(s / 60));
    text = `${m} min`;
  } else if (s < 86400) {
    text = `${Math.max(1, Math.floor(s / 3600))} h`;
  } else if (s < 86400 * 45) {
    const days = Math.max(1, Math.round(s / 86400));
    text = `${days} ${plural(days, ["day", "days"])}`;
  } else {
    const mo = Math.round(s / (86400 * 30));
    text = `${mo} ${plural(mo, ["month", "months"])}`;
  }
  return future ? `in ${text}` : `${text} ago`;
}

const dtSameYear = new Intl.DateTimeFormat(LOCALE, { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
const dtFull = new Intl.DateTimeFormat(LOCALE, { day: "numeric", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit" });
const dtSeconds = new Intl.DateTimeFormat(LOCALE, {
  day: "numeric", month: "long", year: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit", timeZoneName: "short",
});
const timeOnly = new Intl.DateTimeFormat(LOCALE, { hour: "2-digit", minute: "2-digit" });

/** Compact local date-time: "5 Nov, 14:20". */
export function formatDateTime(iso: string | null | undefined): string {
  const d = toDate(iso);
  if (!d) return "—";
  return (d.getFullYear() === new Date().getFullYear() ? dtSameYear : dtFull).format(d);
}

/** Full local timestamp for tooltips. */
export function formatExact(iso: string | null | undefined): string {
  const d = toDate(iso);
  return d ? dtSeconds.format(d) : "—";
}

export function formatTime(iso: string | null | undefined): string {
  const d = toDate(iso);
  return d ? timeOnly.format(d) : "—";
}

/** Seconds → "3.1 s", "4 min 12 s", "1 h 03 min". */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds == null || !Number.isFinite(seconds) || seconds < 0) return "—";
  if (seconds < 10) return `${numFmt.format(seconds)} s`;
  const t = Math.round(seconds);
  const h = Math.floor(t / 3600);
  const m = Math.floor((t % 3600) / 60);
  const s = t % 60;
  if (h > 0) return `${h} h ${String(m).padStart(2, "0")} min`;
  if (m > 0) return `${m} min ${String(s).padStart(2, "0")} s`;
  return `${s} s`;
}

/** Elapsed "mm:ss" / "h:mm:ss" for live counters. */
export function formatClock(seconds: number): string {
  const t = Math.max(0, Math.floor(seconds));
  const h = Math.floor(t / 3600);
  const m = Math.floor((t % 3600) / 60);
  const s = t % 60;
  const mm = String(m).padStart(2, "0");
  const ss = String(s).padStart(2, "0");
  return h > 0 ? `${h}:${mm}:${ss}` : `${mm}:${ss}`;
}

export function secondsBetween(a: string | null | undefined, b: string | null | undefined | number): number | null {
  const da = toDate(a);
  const db = typeof b === "number" ? new Date(b) : toDate(b);
  if (!da || !db) return null;
  return (db.getTime() - da.getTime()) / 1000;
}

// ---- Durations (`30m`, `6h`, `1d12h`, `1w`) ----

const DURATION_RE = /^(?:(\d+)w)?(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?$/;

/** Parses a duration into minutes; null if invalid. */
export function parseDuration(text: string): number | null {
  const t = text.trim();
  if (!t) return null;
  const m = DURATION_RE.exec(t);
  if (!m) return null;
  const [w, d, h, mi] = [m[1], m[2], m[3], m[4]].map((x) => (x ? parseInt(x, 10) : 0)) as [number, number, number, number];
  return w * 7 * 1440 + d * 1440 + h * 60 + mi;
}

export function validateDuration(text: string, label = "Interval"): string | null {
  if (!text.trim()) return `${label}: enter a value, e.g. 6h or 1d`;
  const min = parseDuration(text);
  if (min == null) return "Format: a number and a unit — m, h, d, w. E.g. 30m, 6h, 1d12h";
  if (min < 1) return "At least one minute";
  return null;
}

/** "every 6 h", "once a day", "every 1 day 12 h". */
export function humanDuration(text: string): string | null {
  const total = parseDuration(text);
  if (total == null || total < 1) return null;
  const w = Math.floor(total / (7 * 1440));
  let rest = total - w * 7 * 1440;
  const d = Math.floor(rest / 1440);
  rest -= d * 1440;
  const h = Math.floor(rest / 60);
  const m = rest - h * 60;
  const parts: string[] = [];
  if (w) parts.push(`${w} ${plural(w, ["week", "weeks"])}`);
  if (d) parts.push(`${d} ${plural(d, ["day", "days"])}`);
  if (h) parts.push(`${h} h`);
  if (m) parts.push(`${m} min`);
  return parts.join(" ");
}

export function humanInterval(text: string): string {
  const total = parseDuration(text);
  if (total === 1440) return "once a day";
  if (total === 10080) return "once a week";
  if (total === 60) return "every hour";
  const h = humanDuration(text);
  return h ? `every ${h}` : text;
}

// ---- Retention ----

export function retentionSummary(r: Retention): string {
  const parts: string[] = [];
  if (r.last > 0) parts.push(`the last ${r.last} ${plural(r.last, ["archive", "archives"])}`);
  if (r.daily > 0) parts.push(`one a day for ${r.daily} ${plural(r.daily, ["day", "days"])}`);
  if (r.weekly > 0) parts.push(`one a week for ${r.weekly} ${plural(r.weekly, ["week", "weeks"])}`);
  if (r.monthly > 0) parts.push(`one a month for ${r.monthly} ${plural(r.monthly, ["month", "months"])}`);
  if (!parts.length) return "Nothing is kept — set at least one rule.";
  const max = r.last + r.daily + r.weekly + r.monthly;
  const head = parts.length > 1 ? `${parts.slice(0, -1).join(", ")} and ${parts[parts.length - 1]}` : parts[0];
  const span =
    r.monthly > 0 ? `${r.monthly} ${plural(r.monthly, ["month", "months"])}`
    : r.weekly > 0 ? `${r.weekly} ${plural(r.weekly, ["week", "weeks"])}`
    : r.daily > 0 ? `${r.daily} ${plural(r.daily, ["day", "days"])}`
    : null;
  return `Keeps ${head} — at most ${max} ${plural(max, ["archive", "archives"])} per database${span ? `, reaching ${span} back` : ""}.`;
}

export const KEPT_BY_LABEL: Record<keyof Retention, string> = {
  last: "recent",
  daily: "daily",
  weekly: "weekly",
  monthly: "monthly",
};

export function keptByExplanation(rule: keyof Retention, r?: Retention): string {
  const n = r?.[rule];
  switch (rule) {
    case "last": return n != null ? `One of the last ${n} ${plural(n, ["archive", "archives"])}` : "One of the most recent archives";
    case "daily": return n != null ? `The day's archive — kept for ${n} ${plural(n, ["day", "days"])}` : "The day's archive";
    case "weekly": return n != null ? `The week's archive — kept for ${n} ${plural(n, ["week", "weeks"])}` : "The week's archive";
    case "monthly": return n != null ? `The month's archive — kept for ${n} ${plural(n, ["month", "months"])}` : "The month's archive";
  }
}

export const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

/** "HH:MM" UTC → "HH:MM" local, by today's offset (the UI always shows local time; the server runs in UTC). */
export function utcToLocalHM(hm: string): string {
  return shiftHM(hm, -new Date().getTimezoneOffset());
}

/** "HH:MM" local → "HH:MM" UTC. Both directions use the same (current) offset, so a round trip is exact even on
 *  a DST change day. */
export function localToUtcHM(hm: string): string {
  return shiftHM(hm, new Date().getTimezoneOffset());
}

function shiftHM(hm: string, minutes: number): string {
  const [h, m] = hm.split(":").map(Number);
  const t = ((((h ?? 0) * 60 + (m ?? 0) + minutes) % 1440) + 1440) % 1440;
  return `${String(Math.floor(t / 60)).padStart(2, "0")}:${String(t % 60).padStart(2, "0")}`;
}

/** The browser's offset from UTC, e.g. "UTC+3". */
export function localUtcOffset(): string {
  const off = -new Date().getTimezoneOffset();
  const h = Math.floor(Math.abs(off) / 60), m = Math.abs(off) % 60;
  return `UTC${off >= 0 ? "+" : "−"}${h}${m ? `:${String(m).padStart(2, "0")}` : ""}`;
}

/** "every day at 06:00" — the interval plus the fixed local run time, if any. */
export function humanSchedule(interval: string, startAtUtc?: string | null): string {
  const base = humanInterval(interval);
  if (!startAtUtc) return base;
  const t = utcToLocalHM(startAtUtc);
  const mins = parseDuration(interval);
  return mins === 1440 ? `every day at ${t}` : mins && mins >= 1440 ? `${base}, at ${t}` : `${base} from ${t}`;
}
