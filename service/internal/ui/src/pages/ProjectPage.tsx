import * as Tabs from "@radix-ui/react-tabs";
import { ArrowLeft, FolderSearch } from "lucide-react";
import { useNavigate, useParams } from "react-router-dom";
import { ApiError, describeError } from "@/api/client";
import { useOverview, useProject } from "@/api/queries";
import { Page } from "@/components/layout/AppShell";
import { EngineBadge } from "@/components/Engine";
import { NextDue } from "@/components/NextDue";
import { StatusBadge, statusOf } from "@/components/Status";
import { Empty, ErrorState } from "@/components/ui/Empty";
import { LinkButton } from "@/components/ui/LinkButton";
import { Skeleton } from "@/components/ui/Skeleton";
import { RelTime } from "@/components/ui/Time";
import { LiveRunCard } from "@/features/RunDatabases";
import { RunNowButton } from "@/features/RunNowButton";
import { StopRunButton } from "@/features/StopRunButton";
import { ArchivesTab } from "@/features/project/ArchivesTab";
import { HistoryTab } from "@/features/project/HistoryTab";
import { SettingsTab } from "@/features/project/SettingsTab";
import { StatisticsTab } from "@/features/project/StatisticsTab";
import { formatBytes, humanSchedule, plural } from "@/lib/format";
import { useDocumentTitle } from "@/lib/hooks";
import { cn } from "@/lib/cn";

const TABS = [
  { id: "archives", label: "Archives", path: "" },
  { id: "history", label: "History", path: "history" },
  { id: "stats", label: "Statistics", path: "stats" },
  { id: "settings", label: "Settings", path: "settings" },
] as const;
type TabId = (typeof TABS)[number]["id"];
const TAB_IDS: readonly string[] = TABS.map((t) => t.id);

export function ProjectPage() {
  const { name = "", tab } = useParams();
  const nav = useNavigate();
  useDocumentTitle(name);
  const overview = useOverview();
  const project = useProject(name);
  const summary = overview.data?.projects.find((p) => p.name === name);
  const running = overview.data?.running.find((r) => r.project === name);
  const active: TabId = tab && TAB_IDS.includes(tab) ? (tab as TabId) : "archives";
  const validTab = tab === undefined || TAB_IDS.includes(tab);

  const back = (
    <LinkButton to="/" variant="ghost" size="sm" className="-ml-2 mb-3" icon={<ArrowLeft className="h-3.5 w-3.5" aria-hidden />}>
      Overview
    </LinkButton>
  );

  if (!validTab || (project.error instanceof ApiError && project.error.status === 404) || (overview.data && !summary && !project.data && !project.isLoading)) {
    return (
      <Page title={name} back={back}>
        <Empty icon={<FolderSearch className="h-5 w-5" />} title="Project not found" action={<LinkButton to="/" variant="secondary">Back to overview</LinkButton>}>
          It may have been deleted or renamed in config.yaml.
        </Empty>
      </Page>
    );
  }

  const st = summary ? statusOf(summary) : null;

  return (
    <Page
      wide
      back={back}
      title={<span className="break-all">{name}</span>}
      meta={
        summary ? (
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5">
            <EngineBadge engine={summary.engine} />
            {st && <StatusBadge status={st} />}
            <span>{humanSchedule(summary.interval, summary.start_at)}</span>
            {summary.state === "idle" && summary.next_due && <span>next run <NextDue p={summary} /></span>}
            {summary.last_success_at && <span>last success <RelTime iso={summary.last_success_at} /></span>}
            {summary.archive_count > 0 && (
              <span className="tnum">{formatBytes(summary.total_bytes)} in {summary.archive_count} {plural(summary.archive_count, ["archive", "archives"])}</span>
            )}
          </div>
        ) : (
          <Skeleton className="h-4 w-72" />
        )
      }
      actions={summary ? (
        <div className="flex items-center gap-2">
          {summary.state !== "idle" && <StopRunButton name={name} cancelling={summary.state === "cancelling"} size="md" />}
          <RunNowButton name={name} state={summary.state} uriSet={summary.uri_set} size="md" variant="primary" />
        </div>
      ) : null}
    >
      {running && <LiveRunCard op={running} />}
      <Tabs.Root
        value={active}
        onValueChange={(v) => {
          const t = TABS.find((x) => x.id === v);
          nav(`/projects/${encodeURIComponent(name)}${t?.path ? `/${t.path}` : ""}`);
        }}
        activationMode="manual"
      >
        <Tabs.List aria-label="Project sections" className="mb-6 flex gap-1 border-b border-line">
          {TABS.map((t) => (
            <Tabs.Trigger
              key={t.id}
              value={t.id}
              className={cn(
                "-mb-px shrink-0 whitespace-nowrap border-b-2 px-3 pb-2.5 pt-1 text-sm font-medium transition-colors",
                "data-[state=active]:border-accent data-[state=active]:text-fg data-[state=inactive]:border-transparent data-[state=inactive]:text-muted hover:text-fg",
              )}
            >
              {t.label}
            </Tabs.Trigger>
          ))}
        </Tabs.List>

        {project.error && !(project.error instanceof ApiError && project.error.status === 404) && (
          <ErrorState {...describeError(project.error)} onRetry={() => void project.refetch()} />
        )}

        <Tabs.Content value="archives" className="focus:outline-none">
          <ArchivesTab name={name} engine={summary?.engine ?? project.data?.config.engine} retention={project.data?.config.retention} globalsEnabled={project.data?.config.globals} />
        </Tabs.Content>
        <Tabs.Content value="history" className="focus:outline-none">
          <HistoryTab name={name} live={!!running || summary?.state !== "idle"} running={running} />
        </Tabs.Content>
        <Tabs.Content value="stats" className="focus:outline-none">
          {active === "stats" && <StatisticsTab name={name} engine={summary?.engine ?? project.data?.config.engine} />}
        </Tabs.Content>
        <Tabs.Content value="settings" className="focus:outline-none">
          {project.isLoading ? <Skeleton className="h-96 w-full" /> : project.data ? <SettingsTab name={name} data={project.data} /> : null}
        </Tabs.Content>
      </Tabs.Root>
    </Page>
  );
}
