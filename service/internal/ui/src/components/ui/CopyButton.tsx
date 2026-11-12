import { Check, Copy } from "lucide-react";
import { useState } from "react";
import { copyText } from "@/lib/clipboard";
import { Button } from "./Button";
import { Tooltip } from "./Tooltip";
import { useToast } from "./Toast";

export function CopyButton({ text, label = "Copy", tooltip, size = "icon-sm", children }: { text: string; label?: string; tooltip?: string; size?: "icon-sm" | "sm"; children?: React.ReactNode }) {
  const [done, setDone] = useState(false);
  const { toast } = useToast();
  const onClick = async () => {
    const ok = await copyText(text);
    if (ok) {
      setDone(true);
      window.setTimeout(() => setDone(false), 1400);
    } else {
      toast({ kind: "error", title: "Could not copy", detail: "The browser blocked clipboard access." });
    }
  };
  const icon = done ? <Check className="h-3.5 w-3.5 text-ok" /> : <Copy className="h-3.5 w-3.5" />;
  const btn = (
    <Button variant="ghost" size={size} onClick={onClick} aria-label={label} icon={icon}>
      {children}
    </Button>
  );
  return tooltip ? <Tooltip content={done ? "Copied" : tooltip}>{btn}</Tooltip> : btn;
}
