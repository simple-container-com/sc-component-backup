import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";
import { Loader2 } from "lucide-react";
import { cn } from "@/lib/cn";

type Variant = "primary" | "secondary" | "ghost" | "danger" | "danger-ghost";
type Size = "sm" | "md" | "icon" | "icon-sm";

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  size?: Size;
  loading?: boolean;
  icon?: ReactNode;
}

const variants: Record<Variant, string> = {
  primary:
    "bg-accent text-accent-fg hover:bg-accent/90 active:bg-accent/85 shadow-[inset_0_1px_0_rgb(255_255_255/0.12)] disabled:bg-accent/50",
  secondary:
    "bg-surface text-fg border border-line-strong/80 hover:bg-sunken hover:border-line-strong active:bg-line/60 disabled:text-faint",
  ghost: "text-muted hover:text-fg hover:bg-sunken active:bg-line/60 disabled:text-faint",
  danger: "bg-danger text-white hover:bg-danger/90 dark:text-[#1d0b08] disabled:bg-danger/50",
  "danger-ghost": "text-danger hover:bg-danger-soft",
};

const sizes: Record<Size, string> = {
  sm: "h-7 px-2.5 text-xs gap-1.5 rounded",
  md: "h-9 px-3.5 text-sm gap-2 rounded",
  icon: "h-9 w-9 justify-center rounded",
  "icon-sm": "h-7 w-7 justify-center rounded",
};

export function buttonClass(variant: Variant = "secondary", size: Size = "md", className?: string) {
  return cn(
    "inline-flex shrink-0 select-none items-center whitespace-nowrap font-medium transition-colors duration-100 disabled:cursor-not-allowed",
    variants[variant],
    sizes[size],
    className,
  );
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = "secondary", size = "md", loading, icon, className, children, disabled, type = "button", ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      className={buttonClass(variant, size, className)}
      {...rest}
    >
      {loading ? <Loader2 className="h-[1.1em] w-[1.1em] animate-spin" aria-hidden /> : icon}
      {children}
    </button>
  );
});
