import { Check, CheckCircle2, MessageCircle, PowerOff, RefreshCw, Send } from "lucide-react";
import { useState, type ReactNode } from "react";
import { ApiError, describeError } from "@/api/client";
import { useTelegramCheck, useTelegramDisable, useTelegramSave, useTestNotify } from "@/api/queries";
import type { AppSettings, TelegramChat } from "@/api/types";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { SecretInput, TextInput } from "@/components/ui/Field";
import { useToast } from "@/components/ui/Toast";
import { cn } from "@/lib/cn";

const CHAT_TYPE: Record<string, string> = { private: "private chat", group: "group", supergroup: "group", channel: "channel" };

type Errors = { token?: string; chat_id?: string; other?: string };

/** Server error → the field it belongs to (422 carries `field`), else a general line. */
function toErrors(e: unknown, fallback: keyof Errors = "other"): Errors {
  const msg = e instanceof ApiError && e.status === 422 ? e.message : (() => { const d = describeError(e); return d.detail ? `${d.title}: ${d.detail}` : d.title; })();
  const field = e instanceof ApiError ? e.field : null;
  if (field === "token" || field === "chat_id") return { [field]: msg };
  return { [fallback]: msg };
}

function Step({ n, done, active, title, children }: { n: number; done: boolean; active: boolean; title: ReactNode; children?: ReactNode }) {
  return (
    <li className="grid grid-cols-[24px_minmax(0,1fr)] gap-x-3">
      <span
        className={cn(
          "mt-px flex h-6 w-6 items-center justify-center rounded-full border text-xs font-semibold tnum",
          done ? "border-ok/30 bg-ok-soft text-ok" : active ? "border-accent/30 bg-accent-soft text-accent" : "border-line bg-sunken text-faint",
        )}
        aria-hidden
      >
        {done ? <Check className="h-3.5 w-3.5" strokeWidth={2.5} /> : n}
      </span>
      <div className={cn("min-w-0 pb-5", !active && !done && "opacity-60")}>
        <p className="text-sm font-medium">{title}</p>
        {children && <div className="mt-2 space-y-2">{children}</div>}
      </div>
    </li>
  );
}

function ChatPicker({ chats, value, onChange }: { chats: TelegramChat[]; value: string | null; onChange: (id: string) => void }) {
  return (
    <div role="radiogroup" aria-label="Found chats" className="grid gap-2 sm:grid-cols-2">
      {chats.map((c) => {
        const on = c.id === value;
        return (
          <button
            key={c.id}
            type="button"
            role="radio"
            aria-checked={on}
            onClick={() => onChange(c.id)}
            className={cn(
              "flex min-w-0 items-start gap-2.5 rounded-md border px-3 py-2 text-left transition-colors",
              on ? "border-accent bg-accent-soft/60 ring-1 ring-accent/30" : "border-line bg-surface hover:bg-sunken",
            )}
          >
            <span className={cn("mt-1 flex h-3.5 w-3.5 shrink-0 items-center justify-center rounded-full border", on ? "border-accent" : "border-line-strong")} aria-hidden>
              {on && <span className="h-1.5 w-1.5 rounded-full bg-accent" />}
            </span>
            <span className="min-w-0">
              <span className="block truncate text-sm font-medium">{c.name || c.id}</span>
              <span className="block truncate text-xs text-muted">
                {CHAT_TYPE[c.type] ?? c.type}
                {c.username && <> · @{c.username}</>}
                <span className="text-faint"> · <span className="font-mono">{c.id}</span></span>
              </span>
            </span>
          </button>
        );
      })}
    </div>
  );
}

/**
 * Telegram setup without editing files: token → find the chat → save (the server sends a confirmation first).
 * `changeChat`: a token is stored already, only the chat is picked again.
 */
function Wizard({ changeChat, onDone, onCancel }: { changeChat: boolean; onDone: () => void; onCancel?: () => void }) {
  const check = useTelegramCheck();
  const save = useTelegramSave();
  const { success } = useToast();
  // The token lives only in this component's state: never stored in the browser, dropped after saving.
  const [token, setToken] = useState("");
  const [replaceToken, setReplaceToken] = useState(!changeChat);
  const [bot, setBot] = useState<{ username: string; name: string } | null>(null);
  const [chats, setChats] = useState<TelegramChat[] | null>(null);
  const [chatId, setChatId] = useState<string | null>(null);
  const [manual, setManual] = useState(false);
  const [manualId, setManualId] = useState("");
  const [errors, setErrors] = useState<Errors>({});
  const [searching, setSearching] = useState<"token" | "chats" | null>(null);

  const sentToken = replaceToken ? token.trim() || undefined : undefined;
  const tokenReady = !replaceToken || !!bot;
  const chosen = manual ? manualId.trim() : chatId;

  const run = (kind: "token" | "chats") => {
    if (replaceToken && !token.trim()) { setErrors({ token: "Paste the bot token" }); return; }
    setErrors({});
    setSearching(kind);
    check.mutate(sentToken, {
      onSuccess: (r) => {
        setBot(r.bot);
        if (kind === "chats" || r.chats.length) {
          setChats(r.chats);
          if (r.chats.length === 1 && !chatId) setChatId(r.chats[0]!.id);
        }
      },
      onError: (e) => {
        const er = toErrors(e, kind === "token" ? "token" : "other");
        if (kind === "token") setBot(null);
        // with the stored token a token error belongs to the "find chat" step
        setErrors(!replaceToken && er.token ? { other: er.token } : er);
      },
      onSettled: () => setSearching(null),
    });
  };

  const submit = () => {
    if (!chosen) { setErrors({ chat_id: "Choose a chat or enter its id" }); return; }
    setErrors({});
    save.mutate({ ...(sentToken ? { token: sentToken } : {}), chat_id: chosen }, {
      onSuccess: () => {
        setToken("");
        success("Telegram connected", "Done: a test message was sent to the chat");
        onDone();
      },
      onError: (e) => {
        const er = toErrors(e, "chat_id");
        setErrors(!replaceToken && er.token ? { chat_id: er.token } : er);
      },
    });
  };

  return (
    <div className="space-y-1">
      <ol>
        <Step
          n={1}
          done={tokenReady}
          active={!tokenReady}
          title={replaceToken ? <>Create a bot with <a href="https://t.me/BotFather" target="_blank" rel="noreferrer" className="text-accent hover:underline">@BotFather</a> (the /newbot command) and paste its token</> : "Bot"}
        >
          {replaceToken ? (
            <>
              <div className="flex flex-wrap items-start gap-2">
                <div className="min-w-0 flex-1 basis-64">
                  <SecretInput
                    id="tg-token"
                    what="token"
                    value={token}
                    onChange={(e) => { setToken(e.target.value); setBot(null); setChats(null); setChatId(null); }}
                    onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); run("token"); } }}
                    placeholder="123456789:AA…"
                    aria-label="Bot token"
                    invalid={!!errors.token}
                  />
                </div>
                <Button onClick={() => run("token")} loading={searching === "token"}>Check</Button>
              </div>
              {errors.token && <p id="tg-token-err" role="alert" className="text-xs text-danger">{errors.token}</p>}
              {bot && (
                <p className="flex items-center gap-1.5 text-sm text-ok">
                  <CheckCircle2 className="h-4 w-4" aria-hidden /> Bot @{bot.username} found{bot.name && <span className="text-muted">· {bot.name}</span>}
                </p>
              )}
              {changeChat && (
                <button type="button" className="text-xs text-muted underline underline-offset-2 hover:text-fg" onClick={() => { setReplaceToken(false); setToken(""); setBot(null); setErrors({}); }}>
                  Keep the saved token
                </button>
              )}
            </>
          ) : (
            <p className="flex flex-wrap items-center gap-2 text-sm text-muted">
              <Badge tone="ok">token set</Badge>
              {bot && <>bot @{bot.username}</>}
              <button type="button" className="text-xs underline underline-offset-2 hover:text-fg" onClick={() => { setReplaceToken(true); setBot(null); setChats(null); setChatId(null); setErrors({}); }}>
                replace token
              </button>
            </p>
          )}
        </Step>

        <Step n={2} done={!!chosen} active={tokenReady && !chosen} title={`Send the bot any message (or add it to a group) and click "Find chat"`}>
          {tokenReady && (
            <>
              <div className="flex flex-wrap items-center gap-3">
                <Button icon={<RefreshCw className="h-4 w-4" aria-hidden />} onClick={() => run("chats")} loading={searching === "chats"}>Find chat</Button>
                <button type="button" className="text-xs text-muted underline underline-offset-2 hover:text-fg" onClick={() => { setManual((m) => !m); setErrors({}); }}>
                  {manual ? "choose from found chats" : "enter manually"}
                </button>
              </div>
              {errors.other && <p role="alert" className="text-xs text-danger">{errors.other}</p>}
              {manual ? (
                <div className="max-w-xs">
                  <TextInput
                    id="tg-chat"
                    value={manualId}
                    onChange={(e) => setManualId(e.target.value)}
                    placeholder="-1001234567890 or @channel"
                    aria-label="Chat id"
                    autoComplete="off"
                    spellCheck={false}
                    className="font-mono text-[12.5px]"
                    invalid={!!errors.chat_id}
                  />
                  <p className="mt-1.5 text-xs text-muted">A number (negative for groups and channels) or a @channel name.</p>
                </div>
              ) : chats && (chats.length ? (
                <ChatPicker chats={chats} value={chatId} onChange={(id) => { setChatId(id); setErrors({}); }} />
              ) : (
                <p className="text-sm text-muted">No chats yet: send the bot /start and click again</p>
              ))}
            </>
          )}
        </Step>

        <Step n={3} done={false} active={!!chosen} title="Save: the bot will send a test message to the chat">
          {errors.chat_id && <p id="tg-chat-err" role="alert" className="text-xs text-danger">{errors.chat_id}</p>}
          <div className="flex flex-wrap gap-2">
            <Button variant="primary" icon={<Send className="h-4 w-4" aria-hidden />} disabled={!tokenReady || !chosen} loading={save.isPending} onClick={submit}>
              Save
            </Button>
            {onCancel && <Button variant="ghost" onClick={onCancel}>Cancel</Button>}
          </div>
        </Step>
      </ol>
    </div>
  );
}

export function TelegramCard({ telegram }: { telegram: AppSettings["notify"]["telegram"] }) {
  const [editing, setEditing] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const [justSaved, setJustSaved] = useState(false);
  const test = useTestNotify();
  const disable = useTelegramDisable();
  const { success, error } = useToast();

  if (!telegram || editing) {
    return (
      <div className="px-4 py-4">
        <div className="mb-3 flex items-center gap-2 text-sm font-medium">
          <MessageCircle className="h-4 w-4 text-faint" aria-hidden />
          {telegram ? "Telegram: change chat" : "Connect Telegram"}
        </div>
        <Wizard
          changeChat={!!telegram && telegram.token_set}
          onDone={() => { setEditing(false); setJustSaved(true); }}
          onCancel={telegram ? () => setEditing(false) : undefined}
        />
      </div>
    );
  }

  return (
    <div className="px-4 py-3">
      <div className="grid grid-cols-[140px_minmax(0,1fr)] items-baseline gap-3 text-sm">
        <span className="text-muted">Telegram</span>
        <span className="inline-flex flex-wrap items-center gap-2">
          chat <span className="font-mono">{telegram.chat_id}</span>
          {telegram.token_set ? <Badge tone="ok">token set</Badge> : <Badge tone="warn">no token</Badge>}
        </span>
      </div>
      {justSaved && (
        <p className="mt-2 flex items-center gap-1.5 text-sm text-ok sm:pl-[152px]">
          <CheckCircle2 className="h-4 w-4" aria-hidden /> Done: a test message was sent to the chat
        </p>
      )}
      <div className="mt-3 flex flex-wrap gap-2 sm:pl-[152px]">
        <Button
          size="sm"
          icon={<Send className="h-3.5 w-3.5" aria-hidden />}
          loading={test.isPending}
          onClick={() =>
            test.mutate(undefined, {
              onSuccess: (r) => (r.ok ? success("Test notification sent", "Check Telegram") : error(new Error("send failed"), "Notification not sent")),
              onError: (e) => error(e, "Notification not sent"),
            })
          }
        >
          Send test
        </Button>
        <Button size="sm" icon={<RefreshCw className="h-3.5 w-3.5" aria-hidden />} onClick={() => { setJustSaved(false); setEditing(true); }}>Change chat</Button>
        <Button size="sm" variant="danger-ghost" icon={<PowerOff className="h-3.5 w-3.5" aria-hidden />} onClick={() => setConfirm(true)}>Disable</Button>
      </div>
      <Dialog
        open={confirm}
        onOpenChange={setConfirm}
        title="Disable Telegram?"
        description="Failure notifications will no longer be sent to Telegram, and the saved bot token will be deleted. You can reconnect here at any time."
        footer={
          <>
            <Button onClick={() => setConfirm(false)}>Cancel</Button>
            <Button
              variant="danger"
              loading={disable.isPending}
              onClick={() =>
                disable.mutate(undefined, {
                  onSuccess: () => { setConfirm(false); setJustSaved(false); success("Telegram disabled"); },
                  onError: (e) => error(e, "Could not disable Telegram"),
                })
              }
            >
              Disable
            </Button>
          </>
        }
      />
    </div>
  );
}
