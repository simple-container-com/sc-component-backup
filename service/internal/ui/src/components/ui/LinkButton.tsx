import type { ReactNode } from "react";
import { Link, type LinkProps } from "react-router-dom";
import { buttonClass } from "./Button";

export function LinkButton({ variant = "secondary", size = "md", icon, children, className, ...rest }: LinkProps & { variant?: "primary" | "secondary" | "ghost"; size?: "sm" | "md"; icon?: ReactNode }) {
  return (
    <Link className={buttonClass(variant, size, className)} {...rest}>
      {icon}
      {children}
    </Link>
  );
}
