import { keepPreviousData, useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, setCsrf } from "./client";
import type { Overview, ProjectConfig } from "./types";

export const qk = {
  session: ["session"] as const,
  meta: ["meta"] as const,
  overview: ["overview"] as const,
  project: (n: string) => ["project", n] as const,
  runs: (n: string) => ["runs", n] as const,
  archives: (n: string) => ["archives", n] as const,
  settings: ["settings"] as const,
};

export function useSession() {
  return useQuery({
    queryKey: qk.session,
    queryFn: async () => {
      const s = await api.session();
      setCsrf(s.csrf);
      return s;
    },
    staleTime: 60_000,
    retry: 1,
  });
}

export function useMeta() {
  return useQuery({ queryKey: qk.meta, queryFn: api.meta, staleTime: Infinity });
}

function isBusy(o: Overview | undefined) {
  return !!o && (o.running.length > 0 || o.projects.some((p) => p.state !== "idle"));
}

export function useOverview() {
  return useQuery({
    queryKey: qk.overview,
    queryFn: api.overview,
    refetchInterval: (q) => (isBusy(q.state.data) ? 2000 : 5000),
  });
}

export function useProject(name: string) {
  return useQuery({ queryKey: qk.project(name), queryFn: () => api.project(name), refetchOnWindowFocus: false });
}

export const RUNS_PAGE = 25;

export function useRuns(name: string, live: boolean) {
  return useInfiniteQuery({
    queryKey: qk.runs(name),
    queryFn: ({ pageParam }) => api.runs(name, RUNS_PAGE, pageParam),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) =>
      last.runs.length < RUNS_PAGE ? undefined : last.runs[last.runs.length - 1]?.id,
    refetchInterval: live ? 3000 : 30_000,
  });
}

export function useArchives(name: string) {
  return useQuery({ queryKey: qk.archives(name), queryFn: () => api.archives(name), refetchInterval: 30_000 });
}

/** Collections recorded for one archive; fetched when the row is opened, never changes afterwards. */
export function useCollections(name: string, key: string, enabled: boolean) {
  return useQuery({ queryKey: ["collections", name, key], queryFn: () => api.collections(name, key), enabled, staleTime: Infinity });
}

/** The browser's IANA zone: the server cuts days at local midnight. */
export const browserTz = () => {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"; } catch { return "UTC"; }
};

/** Statistics change only when a run finishes: a slow refresh, previous data kept while the filters change. */
export function useStats(name: string, q: { days: number; metric: string; database: string | null }, enabled = true) {
  const tz = browserTz();
  return useQuery({
    queryKey: ["stats", name, q.days, q.metric, q.database, tz],
    queryFn: () => api.stats(name, { ...q, tz }),
    placeholderData: keepPreviousData,
    refetchInterval: 60_000,
    enabled,
  });
}

/** 30 days of every collection of one database, for the hover chart of a collection row. */
export function useObjectHistory(name: string, database: string | null | undefined, enabled: boolean) {
  const tz = browserTz();
  return useQuery({
    queryKey: ["object-history", name, database, tz],
    queryFn: () => api.objectHistory(name, database!, 30, tz),
    enabled: enabled && !!database,
    staleTime: 5 * 60_000,
  });
}

export function useObjectStats(name: string, q: { days: number; metric: string; database: string }, enabled = true) {
  const tz = browserTz();
  return useQuery({
    queryKey: ["stats-objects", name, q.database, q.days, q.metric, tz],
    queryFn: () => api.objectStats(name, { ...q, tz }),
    placeholderData: keepPreviousData,
    refetchInterval: 60_000,
    enabled,
  });
}

export function useSettings() {
  return useQuery({ queryKey: qk.settings, queryFn: api.settings });
}

export function useRunNow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.run(name),
    onMutate: async (name) => {
      await qc.cancelQueries({ queryKey: qk.overview });
      const prev = qc.getQueryData<Overview>(qk.overview);
      if (prev) {
        qc.setQueryData<Overview>(qk.overview, {
          ...prev,
          projects: prev.projects.map((p) => (p.name === name && p.state === "idle" ? { ...p, state: "queued" } : p)),
        });
      }
      return { prev };
    },
    onError: (_e, _name, ctx) => {
      if (ctx?.prev) qc.setQueryData(qk.overview, ctx.prev);
    },
    onSettled: (_d, _e, name) => {
      void qc.invalidateQueries({ queryKey: qk.overview });
      void qc.invalidateQueries({ queryKey: qk.runs(name) });
    },
  });
}

/** Hide alerts from the overview feed; the list updates at once and rolls back on error. */
function useAlertsMutation<V>(fn: (v: V) => Promise<unknown>, keep: (a: Overview["alerts"][number], v: V) => boolean) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onMutate: async (v: V) => {
      await qc.cancelQueries({ queryKey: qk.overview });
      const prev = qc.getQueryData<Overview>(qk.overview);
      if (prev) qc.setQueryData<Overview>(qk.overview, { ...prev, alerts: prev.alerts.filter((a) => keep(a, v)) });
      return { prev };
    },
    onError: (_e, _v, ctx) => {
      if (ctx?.prev) qc.setQueryData(qk.overview, ctx.prev);
    },
    onSettled: () => void qc.invalidateQueries({ queryKey: qk.overview }),
  });
}

export function useDismissAlert() {
  return useAlertsMutation((id: number) => api.dismissAlert(id), (a, id) => a.id !== id);
}

export function useDismissAllAlerts() {
  return useAlertsMutation((_: void) => api.dismissAllAlerts(), () => false);
}

export function useCreateProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ config, uri }: { config: ProjectConfig; uri?: string }) => api.createProject(config, uri),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.overview }),
  });
}

export function useUpdateProject(name: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ config, revision }: { config: ProjectConfig; revision: string }) =>
      api.updateProject(name, config, revision),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: qk.project(name) });
      void qc.invalidateQueries({ queryKey: qk.overview });
    },
  });
}

export function useDeleteProject(name: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (revision: string) => api.deleteProject(name, revision),
    onSuccess: () => {
      qc.removeQueries({ queryKey: qk.project(name) });
      void qc.invalidateQueries({ queryKey: qk.overview });
    },
  });
}

export function useSetSecret(name: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (uri: string) => api.setSecret(name, uri),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: qk.project(name) });
      void qc.invalidateQueries({ queryKey: qk.overview });
    },
  });
}

export function useTestConnection() {
  return useMutation({ mutationFn: api.testConnection });
}

export function useTestNotify() {
  return useMutation({ mutationFn: api.testNotify });
}

export const ALL_RUNS_PAGE = 50;

/** Global history across projects; `since` is fixed by the caller so the key stays stable between refetches. */
export function useAllRuns(q: { project: string | null; status: string | null; since: string | null }) {
  return useInfiniteQuery({
    queryKey: ["all-runs", q.project, q.status, q.since],
    queryFn: ({ pageParam }) => api.allRuns({ ...q, limit: ALL_RUNS_PAGE, beforeId: pageParam }),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) =>
      last.runs.length < ALL_RUNS_PAGE ? undefined : last.runs[last.runs.length - 1]?.id,
    placeholderData: keepPreviousData,
    refetchInterval: 15_000,
  });
}

export function useTelegramCheck() {
  return useMutation({ mutationFn: (token?: string) => api.telegramCheck(token) });
}

export function useTelegramSave() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.telegramSave,
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.settings }),
  });
}

export function useTelegramDisable() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.telegramDisable,
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.settings }),
  });
}

export function useLogin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (password: string) => api.login(password),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: qk.session });
    },
  });
}

export function useLogout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.logout,
    onSettled: () => {
      setCsrf(null);
      qc.clear();
    },
  });
}

export function useCancelRun() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.cancel(name),
    onSettled: (_d, _e, name) => {
      void qc.invalidateQueries({ queryKey: qk.overview });
      void qc.invalidateQueries({ queryKey: qk.runs(name) });
    },
  });
}
