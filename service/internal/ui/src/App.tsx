import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect } from "react";
import { createBrowserRouter, Navigate, Outlet, RouterProvider, useLocation } from "react-router-dom";
import { ApiError, setCsrfRefreshedHandler, setUnauthorizedHandler } from "./api/client";
import { qk, useSession } from "./api/queries";
import type { Session } from "./api/types";
import { AppShell, Logo } from "./components/layout/AppShell";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { ToastProvider } from "./components/ui/Toast";
import { TooltipProvider } from "./components/ui/Tooltip";
import { UpdateBanner } from "./components/UpdateBanner";
import { ThemeProvider } from "./lib/theme";
import { LoginPage } from "./pages/LoginPage";
import { OverviewPage } from "./pages/OverviewPage";
import { ProjectPage } from "./pages/ProjectPage";
import { NewProjectPage } from "./pages/NewProjectPage";
import { SettingsPage } from "./pages/SettingsPage";
import { HistoryPage } from "./pages/HistoryPage";
import { NotFoundPage } from "./pages/NotFoundPage";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (n, e) => !(e instanceof ApiError && [401, 403, 404, 422].includes(e.status)) && n < 2,
      refetchOnWindowFocus: true,
      staleTime: 2000,
    },
  },
});

setUnauthorizedHandler(() => {
  const prev = queryClient.getQueryData<Session>(qk.session);
  queryClient.setQueryData<Session>(qk.session, { authenticated: false, csrf: null, password_set: prev?.password_set ?? true });
});

setCsrfRefreshedHandler((s) => queryClient.setQueryData<Session>(qk.session, s));

document.documentElement.lang = "en";

function Splash() {
  return (
    <div className="flex min-h-screen items-center justify-center" role="status" aria-label="Loading">
      <Logo className="h-8 w-8 animate-pulse2" />
    </div>
  );
}

function RequireAuth() {
  const { data, isLoading, error, refetch } = useSession();
  const loc = useLocation();
  if (isLoading) return <Splash />;
  if (error && !data) {
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-3 px-6 text-center">
        <Logo className="h-8 w-8" />
        <p className="font-medium">The db-backup service is not responding</p>
        <p className="max-w-sm text-sm text-muted">Check that the container is running. The page will retry.</p>
        <button className="text-sm font-medium text-accent underline underline-offset-2" onClick={() => void refetch()}>Retry</button>
      </div>
    );
  }
  if (!data?.authenticated) {
    const next = loc.pathname + loc.search;
    return <Navigate to={next && next !== "/" ? `/login?next=${encodeURIComponent(next)}` : "/login"} replace />;
  }
  return <Outlet />;
}

function Root() {
  return (
    <ErrorBoundary>
      <Outlet />
    </ErrorBoundary>
  );
}

const router = createBrowserRouter([
  {
    element: <Root />,
    children: [
      { path: "/login", element: <LoginPage /> },
      {
        element: <RequireAuth />,
        children: [
          {
            element: <AppShell />,
            children: [
              { index: true, element: <OverviewPage /> },
              { path: "new", element: <NewProjectPage /> },
              { path: "projects/:name", element: <ProjectPage /> },
              { path: "projects/:name/:tab", element: <ProjectPage /> },
              { path: "history", element: <HistoryPage /> },
              { path: "settings", element: <SettingsPage /> },
              { path: "*", element: <NotFoundPage /> },
            ],
          },
        ],
      },
    ],
  },
], {
  future: {
    v7_relativeSplatPath: true,
    v7_fetcherPersist: true,
    v7_normalizeFormMethod: true,
    v7_partialHydration: true,
    v7_skipActionErrorRevalidation: true,
  },
});

function Title() {
  useEffect(() => {
    document.documentElement.lang = "ru";
  }, []);
  return null;
}

export function App() {
  return (
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <TooltipProvider delayDuration={350} skipDelayDuration={150}>
          <ToastProvider>
            <Title />
            <RouterProvider router={router} future={{ v7_startTransition: true }} />
            <UpdateBanner />
          </ToastProvider>
        </TooltipProvider>
      </QueryClientProvider>
    </ThemeProvider>
  );
}
