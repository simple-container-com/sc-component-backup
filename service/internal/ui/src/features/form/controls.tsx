import { CheckCircle2, Info, Minus, Plus, Search, X, XCircle, AlertTriangle, Server } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import type { EngineId, EngineMeta, Retention, TestConnectionResult } from "@/api/types";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Field, TextInput } from "@/components/ui/Field";
import { Segmented } from "@/components/ui/Segmented";
import { Select } from "@/components/ui/Select";
import { Checkbox } from "@/components/ui/Switch";
import { cn } from "@/lib/cn";
import { humanDuration, humanInterval, KEPT_BY_LABEL, localToUtcHM, localUtcOffset, parseDuration, plural, retentionSummary } from "@/lib/format";
import { argFlag, RETENTION_KEYS, SYSTEM_DBS, type Errors, type FormValues } from "./model";

/** Settings-style section: title + explanation on the left, controls on the right. */
export function Section({ title, description, children, id }: { title: string; description?: ReactNode; children: ReactNode; id?: string }) {
  return (
    <section aria-labelledby={id} className="grid gap-x-10 gap-y-4 border-t border-line py-7 first:border-t-0 first:pt-0 md:grid-cols-[220px_minmax(0,1fr)]">
      <div>
        <h2 id={id} className="text-[15px] font-semibold">{title}</h2>
        {description && <p className="mt-1 text-sm text-muted">{description}</p>}
      </div>
      <div className="min-w-0 space-y-5">{children}</div>
    </section>
  );
}

export function Chip({ active, onClick, children, label }: { active?: boolean; onClick: () => void; children: ReactNode; label?: string }) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      aria-label={label}
      className={cn(
        "h-6 rounded border px-2 font-mono text-xs transition-colors",
        active ? "border-accent/40 bg-accent-soft text-accent" : "border-line bg-surface text-muted hover:border-line-strong hover:text-fg",
      )}
    >
      {children}
    </button>
  );
}

export function DurationField({
  id, label, value, onChange, error, presets, kind, hint,
}: {
  id: string; label: string; value: string; onChange: (v: string) => void; error?: string;
  presets: string[]; kind: "interval" | "timeout"; hint?: string;
}) {
  const human = humanDuration(value);
  return (
    <Field
      label={label}
      htmlFor={id}
      error={error}
      hint={hint}
      aside={human && !error ? <span className="text-xs text-muted">{kind === "interval" ? humanInterval(value) : `no longer than ${human}`}</span> : null}
    >
      <div className="flex flex-wrap items-center gap-2">
        <TextInput
          id={id}
          value={value}
          onChange={(e) => onChange(e.target.value.replace(/\s+/g, "").toLowerCase())}
          invalid={!!error}
          className="w-28 font-mono"
          inputMode="text"
          autoComplete="off"
          spellCheck={false}
          placeholder={kind === "interval" ? "1d" : "6h"}
        />
        <div className="flex flex-wrap gap-1" role="group" aria-label={`Quick pick: ${label.toLowerCase()}`}>
          {presets.map((p) => <Chip key={p} active={value === p} onClick={() => onChange(p)}>{p}</Chip>)}
        </div>
      </div>
    </Field>
  );
}

/** Integer input with custom −/+ buttons (no native number stepper). */
export function CountField({
  id, label, value, onChange, min, max, error, hint, suffix, compact,
}: {
  id: string; label: ReactNode; value: string; onChange: (v: string) => void; min: number; max: number;
  error?: string; hint?: ReactNode; suffix?: string; compact?: boolean;
}) {
  const n = /^\d+$/.test(value) ? parseInt(value, 10) : NaN;
  const step = (d: number) => {
    const base = Number.isNaN(n) ? min : n;
    onChange(String(Math.min(max, Math.max(min, base + d))));
  };
  const btn = "flex h-9 w-8 items-center justify-center text-muted hover:bg-sunken hover:text-fg disabled:opacity-35 disabled:hover:bg-transparent";
  return (
    <Field label={label} htmlFor={id} error={error} hint={hint}>
      <div className={cn("flex items-center", compact ? "" : "gap-2")}>
        <div className={cn("inline-flex items-stretch overflow-hidden rounded border bg-surface", error ? "border-danger" : "border-line-strong/80 focus-within:border-accent focus-within:ring-[3px] focus-within:ring-accent/15")}>
          <button type="button" className={btn} onClick={() => step(-1)} disabled={!Number.isNaN(n) && n <= min} aria-label="Decrease" tabIndex={-1}>
            <Minus className="h-3.5 w-3.5" />
          </button>
          <input
            id={id}
            type="text"
            inputMode="numeric"
            pattern="[0-9]*"
            autoComplete="off"
            value={value}
            aria-invalid={!!error || undefined}
            aria-describedby={error ? `${id}-err` : hint ? `${id}-hint` : undefined}
            onChange={(e) => onChange(e.target.value.replace(/[^\d]/g, "").slice(0, 5))}
            onKeyDown={(e) => {
              if (e.key === "ArrowUp") { e.preventDefault(); step(1); }
              if (e.key === "ArrowDown") { e.preventDefault(); step(-1); }
            }}
            className="h-9 w-12 border-x border-line bg-transparent text-center text-sm tnum focus:outline-none"
          />
          <button type="button" className={btn} onClick={() => step(1)} disabled={!Number.isNaN(n) && n >= max} aria-label="Increase" tabIndex={-1}>
            <Plus className="h-3.5 w-3.5" />
          </button>
        </div>
        {suffix && <span className="text-sm text-muted">{suffix}</span>}
      </div>
    </Field>
  );
}

const RET_LABEL: Record<keyof Retention, { title: string; unit: [string, string] }> = {
  last: { title: "Latest", unit: ["archive", "archives"] },
  daily: { title: "Daily", unit: ["day", "days"] },
  weekly: { title: "Weekly", unit: ["week", "weeks"] },
  monthly: { title: "Monthly", unit: ["month", "months"] },
};

export function RetentionEditor({ v, set, errors }: { v: FormValues; set: (r: FormValues["retention"]) => void; errors: Errors }) {
  const nums = RETENTION_KEYS.map((k) => (/^\d+$/.test(v.retention[k]) ? parseInt(v.retention[k], 10) : NaN));
  const valid = nums.every((x) => !Number.isNaN(x));
  const r = valid ? { last: nums[0]!, daily: nums[1]!, weekly: nums[2]!, monthly: nums[3]! } : null;
  return (
    <div>
      <div className="grid grid-cols-2 gap-x-4 gap-y-4 sm:grid-cols-4">
        {RETENTION_KEYS.map((k, i) => (
          <CountField
            key={k}
            id={`ret-${k}`}
            label={RET_LABEL[k].title}
            value={v.retention[k]}
            onChange={(x) => set({ ...v.retention, [k]: x })}
            min={0}
            max={10000}
            compact
            error={errors[`retention.${k}`]}
            hint={!Number.isNaN(nums[i]!) ? plural(nums[i]!, RET_LABEL[k].unit) : undefined}
          />
        ))}
      </div>
      <p className={cn("mt-4 flex gap-2 text-sm", errors.retention ? "text-danger" : "text-fg/80")} role={errors.retention ? "alert" : undefined}>
        <Info className={cn("mt-0.5 h-4 w-4 shrink-0", errors.retention ? "text-danger" : "text-accent")} aria-hidden />
        <span>{errors.retention ?? (r ? retentionSummary(r) : "Fix the fields above to see what will be kept.")}</span>
      </p>
      <p className="mt-2 text-xs text-faint">
        Rules add up: an archive stays while at least one rule keeps it. The labels on an archive ({RETENTION_KEYS.map((k) => `"${KEPT_BY_LABEL[k]}"`).join(", ")}) show which one.
      </p>
    </div>
  );
}

export function DatabasePicker({
  engine, mode, selected, available, onMode, onSelected, error, tested,
}: {
  engine: EngineId; mode: "all" | "list"; selected: string[]; available: string[] | null;
  onMode: (m: "all" | "list") => void; onSelected: (dbs: string[]) => void; error?: string; tested: boolean;
}) {
  const [q, setQ] = useState("");
  const [custom, setCustom] = useState("");
  const items = useMemo(() => {
    const set = new Set([...(available ?? []), ...selected]);
    return [...set].sort((a, b) => a.localeCompare(b));
  }, [available, selected]);
  const filtered = q ? items.filter((d) => d.toLowerCase().includes(q.toLowerCase())) : items;
  const sel = new Set(selected);
  const allVisibleOn = filtered.length > 0 && filtered.every((d) => sel.has(d));
  const someVisibleOn = filtered.some((d) => sel.has(d));
  const toggle = (d: string) => onSelected(sel.has(d) ? selected.filter((x) => x !== d) : [...selected, d]);
  const toggleVisible = () => {
    if (allVisibleOn) onSelected(selected.filter((d) => !filtered.includes(d)));
    else onSelected([...new Set([...selected, ...filtered])]);
  };
  const addCustom = () => {
    const n = custom.trim();
    if (n && !sel.has(n)) onSelected([...selected, n]);
    setCustom("");
  };

  return (
    <div>
      <div className="mb-1.5 flex items-baseline justify-between gap-2">
        <span id="dbs-label" className="text-sm font-medium">What to back up</span>
        {available && <span className="text-xs text-muted">{available.length} {plural(available.length, ["database", "databases"])} on the server</span>}
      </div>
      <Segmented
        ariaLabel="Which databases to back up"
        value={mode}
        onChange={onMode}
        options={[{ value: "all", label: "All databases" }, { value: "list", label: "Selected only" }]}
      />
      {mode === "all" ? (
        <p className="mt-2 text-xs text-muted">
          {engine === "s3import"
            ? "Each top-level folder in the bucket (e.g. ghost, @content) is a separate \"database\"; new folders are picked up automatically."
            : `Every run fetches the database list again, so new databases are backed up automatically. System databases: ${SYSTEM_DBS[engine]}.`}
          {available && available.length > 0 && (
            <span className="mt-1.5 block font-mono text-faint">{available.slice(0, 6).join(", ")}{available.length > 6 ? ` and ${available.length - 6} more` : ""}</span>
          )}
        </p>
      ) : (
        <div className={cn("mt-3 overflow-hidden rounded-md border bg-surface", error ? "border-danger/60" : "border-line")}>
          {items.length > 0 && (
            <div className="flex items-center gap-2 border-b border-line px-2 py-1.5">
              <button type="button" onClick={toggleVisible} className="flex items-center gap-2 rounded px-1 py-0.5 text-xs text-muted hover:text-fg" aria-label={allVisibleOn ? "Deselect all visible" : "Select all visible"}>
                <Checkbox checked={allVisibleOn} indeterminate={!allVisibleOn && someVisibleOn} />
                <span className="tnum">{selected.length} of {items.length}</span>
              </button>
              {items.length > 8 && (
                <div className="relative ml-auto w-44">
                  <Search className="pointer-events-none absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-faint" aria-hidden />
                  <input
                    type="search"
                    value={q}
                    onChange={(e) => setQ(e.target.value)}
                    placeholder="Find a database"
                    aria-label="Find a database"
                    className="h-7 w-full rounded border border-line bg-sunken pl-7 pr-2 text-xs focus:border-accent focus:outline-none"
                  />
                </div>
              )}
            </div>
          )}
          <ul role="group" aria-labelledby="dbs-label" className="max-h-64 overflow-y-auto py-1">
            {filtered.map((d) => {
              const missing = tested && available && !available.includes(d);
              return (
                <li key={d}>
                  <button
                    type="button"
                    role="checkbox"
                    aria-checked={sel.has(d)}
                    onClick={() => toggle(d)}
                    className="flex w-full items-center gap-2.5 px-3 py-1.5 text-left text-sm hover:bg-sunken focus-visible:bg-sunken"
                  >
                    <Checkbox checked={sel.has(d)} />
                    <span className="min-w-0 flex-1 truncate font-mono text-[12.5px]">{d}</span>
                    {missing && <Badge tone="warn">not on server</Badge>}
                  </button>
                </li>
              );
            })}
            {items.length === 0 && (
              <li className="px-3 py-3 text-sm text-muted">
                {tested ? "The server returned no databases." : "Check the connection to get the database list, or add a name manually."}
              </li>
            )}
            {items.length > 0 && filtered.length === 0 && <li className="px-3 py-3 text-sm text-muted">Nothing matches "{q}"</li>}
          </ul>
          <div className="flex gap-2 border-t border-line bg-sunken/50 p-2">
            <input
              type="text"
              value={custom}
              onChange={(e) => setCustom(e.target.value)}
              onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); addCustom(); } }}
              placeholder="Add a database by name"
              aria-label="Database name"
              spellCheck={false}
              className="h-7 min-w-0 flex-1 rounded border border-line bg-surface px-2 text-xs placeholder:text-faint focus:border-accent focus:outline-none"
            />
            <Button size="sm" onClick={addCustom} disabled={!custom.trim()}>Add</Button>
          </div>
        </div>
      )}
      {error && <p role="alert" className="mt-1.5 text-xs text-danger">{error}</p>}
    </div>
  );
}

export function DumpArgsEditor({ engine, value, onChange, error }: { engine: EngineMeta; value: string[]; onChange: (v: string[]) => void; error?: string }) {
  const [flag, setFlag] = useState<string | null>(engine.dump_args[0] ?? null);
  const [val, setVal] = useState("");
  const [localErr, setLocalErr] = useState<string | null>(null);
  const add = () => {
    if (!flag) return;
    const x = val.trim();
    if (!x) { setLocalErr("Enter a value for the flag"); return; }
    if (/\s/.test(x) && !/^".*"$/.test(x)) { setLocalErr("The value must not contain spaces"); return; }
    const arg = `${flag}=${x}`;
    if (value.includes(arg)) { setLocalErr("This argument is already added"); return; }
    onChange([...value, arg]);
    setVal("");
    setLocalErr(null);
  };
  return (
    <div>
      <div className="mb-1.5 flex items-baseline justify-between">
        <span className="text-sm font-medium">Dump arguments</span>
        <span className="text-xs text-faint">only those allowed for {engine.label}</span>
      </div>
      {value.length > 0 && (
        <ul className="mb-2 flex flex-wrap gap-1.5" aria-label="Added arguments">
          {value.map((a) => {
            const ok = engine.dump_args.includes(argFlag(a));
            return (
              <li key={a} className={cn("inline-flex h-7 items-center gap-1 rounded border pl-2 pr-0.5 font-mono text-xs", ok ? "border-line bg-sunken" : "border-danger/40 bg-danger-soft text-danger")}>
                {a}
                <button type="button" onClick={() => onChange(value.filter((x) => x !== a))} className="rounded p-1 text-faint hover:bg-line/60 hover:text-fg" aria-label={`Remove ${a}`}>
                  <X className="h-3 w-3" />
                </button>
              </li>
            );
          })}
        </ul>
      )}
      <div className="flex flex-wrap gap-2 sm:flex-nowrap">
        <Select
          ariaLabel="Flag"
          value={flag}
          onChange={(f) => { setFlag(f); setLocalErr(null); }}
          options={engine.dump_args.map((f) => ({ value: f, label: <span className="font-mono text-xs">{f}</span>, text: f }))}
          className="w-full font-mono sm:w-64"
        />
        <span className="hidden self-center font-mono text-faint sm:inline">=</span>
        <TextInput
          value={val}
          onChange={(e) => { setVal(e.target.value); setLocalErr(null); }}
          onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); add(); } }}
          placeholder="value"
          aria-label="Flag value"
          invalid={!!localErr}
          spellCheck={false}
          className="min-w-0 flex-1 font-mono"
        />
        <Button onClick={add} icon={<Plus className="h-3.5 w-3.5" />}>Add</Button>
      </div>
      {(localErr || error) && <p role="alert" className="mt-1.5 text-xs text-danger">{localErr ?? error}</p>}
    </div>
  );
}

export function ConnectionResult({ result }: { result: TestConnectionResult }) {
  if (!result.ok) {
    return (
      <div role="alert" className="rounded-md border border-danger/30 bg-danger-soft/50 px-3.5 py-3">
        <div className="flex items-center gap-2 text-sm font-medium text-danger"><XCircle className="h-4 w-4" aria-hidden /> Could not connect</div>
        {result.error && <p className="mt-1.5 break-words font-mono text-xs text-fg/80">{result.error}</p>}
        <p className="mt-2 text-xs text-muted">Check the host, port, username and password, and that the NAS can reach the server.</p>
      </div>
    );
  }
  return (
    <div role="status" className={cn("rounded-md border px-3.5 py-3", result.warnings.length ? "border-warn/30 bg-warn-soft/40" : "border-ok/25 bg-ok-soft/40")}>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
        <span className="flex items-center gap-2 text-sm font-medium text-ok"><CheckCircle2 className="h-4 w-4" aria-hidden /> Connection works</span>
        <span className="flex items-center gap-1.5 text-xs text-muted">
          <Server className="h-3.5 w-3.5" aria-hidden />
          server <span className="font-mono text-fg">{result.server_version ?? "?"}</span>
          <span className="text-faint">/</span>
          client <span className="font-mono text-fg">{result.client_version ?? "?"}</span>
        </span>
        <span className="text-xs text-muted tnum">{result.databases.length} {plural(result.databases.length, ["database", "databases"])}</span>
      </div>
      {result.warnings.length > 0 && (
        <ul className="mt-2.5 space-y-1">
          {result.warnings.map((w, i) => (
            <li key={i} className="flex gap-2 text-xs text-warn">
              <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" aria-hidden />
              <span className="break-words">{w}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

const HOURS = Array.from({ length: 24 }, (_, h) => String(h).padStart(2, "0"));
const MINUTES = Array.from({ length: 12 }, (_, i) => String(i * 5).padStart(2, "0"));

/** Run time in local time: "by interval" or hour + minute (custom selects, never native time inputs). */
export function StartAtField({ value, onChange, interval, error }: { value: string; onChange: (v: string) => void; interval: string; error?: string }) {
  const [h, m] = value ? value.split(":") : ["", ""];
  const mins = parseDuration(interval);
  const sub = mins && mins < 1440 && 1440 % mins === 0 && value ? slotsOfDay(value, mins) : null;
  return (
    <Field
      label="Start time"
      htmlFor="f-start-h"
      error={error}
      hint={value
        ? `In your local time (${localUtcOffset()}); stored on the server in UTC: ${localToUtcHM(value)}.${mins && mins >= 10080 && mins % 10080 === 0 ? " Weekly runs happen on Mondays (UTC)." : ""}`
        : "Not set: the next run starts one interval after the start of the last successful one."}
      aside={sub ? <span className="text-xs text-muted">{sub}</span> : null}
    >
      <div className="flex flex-wrap items-center gap-2">
        <Chip active={!value} onClick={() => onChange("")}>by interval</Chip>
        <Select<string>
          id="f-start-h"
          ariaLabel="Start hour"
          value={h || null}
          placeholder="hour"
          onChange={(x) => onChange(`${x}:${m || "00"}`)}
          options={HOURS.map((x) => ({ value: x, label: <span className="font-mono">{x}</span>, text: x }))}
          className="w-24"
          invalid={!!error}
        />
        <span className="text-muted">:</span>
        <Select<string>
          ariaLabel="Start minute"
          value={m || null}
          placeholder="min"
          onChange={(x) => onChange(`${h || "03"}:${x}`)}
          options={MINUTES.map((x) => ({ value: x, label: <span className="font-mono">{x}</span>, text: x }))}
          className="w-24"
          invalid={!!error}
        />
      </div>
    </Field>
  );
}

/** "03:00, 09:00, 15:00, 21:00" for an interval that divides a day. */
function slotsOfDay(hm: string, mins: number): string {
  const [h, m] = hm.split(":").map(Number);
  const start = (h ?? 0) * 60 + (m ?? 0);
  const out: string[] = [];
  for (let t = start % mins; t < 1440 && out.length < 6; t += mins) out.push(`${String(Math.floor(t / 60)).padStart(2, "0")}:${String(t % 60).padStart(2, "0")}`);
  return out.join(", ") + (1440 / mins > 6 ? "…" : "");
}
