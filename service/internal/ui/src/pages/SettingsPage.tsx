import { formatDateTime } from "@/lib/format";
import { BellRing, Info, LogOut, Monitor, Moon, Send, Sun } from "lucide-react";
import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { describeError } from "@/api/client";
import { useLogout, useMeta, useOverview, useSettings, useTestNotify } from "@/api/queries";
import { Page } from "@/components/layout/AppShell";
import { Badge, Dot } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { ErrorState } from "@/components/ui/Empty";
import { Segmented } from "@/components/ui/Segmented";
import { SkeletonRows } from "@/components/ui/Skeleton";
import { useToast } from "@/components/ui/Toast";
import { Section } from "@/features/form/controls";
import { TelegramCard } from "@/features/settings/TelegramCard";
import { useDocumentTitle } from "@/lib/hooks";
import { useTheme, type ThemeMode } from "@/lib/theme";

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-[140px_minmax(0,1fr)] items-baseline gap-3 py-2 text-sm">
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </div>
  );
}

export function SettingsPage() {
  useDocumentTitle("Settings");
  const settings = useSettings();
  const overview = useOverview();
  const meta = useMeta();
  const notify = useTestNotify();
  const logout = useLogout();
  const nav = useNavigate();
  const { success, error } = useToast();
  const { mode, setMode } = useTheme();
  const storage = overview.data?.storage;
  const s = settings.data;

  return (
    <Page title="Settings" meta="Storage is configured in config.yaml on the NAS; Telegram is connected right here.">
      {settings.isLoading && <SkeletonRows rows={6} />}
      {settings.error && <ErrorState {...describeError(settings.error)} onRetry={() => void settings.refetch()} />}
      {s && (
        <>
          <Section id="st-storage" title="Storage" description="Where archives are stored. Changes take effect after the service restarts.">
            <div className="card px-4 py-1">
              <dl className="divide-y divide-line">
                <Row label="Status">
                  {storage ? (
                    <span className="inline-flex items-center gap-2">
                      <Dot tone={storage.ok ? "ok" : "danger"} />
                      {storage.ok ? "Available" : <span className="text-danger">Unavailable</span>}
                      {storage.error && <span className="font-mono text-xs text-danger">{storage.error}</span>}
                    </span>
                  ) : "—"}
                </Row>
                <Row label="Type"><span className="font-mono">{s.storage.kind}</span></Row>
                {s.storage.endpoint && <Row label="Endpoint"><span className="font-mono">{s.storage.endpoint}</span></Row>}
                {s.storage.bucket && <Row label="Bucket"><span className="font-mono">{s.storage.bucket}</span></Row>}
                {s.storage.prefix !== undefined && <Row label="Prefix"><span className="font-mono">{s.storage.prefix || "—"}</span></Row>}
                <Row label="Parallelism">{s.max_parallel} {s.max_parallel === 1 ? "run" : "runs"} at a time</Row>
              </dl>
            </div>
          </Section>

          <Section id="st-notify" title="Notifications" description="Where messages about failures and suspiciously small archives are sent.">
            <div className="card divide-y divide-line">
              <TelegramCard telegram={s.notify.telegram} />
              <dl className="px-4 py-1">
                <Row label="Webhook">{s.notify.webhook_set ? <Badge tone="ok">set</Badge> : <span className="text-faint">not set</span>}</Row>
              </dl>
            </div>
            {s.notify.webhook_set && (
              <div className="flex flex-wrap items-center gap-3">
                <Button
                  icon={<Send className="h-4 w-4" aria-hidden />}
                  loading={notify.isPending}
                  onClick={() =>
                    notify.mutate(undefined, {
                      onSuccess: (r) => (r.ok ? success("Test notification sent", "Check Telegram or the webhook") : error(new Error("send failed"), "Notification not sent")),
                      onError: (e) => error(e, "Notification not sent"),
                    })
                  }
                >
                  Send test notification
                </Button>
                <span className="flex items-center gap-1.5 text-xs text-muted"><BellRing className="h-3.5 w-3.5" aria-hidden /> To all configured channels</span>
              </div>
            )}
          </Section>
        </>
      )}

      <Section id="st-ui" title="Interface" description="Stored in this browser.">
        <div>
          <div className="mb-1.5 text-sm font-medium">Theme</div>
          <Segmented<ThemeMode>
            ariaLabel="Color theme"
            value={mode}
            onChange={setMode}
            options={[
              { value: "system", label: "System", icon: <Monitor className="h-3.5 w-3.5" aria-hidden /> },
              { value: "light", label: "Light", icon: <Sun className="h-3.5 w-3.5" aria-hidden /> },
              { value: "dark", label: "Dark", icon: <Moon className="h-3.5 w-3.5" aria-hidden /> },
            ]}
          />
        </div>
      </Section>

      <Section id="st-session" title="Session">
        <div className="flex flex-wrap items-center gap-3">
          <Button
            icon={<LogOut className="h-4 w-4" aria-hidden />}
            loading={logout.isPending}
            onClick={() => logout.mutate(undefined, { onSuccess: () => nav("/login", { replace: true }), onError: (e) => error(e, "Could not sign out") })}
          >
            Sign out
          </Button>
          {meta.data && (
            <span className="flex items-center gap-1.5 text-xs text-faint"><Info className="h-3.5 w-3.5" aria-hidden /> db-backup v{meta.data.version}
              {meta.data.build && <> · commit {meta.data.build.commit}{meta.data.build.deployed_at && <>, deployed {formatDateTime(meta.data.build.deployed_at)}</>}</>}</span>
          )}
        </div>
      </Section>
    </Page>
  );
}
