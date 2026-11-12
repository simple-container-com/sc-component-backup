import { ArrowLeft, PlugZap } from "lucide-react";
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { ApiError, describeError } from "@/api/client";
import { useCreateProject, useMeta, useOverview, useTestConnection } from "@/api/queries";
import type { EngineId, EngineMeta, TestConnectionResult } from "@/api/types";
import { Page } from "@/components/layout/AppShell";
import { EngineMark } from "@/components/Engine";
import { Button } from "@/components/ui/Button";
import { ErrorState } from "@/components/ui/Empty";
import { Field, SecretInput, TextInput } from "@/components/ui/Field";
import { LinkButton } from "@/components/ui/LinkButton";
import { SkeletonRows } from "@/components/ui/Skeleton";
import { useToast } from "@/components/ui/Toast";
import { ConnectionResult, Section } from "@/features/form/controls";
import { argFlag, defaults, serverFieldKey, toConfig, URI_SCHEMES, validate, type Errors, type FormValues } from "@/features/form/model";
import { ProjectFormBody } from "@/features/form/ProjectFormBody";
import { useUnsavedGuard } from "@/features/form/useUnsavedGuard";
import { cn } from "@/lib/cn";
import { useDocumentTitle } from "@/lib/hooks";

const ENGINE_NOTE: Record<EngineId, string> = {
  mongodb: "mongodump, .archive.gz",
  postgres: "pg_dump -Fc plus roles",
  mariadb: "mariadb-dump, .sql.gz",
  s3import: "pulls ready-made archives from S3",
};

export function NewProjectPage() {
  useDocumentTitle("New project");
  const meta = useMeta();
  if (meta.isLoading) return <Page title="New project"><SkeletonRows rows={6} /></Page>;
  if (meta.error || !meta.data) return <Page title="New project"><ErrorState {...describeError(meta.error)} onRetry={() => void meta.refetch()} /></Page>;
  return <NewProjectForm engines={meta.data.engines} />;
}

function NewProjectForm({ engines }: { engines: EngineMeta[] }) {
  const nav = useNavigate();
  const overview = useOverview();
  const create = useCreateProject();
  const test = useTestConnection();
  const { success, error: toastError } = useToast();

  const initial = useMemo(() => defaults(engines[0]?.id ?? "mongodb", engines), [engines]);
  const [v, setV] = useState<FormValues>(initial);
  const [uri, setUri] = useState("");
  const [submitted, setSubmitted] = useState(false);
  const [serverErrors, setServerErrors] = useState<Errors>({});
  const [result, setResult] = useState<TestConnectionResult | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const nameRef = useRef<HTMLInputElement>(null);

  const engine = engines.find((e) => e.id === v.engine);
  const taken = overview.data?.projects.map((p) => p.name) ?? [];
  const localErrors = validate(v, engine, { checkName: true, takenNames: taken });
  if (!uri.trim()) localErrors.uri = "Paste a connection string";
  const errors: Errors = submitted ? { ...localErrors, ...serverErrors } : serverErrors;
  const dirty = !done && (uri !== "" || JSON.stringify(v) !== JSON.stringify(initial));
  const guard = useUnsavedGuard(dirty && !create.isPending);

  useEffect(() => nameRef.current?.focus(), []);

  const set = <K extends keyof FormValues>(k: K, val: FormValues[K]) => {
    setV((p) => ({ ...p, [k]: val }));
    setServerErrors((e) => {
      const n = { ...e };
      delete n[k === "shrink" ? "shrink_warn_percent" : k === "dumpArgs" ? "dump_args" : k === "dbMode" ? "databases" : k];
      return n;
    });
  };

  const pickEngine = (id: EngineId) => {
    if (id === v.engine) return;
    const em = engines.find((e) => e.id === id);
    setV((p) => ({ ...p, engine: id, dumpArgs: p.dumpArgs.filter((a) => em?.dump_args.includes(argFlag(a))), globals: !!em?.supports_globals, databases: [], dbMode: "all" }));
    setResult(null);
    test.reset();
  };

  const scheme = uri.trim() ? uri.trim().toLowerCase() : "";
  const schemeWarn = scheme && !URI_SCHEMES[v.engine].some((s) => scheme.startsWith(s))
    ? `For ${engine?.label ?? v.engine} the string usually starts with ${URI_SCHEMES[v.engine].join(" or ")}`
    : null;

  const runTest = () => {
    if (!uri.trim()) { setSubmitted(true); return; }
    test.mutate(
      { engine: v.engine, uri: uri.trim() },
      {
        onSuccess: (r) => {
          setResult(r);
          if (r.ok && v.dbMode === "list" && v.databases.length === 0) set("databases", r.databases);
        },
        onError: (e) => { setResult(null); toastError(e, "Check failed"); },
      },
    );
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    setFormError(null);
    if (Object.keys(localErrors).length) {
      const first = document.querySelector<HTMLElement>("[aria-invalid='true']");
      first?.focus();
      return;
    }
    const config = toConfig(v, engine);
    create.mutate(
      { config, uri: uri.trim() },
      {
        onSuccess: () => {
          guard.allow();
          setDone(true);
          setUri("");
          success("Project created", `${config.name}: the first run happens on schedule or via the "Run now" button`);
          nav(`/projects/${encodeURIComponent(config.name)}`, { replace: true });
        },
        onError: (err) => {
          if (err instanceof ApiError) {
            const key = serverFieldKey(err.field);
            if (err.status === 409) setServerErrors({ name: "A project with this name already exists" });
            else if (key) setServerErrors({ [key]: err.message });
            else setFormError(describeError(err).title + (err.message ? `: ${err.message}` : ""));
          } else setFormError(describeError(err).title);
        },
      },
    );
  };

  const head = (
    <>
      <Section id="sec-engine" title="Engine" description="The server type determines the dump tool, archive format and allowed arguments.">
        <div role="radiogroup" aria-label="Engine" className="grid gap-2 sm:grid-cols-3">
          {engines.map((em, i) => {
            const on = em.id === v.engine;
            return (
              <button
                key={em.id}
                type="button"
                role="radio"
                aria-checked={on}
                tabIndex={on ? 0 : -1}
                onClick={() => pickEngine(em.id)}
                onKeyDown={(e) => {
                  const d = e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : 0;
                  if (!d) return;
                  e.preventDefault();
                  const next = engines[(i + d + engines.length) % engines.length];
                  if (next) {
                    pickEngine(next.id);
                    (e.currentTarget.parentElement?.children[(i + d + engines.length) % engines.length] as HTMLElement | undefined)?.focus();
                  }
                }}
                className={cn(
                  "flex items-start gap-3 rounded-md border p-3 text-left transition-colors",
                  on ? "border-accent bg-accent-soft/40 ring-1 ring-accent" : "border-line bg-surface hover:border-line-strong",
                )}
              >
                <EngineMark engine={em.id} size="lg" />
                <span className="min-w-0">
                  <span className="block text-sm font-medium">{em.label}</span>
                  <span className="mt-0.5 block text-xs text-muted">{ENGINE_NOTE[em.id] ?? ""}</span>
                </span>
              </button>
            );
          })}
        </div>
      </Section>

      <Section id="sec-conn" title="Connection" description="The connection string is stored only on the NAS in a separate file and is never shown again.">
        <Field label="Project name" htmlFor="f-name" error={errors.name} hint="Used in storage paths and notifications. It cannot be renamed later.">
          <TextInput
            ref={nameRef}
            id="f-name"
            value={v.name}
            onChange={(e) => set("name", e.target.value)}
            invalid={!!errors.name}
            placeholder="e.g. prod-main"
            autoComplete="off"
            spellCheck={false}
            className="font-mono sm:w-80"
          />
        </Field>
        <Field label="Connection string" htmlFor="f-uri" error={errors.uri} hint={schemeWarn ?? undefined}>
          <SecretInput
            id="f-uri"
            value={uri}
            onChange={(e) => { setUri(e.target.value); setResult(null); setServerErrors((s) => { const n = { ...s }; delete n.uri; return n; }); }}
            invalid={!!errors.uri}
            placeholder={engine?.uri_example}
          />
        </Field>
        <div className="space-y-3">
          <Button onClick={runTest} loading={test.isPending} icon={<PlugZap className="h-4 w-4" aria-hidden />} disabled={!uri.trim()}>
            Test connection
          </Button>
          {result && <ConnectionResult result={result} />}
        </div>
      </Section>
    </>
  );

  return (
    <Page
      title="New project"
      meta="One project is one server. Test the connection to pick databases from a list."
      back={
        <LinkButton to="/" variant="ghost" size="sm" className="-ml-2 mb-3" icon={<ArrowLeft className="h-3.5 w-3.5" aria-hidden />}>
          Overview
        </LinkButton>
      }
    >
      {guard.dialog}
      <form onSubmit={submit} noValidate>
        <ProjectFormBody v={v} set={set} errors={errors} engine={engine} available={result?.ok ? result.databases : null} tested={!!result?.ok} head={head} />
        {formError && <div className="mb-4"><ErrorState title="Project not created" detail={formError} /></div>}
        <div className="sticky bottom-0 z-20 flex items-center justify-end gap-2 border-t border-line bg-bg py-3">
          {submitted && Object.keys(localErrors).length > 0 && (
            <span className="mr-auto text-xs text-danger" role="status">Fix the fields marked in red</span>
          )}
          <LinkButton to="/" variant="ghost">Cancel</LinkButton>
          <Button type="submit" variant="primary" loading={create.isPending}>Create project</Button>
        </div>
      </form>
    </Page>
  );
}
