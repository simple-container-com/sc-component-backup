import { formatDateTime } from "@/lib/format";
import * as D from "@radix-ui/react-dialog";
import * as DM from "@radix-ui/react-dropdown-menu";
import { Check, History, LayoutGrid, LogOut, Menu, Monitor, Moon, Plus, Settings2, Sun, X } from "lucide-react";
import { useEffect, useState } from "react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useLogout, useMeta, useOverview } from "@/api/queries";
import { cn } from "@/lib/cn";
import { useTheme, type ThemeMode } from "@/lib/theme";
import { Dot } from "../ui/Badge";
import { STATUS, statusOf } from "../Status";
import { EngineMark } from "../Engine";
import { Tooltip } from "../ui/Tooltip";
import { useToast } from "../ui/Toast";
import { ErrorBoundary } from "../ErrorBoundary";

export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" className={className} aria-hidden>
      <rect width="32" height="32" rx="7" className="fill-accent" />
      <g fill="none" strokeWidth="2.2" strokeLinecap="round" className="stroke-accent-fg">
        <ellipse cx="16" cy="9.5" rx="8" ry="3" />
        <path d="M8 9.5v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6M8 15.5v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6" />
      </g>
    </svg>
  );
}

const navCls = ({ isActive }: { isActive: boolean }) =>
  cn(
    "flex h-8 items-center gap-2.5 rounded px-2 text-sm transition-colors",
    isActive ? "bg-surface text-fg font-medium shadow-[0_0_0_1px_rgb(var(--line))]" : "text-muted hover:bg-line/40 hover:text-fg",
  );

function Sidebar({ onNavigate }: { onNavigate?: () => void }) {
  const { data } = useOverview();
  const meta = useMeta();
  const projects = data?.projects ?? [];
  const storageBad = data && !data.storage.ok;
  return (
    <div className="flex h-full flex-col">
      <div className="flex h-14 items-center gap-2.5 px-4">
        <Logo className="h-6 w-6" />
        <span className="text-[15px] font-semibold tracking-tight">db-backup</span>
        {meta.data && (
          <span className="ml-auto font-mono text-2xs text-faint" title={meta.data.build ? `commit ${meta.data.build.commit}${meta.data.build.deployed_at ? `, deployed ${formatDateTime(meta.data.build.deployed_at)}` : ""}` : undefined}>
            v{meta.data.version}{meta.data.build && <span className="text-faint/70"> · {meta.data.build.commit}</span>}
          </span>
        )}
      </div>
      <nav aria-label="Main navigation" className="flex min-h-0 flex-1 flex-col gap-0.5 px-2">
        <NavLink to="/" end className={navCls} onClick={onNavigate}>
          <LayoutGrid className="h-4 w-4" aria-hidden /> Overview
          {storageBad && (
            <Tooltip content="Storage unavailable">
              <span className="ml-auto"><Dot tone="danger" /></span>
            </Tooltip>
          )}
        </NavLink>
        <NavLink to="/history" className={navCls} onClick={onNavigate}>
          <History className="h-4 w-4" aria-hidden /> History
        </NavLink>
        <NavLink to="/settings" className={navCls} onClick={onNavigate}>
          <Settings2 className="h-4 w-4" aria-hidden /> Settings
        </NavLink>

        <div className="mb-1 mt-5 flex items-center justify-between px-2">
          <span className="text-xs font-medium text-faint">Projects</span>
          <Tooltip content="Add project">
            <NavLink to="/new" onClick={onNavigate} aria-label="Add project" className="rounded p-1 text-faint hover:bg-line/50 hover:text-fg">
              <Plus className="h-3.5 w-3.5" />
            </NavLink>
          </Tooltip>
        </div>
        <div className="-mx-2 min-h-0 flex-1 space-y-0.5 overflow-y-auto px-2 pb-3">
          {projects.map((p) => {
            const st = statusOf(p);
            return (
              <NavLink key={p.name} to={`/projects/${encodeURIComponent(p.name)}`} className={navCls} onClick={onNavigate}>
                <EngineMark engine={p.engine} size="sm" />
                <span className="min-w-0 flex-1 truncate">{p.name}</span>
                <Tooltip content={STATUS[st].label}>
                  <span><Dot tone={STATUS[st].tone} pulse={st === "running"} /></span>
                </Tooltip>
              </NavLink>
            );
          })}
          {data && projects.length === 0 && <p className="px-2 py-1 text-xs text-faint">No projects yet</p>}
        </div>
      </nav>
      <div className="flex items-center gap-1 border-t border-line px-2 py-2">
        <ThemeMenu />
        <LogoutButton />
      </div>
    </div>
  );
}

function ThemeMenu() {
  const { mode, setMode, resolved } = useTheme();
  const items: { v: ThemeMode; label: string; icon: JSX.Element }[] = [
    { v: "system", label: "System", icon: <Monitor className="h-4 w-4" /> },
    { v: "light", label: "Light", icon: <Sun className="h-4 w-4" /> },
    { v: "dark", label: "Dark", icon: <Moon className="h-4 w-4" /> },
  ];
  return (
    <DM.Root>
      <DM.Trigger className="flex h-8 flex-1 items-center gap-2 rounded px-2 text-sm text-muted hover:bg-line/40 hover:text-fg" aria-label="Color theme">
        {resolved === "dark" ? <Moon className="h-4 w-4" /> : <Sun className="h-4 w-4" />}
        Theme
      </DM.Trigger>
      <DM.Portal>
        <DM.Content side="top" align="start" sideOffset={6} className="z-50 min-w-[180px] animate-pop-in rounded-md border border-line bg-surface p-1 shadow-pop">
          <DM.RadioGroup value={mode} onValueChange={(v) => setMode(v as ThemeMode)}>
            {items.map((it) => (
              <DM.RadioItem
                key={it.v}
                value={it.v}
                className="flex cursor-pointer items-center gap-2 rounded px-2 py-1.5 text-sm outline-none data-[highlighted]:bg-sunken"
              >
                <span className="text-muted">{it.icon}</span>
                {it.label}
                <DM.ItemIndicator className="ml-auto"><Check className="h-3.5 w-3.5 text-accent" /></DM.ItemIndicator>
              </DM.RadioItem>
            ))}
          </DM.RadioGroup>
        </DM.Content>
      </DM.Portal>
    </DM.Root>
  );
}

function LogoutButton() {
  const logout = useLogout();
  const nav = useNavigate();
  const { error } = useToast();
  return (
    <Tooltip content="Sign out">
      <button
        type="button"
        aria-label="Sign out"
        onClick={() =>
          logout.mutate(undefined, {
            onSuccess: () => nav("/login", { replace: true }),
            onError: (e) => error(e, "Could not sign out"),
          })
        }
        className="flex h-8 w-8 items-center justify-center rounded text-muted hover:bg-line/40 hover:text-fg"
      >
        <LogOut className="h-4 w-4" />
      </button>
    </Tooltip>
  );
}

export function AppShell() {
  const [open, setOpen] = useState(false);
  const loc = useLocation();
  useEffect(() => setOpen(false), [loc.pathname]);
  return (
    <div className="min-h-screen lg:grid lg:grid-cols-[232px_1fr]">
      <a href="#main" className="sr-only focus:not-sr-only focus:fixed focus:left-2 focus:top-2 focus:z-50 focus:rounded focus:bg-surface focus:px-3 focus:py-2">
        Skip to content
      </a>
      <aside className="sticky top-0 hidden h-screen border-r border-line bg-sunken/60 lg:block">
        <Sidebar />
      </aside>

      <header className="sticky top-0 z-30 flex h-12 items-center gap-2 border-b border-line bg-bg/90 px-3 backdrop-blur lg:hidden">
        <D.Root open={open} onOpenChange={setOpen}>
          <D.Trigger className="rounded p-2 text-muted hover:bg-sunken hover:text-fg" aria-label="Open menu">
            <Menu className="h-5 w-5" />
          </D.Trigger>
          <D.Portal>
            <D.Overlay className="fixed inset-0 z-40 animate-fade-in bg-[rgb(10_14_20/0.45)]" />
            <D.Content className="fixed inset-y-0 left-0 z-50 w-[272px] max-w-[85vw] animate-fade-in border-r border-line bg-bg shadow-pop focus:outline-none">
              <D.Title className="sr-only">Menu</D.Title>
              <D.Description className="sr-only">Navigation across sections and projects</D.Description>
              <D.Close className="absolute right-2 top-3 rounded p-1.5 text-faint hover:bg-sunken" aria-label="Close menu">
                <X className="h-4 w-4" />
              </D.Close>
              <Sidebar onNavigate={() => setOpen(false)} />
            </D.Content>
          </D.Portal>
        </D.Root>
        <Logo className="h-5 w-5" />
        <span className="font-semibold tracking-tight">db-backup</span>
      </header>

      <main id="main" className="min-w-0">
        <ErrorBoundary resetKey={loc.pathname}>
          <Outlet />
        </ErrorBoundary>
      </main>
    </div>
  );
}

/** Page container + header used by every screen. */
export function Page({ title, actions, children, meta, back, wide }: { title: React.ReactNode; actions?: React.ReactNode; children: React.ReactNode; meta?: React.ReactNode; back?: React.ReactNode; wide?: boolean }) {
  return (
    // Every page shares one centred container, so the left edge never jumps between screens;
    // narrow (form) pages only cap their own width inside it.
    <div className="mx-auto w-full max-w-[1200px] px-4 pb-24 pt-5 sm:px-6 lg:px-8 lg:pt-8">
      <div className={cn(!wide && "max-w-[960px]")}>
      {back}
      <div className="mb-6 flex flex-wrap items-start justify-between gap-x-4 gap-y-3">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold tracking-[-0.015em]">{title}</h1>
          {meta && <div className="mt-1.5 text-sm text-muted">{meta}</div>}
        </div>
        {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
      </div>
      {children}
      </div>
    </div>
  );
}
