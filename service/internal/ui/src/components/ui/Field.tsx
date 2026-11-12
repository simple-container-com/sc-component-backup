import { forwardRef, useId, useState, type InputHTMLAttributes, type ReactNode } from "react";
import { Eye, EyeOff } from "lucide-react";
import { cn } from "@/lib/cn";

/** Label + control + hint/error, with the ids wired for a11y. */
export function Field({
  label, hint, error, children, htmlFor, className, optional, aside,
}: {
  label: ReactNode; hint?: ReactNode; error?: string | null; children: ReactNode; htmlFor?: string;
  className?: string; optional?: boolean; aside?: ReactNode;
}) {
  return (
    <div className={cn("min-w-0", className)}>
      <div className="mb-1.5 flex items-baseline justify-between gap-2">
        <label htmlFor={htmlFor} className="text-sm font-medium text-fg">
          {label}
          {optional && <span className="ml-1.5 font-normal text-faint">optional</span>}
        </label>
        {aside}
      </div>
      {children}
      {error ? (
        <p id={htmlFor ? `${htmlFor}-err` : undefined} role="alert" className="mt-1.5 text-xs text-danger">{error}</p>
      ) : hint ? (
        <p id={htmlFor ? `${htmlFor}-hint` : undefined} className="mt-1.5 text-xs text-muted">{hint}</p>
      ) : null}
    </div>
  );
}

export interface TextInputProps extends Omit<InputHTMLAttributes<HTMLInputElement>, "type"> {
  invalid?: boolean;
  type?: "text" | "password" | "search";
  leading?: ReactNode;
  trailing?: ReactNode;
}

export const TextInput = forwardRef<HTMLInputElement, TextInputProps>(function TextInput(
  { invalid, className, type = "text", leading, trailing, id, ...rest },
  ref,
) {
  const input = (
    <input
      ref={ref}
      id={id}
      type={type}
      aria-invalid={invalid || undefined}
      aria-describedby={id ? (invalid ? `${id}-err` : `${id}-hint`) : undefined}
      className={cn("field", leading && "pl-8", trailing && "pr-9", className)}
      {...rest}
    />
  );
  if (!leading && !trailing) return input;
  return (
    <div className="relative">
      {leading && <span className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-faint">{leading}</span>}
      {input}
      {trailing && <span className="absolute right-1 top-1/2 -translate-y-1/2">{trailing}</span>}
    </div>
  );
});

/** Masked input for connection strings. Value is never echoed back from the server. */
export const SecretInput = forwardRef<HTMLInputElement, Omit<TextInputProps, "type" | "trailing"> & { what?: string }>(function SecretInput({ what = "connection string", ...props }, ref) {
  const [shown, setShown] = useState(false);
  return (
    <TextInput
      ref={ref}
      {...props}
      type={shown ? "text" : "password"}
      autoComplete="off"
      autoCapitalize="off"
      autoCorrect="off"
      spellCheck={false}
      data-1p-ignore
      data-lpignore="true"
      className={cn("font-mono text-[12.5px]", props.className)}
      trailing={
        <button
          type="button"
          onClick={() => setShown((s) => !s)}
          className="flex h-7 w-7 items-center justify-center rounded text-faint hover:bg-sunken hover:text-fg"
          aria-label={shown ? `Hide ${what}` : `Show ${what}`}
          aria-pressed={shown}
        >
          {shown ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
        </button>
      }
    />
  );
});

export function useFieldId(prefix: string) {
  return `${prefix}-${useId().replace(/:/g, "")}`;
}
