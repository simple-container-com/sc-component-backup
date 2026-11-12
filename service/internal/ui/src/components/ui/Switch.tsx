import { cn } from "@/lib/cn";

export function Switch({ checked, onChange, id, label, disabled }: { checked: boolean; onChange: (v: boolean) => void; id?: string; label?: string; disabled?: boolean }) {
  return (
    <button
      id={id}
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cn(
        "relative inline-flex h-5 w-9 shrink-0 items-center rounded-full border transition-colors disabled:opacity-50",
        checked ? "border-accent bg-accent" : "border-line-strong bg-sunken",
      )}
    >
      <span
        className={cn(
          "inline-block h-3.5 w-3.5 rounded-full bg-white shadow transition-transform dark:bg-fg",
          checked ? "translate-x-[18px] dark:bg-accent-fg" : "translate-x-[2px]",
        )}
      />
    </button>
  );
}

export function Checkbox({ checked, indeterminate }: { checked: boolean; indeterminate?: boolean }) {
  return (
    <span
      aria-hidden
      className={cn(
        "inline-flex h-4 w-4 shrink-0 items-center justify-center rounded-[4px] border transition-colors",
        checked || indeterminate ? "border-accent bg-accent text-accent-fg" : "border-line-strong bg-surface",
      )}
    >
      {indeterminate ? (
        <svg viewBox="0 0 12 12" className="h-3 w-3"><path d="M3 6h6" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" /></svg>
      ) : checked ? (
        <svg viewBox="0 0 12 12" className="h-3 w-3"><path d="M2.5 6.2 5 8.5l4.5-5" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" /></svg>
      ) : null}
    </span>
  );
}
