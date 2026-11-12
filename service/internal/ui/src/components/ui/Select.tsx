import * as Popover from "@radix-ui/react-popover";
import { Check, ChevronDown, Search } from "lucide-react";
import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { cn } from "@/lib/cn";

export interface SelectOption<T extends string> { value: T; label: ReactNode; hint?: ReactNode; text?: string }

/**
 * Custom single-select: button + popover listbox (never the native element).
 * Keyboard: ↑/↓/Home/End move, Enter/Space choose, Esc close, letters jump.
 * More than SEARCH_FROM options: a search field on top filters them (by `text`, else the value).
 */
const SEARCH_FROM = 10;
const textOf = <T extends string>(o: SelectOption<T>) => (o.text ?? String(o.value)).toLowerCase();

export function Select<T extends string>({
  value, onChange, options, placeholder = "Select…", ariaLabel, id, className, invalid,
}: {
  value: T | null; onChange: (v: T) => void; options: SelectOption<T>[]; placeholder?: string;
  ariaLabel?: string; id?: string; className?: string; invalid?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const [q, setQ] = useState("");
  const listRef = useRef<HTMLUListElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const uid = useId().replace(/:/g, "");
  const selected = options.find((o) => o.value === value);
  const searchable = options.length > SEARCH_FROM;
  const needle = q.trim().toLowerCase();
  const shown = needle ? options.filter((o) => textOf(o).includes(needle)) : options;

  useEffect(() => {
    if (open) { setQ(""); setActive(Math.max(0, options.findIndex((o) => o.value === value))); }
  }, [open, options, value]);

  useEffect(() => {
    if (!open) return;
    listRef.current?.querySelector<HTMLElement>(`[data-idx="${active}"]`)?.scrollIntoView({ block: "nearest" });
  }, [active, open]);

  const choose = (i: number) => {
    const o = shown[i];
    if (!o) return;
    onChange(o.value);
    setOpen(false);
  };

  const onKey = (e: KeyboardEvent, typing = false) => {
    const n = shown.length;
    if (!n) return;
    if (e.key === "ArrowDown") { e.preventDefault(); setActive((a) => (a + 1) % n); }
    else if (e.key === "ArrowUp") { e.preventDefault(); setActive((a) => (a - 1 + n) % n); }
    else if (typing) { if (e.key === "Enter") { e.preventDefault(); choose(active); } }
    else if (e.key === "Home") { e.preventDefault(); setActive(0); }
    else if (e.key === "End") { e.preventDefault(); setActive(n - 1); }
    else if (e.key === "Enter" || e.key === " ") { e.preventDefault(); choose(active); }
    else if (searchable && e.key.length === 1 && /\S/.test(e.key)) {
      // typing in the list goes to the search field
      e.preventDefault();
      setQ(e.key); setActive(0); searchRef.current?.focus();
    }
    else if (e.key.length === 1 && /\S/.test(e.key)) {
      const ch = e.key.toLowerCase();
      const start = active + 1;
      for (let k = 0; k < n; k++) {
        const j = (start + k) % n;
        const t = (shown[j]?.text ?? String(shown[j]?.value ?? "")).toLowerCase().replace(/^-+/, "");
        if (t.startsWith(ch)) { setActive(j); break; }
      }
    }
  };

  return (
    <Popover.Root open={open} onOpenChange={setOpen}>
      <Popover.Trigger asChild>
        <button
          id={id}
          type="button"
          aria-haspopup="listbox"
          aria-expanded={open}
          aria-label={ariaLabel}
          aria-invalid={invalid || undefined}
          onKeyDown={(e) => {
            if (!open && (e.key === "ArrowDown" || e.key === "ArrowUp")) { e.preventDefault(); setOpen(true); }
          }}
          className={cn("field flex items-center justify-between gap-2 text-left", className)}
        >
          <span className={cn("truncate", !selected && "text-faint")}>{selected ? selected.label : placeholder}</span>
          <ChevronDown className={cn("h-3.5 w-3.5 shrink-0 text-faint transition-transform", open && "rotate-180")} aria-hidden />
        </button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="start"
          sideOffset={4}
          collisionPadding={8}
          onOpenAutoFocus={(e) => { e.preventDefault(); (searchable ? searchRef.current : listRef.current)?.focus(); }}
          className="z-50 min-w-[var(--radix-popover-trigger-width)] max-w-[min(92vw,360px)] animate-pop-in rounded-md border border-line bg-surface p-1 shadow-pop"
        >
          {searchable && (
            <div className="relative mb-1">
              <Search className="pointer-events-none absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-faint" aria-hidden />
              <input
                ref={searchRef}
                type="text"
                value={q}
                onChange={(e) => { setQ(e.target.value); setActive(0); }}
                onKeyDown={(e) => onKey(e, true)}
                placeholder="Search…"
                aria-label={`Search: ${ariaLabel ?? "options"}`}
                aria-controls={`${uid}-list`}
                aria-activedescendant={shown.length ? `${uid}-opt-${active}` : undefined}
                autoComplete="off"
                spellCheck={false}
                className="h-8 w-full rounded border border-line bg-bg pl-7 pr-2 text-sm text-fg outline-none placeholder:text-faint focus:border-accent"
              />
            </div>
          )}
          <ul
            id={`${uid}-list`}
            ref={listRef}
            role="listbox"
            tabIndex={-1}
            aria-label={ariaLabel}
            aria-activedescendant={`${uid}-opt-${active}`}
            onKeyDown={onKey}
            className="max-h-64 overflow-auto outline-none"
          >
            {!shown.length && <li className="px-2 py-1.5 text-sm text-faint">Nothing found</li>}
            {shown.map((o, i) => {
              const sel = o.value === value;
              return (
                <li
                  key={o.value}
                  id={`${uid}-opt-${i}`}
                  data-idx={i}
                  role="option"
                  aria-selected={sel}
                  onMouseMove={() => setActive(i)}
                  onClick={() => choose(i)}
                  className={cn(
                    "flex cursor-pointer items-start gap-2 rounded px-2 py-1.5 text-sm",
                    i === active ? "bg-sunken text-fg" : "text-fg/90",
                  )}
                >
                  <Check className={cn("mt-0.5 h-3.5 w-3.5 shrink-0 text-accent", !sel && "invisible")} aria-hidden />
                  <span className="min-w-0">
                    <span className="block truncate">{o.label}</span>
                    {o.hint && <span className="block text-xs text-muted">{o.hint}</span>}
                  </span>
                </li>
              );
            })}
          </ul>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  );
}
