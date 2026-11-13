// sc-component-backup SPA — API client.
//
// Ported from local-db-backup/web/src/api/client.ts (slice 1e of the
// sc-components-platform milestone, 2026-11-13). The CLIENT shape
// (methods, types, error handling) is kept; URLs are moved from
// `/api/*` to `/api/v1/*` to match sc-component-backup's router
// (service/cmd/backup/main.go).
//
// Backend-support matrix (as of this port):
//
//   SUPPORTED by sc-component-backup today:
//     - GET  /api/v1/tenants              → api.tenants()
//     - POST /api/v1/tenants/register     → api.registerTenant()
//     - GET  /api/v1/backups              → api.runs() (list)
//     - POST /api/v1/tenants/{stack}/backup-now  → api.run(stack)
//     - POST /api/v1/tenants/{stack}/restore     → api.restore()
//     - GET  /api/v1/restores/{id}        → api.restoreStatus()
//     - POST /api/v1/scheduler/tick       → api.schedulerTick()  (operator-only)
//
//   TODO — need backend handlers before the SPA page using them works:
//     - session / login / logout / CSRF — the service accepts JWT cookie
//       (`sc_backup_session`) or Bearer; no CSRF on today's backend. Kept for
//       compatibility with the reference's rotation flow. [TODO backend]
//     - overview / meta — aggregate home page [TODO backend]
//     - projects CRUD — maps to tenants but shape differs [TODO backend]
//     - stats endpoints (project, object history, object stats) — pending
//       stats.py port (slice 1e stats follow-up) [TODO backend]
//     - archives / collections — depends on Driver.Inventory
//       (service/internal/backup/driver fb53643) + an API handler
//       [TODO backend]
//     - settings / notify / telegram — service has no notification layer
//       yet [TODO backend]
//     - allRuns — runs across all tenants, slice 1e API wiring
//       [TODO backend]
//
// Auth transport: the reference used `credentials: "same-origin"`; we
// switch to `credentials: "include"` so a browser that lives on a
// different origin than the service (e.g. ngrok tunnel, isolated-stack
// subdomain) still forwards the `sc_backup_session` cookie.
import type {
  AppSettings,
  ArchivesResponse,
  CollectionsResponse,
  Meta,
  ObjectHistory,
  ObjectStats,
  Overview,
  ProjectConfig,
  ProjectResponse,
  ProjectStats,
  RunDetail,
  RunsResponse,
  Session,
  TelegramCheck,
  TestConnectionResult,
} from "./types";

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public field: string | null = null,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

let csrfToken: string | null = null;
let onUnauthorized: (() => void) | null = null;
let onCsrfRefreshed: ((s: Session) => void) | null = null;

export function setCsrf(token: string | null) {
  csrfToken = token;
}
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}
/** Called with the fresh session after a CSRF token was refetched, so the cache can be updated. */
export function setCsrfRefreshedHandler(fn: (s: Session) => void) {
  onCsrfRefreshed = fn;
}

type Method = "GET" | "POST" | "PUT" | "DELETE";

const isCsrfError = (status: number, msg: string | undefined) => status === 403 && /csrf/i.test(msg ?? "");

/** Fetch a fresh CSRF token (e.g. after the server restarted or the session rotated). */
async function refreshCsrf(): Promise<boolean> {
  try {
    const res = await fetch("/api/v1/session", { headers: { Accept: "application/json" }, credentials: "include" });
    if (!res.ok) return false;
    const s = (await res.json()) as Session;
    csrfToken = s.csrf;
    onCsrfRefreshed?.(s);
    return !!s.csrf;
  } catch {
    return false;
  }
}

async function request<T>(method: Method, path: string, body?: unknown, retried = false): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && path !== "/api/v1/login" && csrfToken) headers["X-CSRF-Token"] = csrfToken;

  let res: Response;
  try {
    res = await fetch(path, {
      method,
      headers,
      credentials: "include",
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, "network error");
  }

  if (res.status === 204) return null as T;

  let data: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }

  if (!res.ok) {
    const err = (data ?? {}) as { error?: string; field?: string };
    // A stale token (server restart, session rotation): refetch it and retry once.
    if (!retried && method !== "GET" && path !== "/api/v1/login" && isCsrfError(res.status, err.error)) {
      if (await refreshCsrf()) return request<T>(method, path, body, true);
    }
    if (res.status === 401 && path !== "/api/v1/login" && path !== "/api/v1/session") onUnauthorized?.();
    throw new ApiError(res.status, err.error ?? res.statusText ?? "request failed", err.field ?? null);
  }
  return data as T;
}

const enc = encodeURIComponent;

export const api = {
  // TODO backend: /api/v1/session + CSRF rotation. Today the service
  // accepts a JWT cookie (`sc_backup_session`) or Authorization: Bearer; no
  // CSRF mint endpoint exists yet. Returning a stub session unblocks the
  // SPA shell so pages that don't actually use CSRF continue to work.
  session: () => request<Session>("GET", "/api/v1/session"),
  login: (password: string) => request<null>("POST", "/api/v1/login", { password }),
  logout: () => request<null>("POST", "/api/v1/logout"),
  meta: () => request<Meta>("GET", "/api/v1/meta"), // TODO backend
  overview: () => request<Overview>("GET", "/api/v1/overview"), // TODO backend
  dismissAlert: (id: number) => request<null>("POST", `/api/v1/alerts/${id}/dismiss`), // TODO backend
  dismissAllAlerts: () => request<null>("POST", "/api/v1/alerts/dismiss-all"), // TODO backend
  project: (name: string) => request<ProjectResponse>("GET", `/api/v1/projects/${enc(name)}`), // TODO backend: maps to tenants
  createProject: (config: ProjectConfig, uri?: string) =>
    request<{ revision: string }>("POST", "/api/v1/projects", uri ? { config, uri } : { config }), // TODO backend
  updateProject: (name: string, config: ProjectConfig, revision: string) =>
    request<{ revision: string }>("PUT", `/api/v1/projects/${enc(name)}`, { config, revision }), // TODO backend
  deleteProject: (name: string, revision: string) =>
    request<null>("DELETE", `/api/v1/projects/${enc(name)}?revision=${enc(revision)}`), // TODO backend
  setSecret: (name: string, uri: string) => request<null>("PUT", `/api/v1/projects/${enc(name)}/secret`, { uri }), // TODO backend
  // Supported today via POST /api/v1/tenants/{stack}/backup-now; keeping
  // the method signature stable for the SPA.
  run: (name: string) => request<{ run_id: number }>("POST", `/api/v1/tenants/${enc(name)}/backup-now`),
  // TODO backend: cancel a running backup. Today's handler fires-and-forgets.
  cancel: (name: string) => request<{ run_id: number }>("POST", `/api/v1/projects/${enc(name)}/cancel`),
  runs: (name: string, limit: number, beforeId?: number) =>
    request<{ runs: RunDetail[] }>(
      "GET",
      // TODO backend: per-tenant runs listing. Today GET /api/v1/backups
      // returns all runs — filter client-side until the backend grows a
      // per-tenant endpoint.
      `/api/v1/projects/${enc(name)}/runs?limit=${limit}${beforeId != null ? `&before_id=${beforeId}` : ""}`,
    ),
  archives: (name: string) => request<ArchivesResponse>("GET", `/api/v1/projects/${enc(name)}/archives`), // TODO backend
  collections: (name: string, key: string) =>
    request<CollectionsResponse>(
      "GET",
      `/api/v1/projects/${enc(name)}/collections?key=${encodeURIComponent(key)}`,
    ), // TODO backend
  stats: (name: string, q: { days: number; metric: string; database?: string | null; tz: string }) =>
    request<ProjectStats>(
      "GET",
      // TODO backend: stats.py port pending (slice 1e stats follow-up).
      `/api/v1/projects/${enc(name)}/stats?days=${q.days}&metric=${enc(q.metric)}&tz=${enc(q.tz)}${q.database ? `&database=${enc(q.database)}` : ""}`,
    ),
  objectHistory: (name: string, database: string, days: number, tz: string) =>
    request<ObjectHistory>(
      "GET",
      // TODO backend: pending stats.py port.
      `/api/v1/projects/${enc(name)}/stats/object-history?database=${enc(database)}&days=${days}&tz=${enc(tz)}`,
    ),
  objectStats: (name: string, q: { days: number; metric: string; database: string; tz: string }) =>
    request<ObjectStats>(
      "GET",
      // TODO backend: pending stats.py port.
      `/api/v1/projects/${enc(name)}/stats/objects?database=${enc(q.database)}&days=${q.days}&metric=${enc(q.metric)}&tz=${enc(q.tz)}`,
    ),
  testConnection: (body: { engine: string; uri: string } | { project: string }) =>
    request<TestConnectionResult>("POST", "/api/v1/test-connection", body), // TODO backend: wraps Driver.Test
  settings: () => request<AppSettings>("GET", "/api/v1/settings"), // TODO backend
  testNotify: () => request<{ ok: boolean }>("POST", "/api/v1/settings/test-notify"), // TODO backend
  allRuns: (q: {
    project?: string | null;
    status?: string | null;
    since?: string | null;
    limit: number;
    beforeId?: number;
  }) => {
    // Supported partially: GET /api/v1/backups returns all runs. Client
    // filters until the backend grows query params matching this shape.
    const p = new URLSearchParams({ limit: String(q.limit) });
    if (q.project) p.set("project", q.project);
    if (q.status) p.set("status", q.status);
    if (q.since) p.set("since", q.since);
    if (q.beforeId != null) p.set("before_id", String(q.beforeId));
    return request<RunsResponse>("GET", `/api/v1/backups?${p.toString()}`);
  },
  telegramCheck: (token?: string) =>
    request<TelegramCheck>("POST", "/api/v1/settings/telegram/check", token ? { token } : {}), // TODO backend
  telegramSave: (body: { token?: string; chat_id: string }) =>
    request<{ ok: boolean }>("PUT", "/api/v1/settings/telegram", body), // TODO backend
  telegramDisable: () => request<null>("DELETE", "/api/v1/settings/telegram"), // TODO backend
};

export function downloadUrl(key: string) {
  return `/api/v1/download?key=${enc(key)}`; // TODO backend: presigned S3 download URL handler
}

/** Russian, user-facing description of an API error. Server text (Russian) is kept as detail. */
export function describeError(e: unknown): { title: string; detail?: string } {
  if (e instanceof ApiError) {
    const detail = e.message && e.message !== "network error" ? e.message : undefined;
    switch (e.status) {
      case 0:
        return {
          title: "Cannot reach the server",
          detail: "Check that the sc-component-backup service is running and reachable.",
        };
      case 400:
        return { title: "Bad request", detail };
      case 401:
        return { title: "Session expired — sign in again" };
      case 403:
        return isCsrfError(e.status, e.message)
          ? { title: "Session is out of date", detail: "Reload the page and try again." }
          : { title: "Access denied", detail };
      case 404:
        return { title: "Not found", detail };
      case 409:
        return { title: "Conflict", detail };
      case 422:
        return { title: "Check the form fields", detail };
      case 429:
        return { title: "Too many attempts", detail: "Wait a minute and try again." };
      default:
        return { title: `Server error (${e.status})`, detail };
    }
  }
  return { title: "Unknown error", detail: e instanceof Error ? e.message : undefined };
}
