import { useRef, type KeyboardEvent, type ReactNode } from "react";
import { cn } from "@/lib/cn";

export interface SegOption<T extends string> { value: T; label: ReactNode; icon?: ReactNode; disabled?: boolean }

/** Radio-group styled as a segmented control; arrow keys move selection. */
export function Segmented<T extends string>({
  value, onChange, options, ariaLabel, size = "md", className,
}: {
  value: T; onChange: (v: T) => void; options: SegOption<T>[]; ariaLabel: string; size?: "sm" | "md"; className?: string;
}) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const onKey = (e: KeyboardEvent, i: number) => {
    const dir = e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : 0;
    if (!dir) return;
    e.preventDefault();
    const n = options.length;
    for (let k = 1; k <= n; k++) {
      const j = (i + dir * k + n) % n;
      const o = options[j];
      if (o && !o.disabled) {
        onChange(o.value);
        refs.current[j]?.focus();
        break;
      }
    }
  };
  return (
    <div role="radiogroup" aria-label={ariaLabel} className={cn("inline-flex rounded-md border border-line bg-sunken p-0.5", className)}>
      {options.map((o, i) => {
        const on = o.value === value;
        return (
          <button
            key={o.value}
            ref={(el) => { refs.current[i] = el; }}
            type="button"
            role="radio"
            aria-checked={on}
            tabIndex={on ? 0 : -1}
            disabled={o.disabled}
            onClick={() => onChange(o.value)}
            onKeyDown={(e) => onKey(e, i)}
            className={cn(
              "inline-flex items-center justify-center gap-1.5 rounded-[5px] font-medium transition-colors",
              size === "sm" ? "h-6 px-2 text-xs" : "h-8 px-3 text-sm",
              on ? "bg-surface text-fg shadow-[0_1px_2px_rgb(0_0_0/0.08)] ring-1 ring-line" : "text-muted hover:text-fg",
              o.disabled && "opacity-40",
            )}
          >
            {o.icon}
            {o.label}
          </button>
        );
      })}
    </div>
  );
}
