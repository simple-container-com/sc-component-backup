import * as T from "@radix-ui/react-tooltip";
import type { ReactNode } from "react";

export const TooltipProvider = T.Provider;

export function Tooltip({
  content,
  children,
  side = "top",
  asChild = true,
}: {
  content: ReactNode;
  children: ReactNode;
  side?: "top" | "bottom" | "left" | "right";
  asChild?: boolean;
}) {
  if (!content) return <>{children}</>;
  return (
    <T.Root>
      <T.Trigger asChild={asChild}>{children}</T.Trigger>
      <T.Portal>
        <T.Content
          side={side}
          sideOffset={6}
          collisionPadding={8}
          className="z-50 max-w-xs animate-fade-in rounded bg-fg px-2 py-1 text-xs text-bg shadow-pop"
        >
          {content}
          <T.Arrow className="fill-fg" width={8} height={4} />
        </T.Content>
      </T.Portal>
    </T.Root>
  );
}
