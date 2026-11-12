import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, KeyRound, PlugZap, RotateCcw, Trash2 } from "lucide-react";
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { api, ApiError, describeError } from "@/api/client";
import { qk, useDeleteProject, useMeta, useSetSecret, useTestConnection, useUpdateProject } from "@/api/queries";
import type { ProjectResponse, TestConnectionResult } from "@/api/types";
import { EngineMark, engineLabel } from "@/components/Engine";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { Field, SecretInput, TextInput } from "@/components/ui/Field";
import { useToast } from "@/components/ui/Toast";
import { ConnectionResult, Section } from "@/features/form/controls";
import { fromConfig, sameValues, serverFieldKey, toConfig, URI_SCHEMES, validate, type Errors, type FormValues } from "@/features/form/model";
import { ProjectFormBody } from "@/features/form/ProjectFormBody";
import { useUnsavedGuard } from "@/features/form/useUnsavedGuard";

export function SettingsTab({ name, data }: { name: string; data: ProjectResponse }) {
  const meta = useMeta();
  const engine = meta.data?.engines.find((e) => e.id === data.config.engine);
  const base = useMemo(() => fromConfig(data.config), [data.config]);
  const [v, setV] = useState<FormValues>(base);
  const [revision, setRevision] = useState(data.revision);
  const [serverErrors, setServerErrors] = useState<Errors>({});
  const [conflict, setConflict] = useState(false);
  const [result, setResult] = useState<TestConnectionResult | null>(null);
  const update = useUpdateProject(name);
  const test = useTestConnection();
  const qc = useQueryClient();
  const { success, error: toastError, toast } = useToast();

  // Server copy changed (after our save, or someone else's edit): adopt it if we have no local edits.
  const prevBase = useRef(base);
  useEffect(() => {
    if (data.revision !== revision && sameValues(v, prevBase.current)) {
      setV(fromConfig(data.config));
      setRevision(data.revision);
    } else if (data.revision === revision) {
      setV(fromConfig(data.config)); // our own save came back, possibly normalised by the server
    }
    prevBase.current = base;
    // Only react to a new server revision.
  }, [data.revision]);

  const dirty = !sameValues(v, base);
  const guard = useUnsavedGuard(dirty);
  const local = validate(v, engine, {});
  const errors: Errors = { ...local, ...serverErrors };
  const hasErrors = Object.keys(local).length > 0;

  const set = <K extends keyof FormValues>(k: K, val: FormValues[K]) => {
    setV((p) => ({ ...p, [k]: val }));
    setServerErrors({});
  };

  const save = (rev = revision) => {
    if (hasErrors) {
      document.querySelector<HTMLElement>("[aria-invalid='true']")?.focus();
      return;
    }
    update.mutate(
      { config: toConfig(v, engine), revision: rev },
      {
        onSuccess: (r) => {
          setRevision(r.revision);
          setConflict(false);
          success("Settings saved", "They apply from the next run");
        },
        onError: (e) => {
          if (e instanceof ApiError && e.status === 409) setConflict(true);
          else if (e instanceof ApiError && serverFieldKey(e.field)) setServerErrors({ [serverFieldKey(e.field)!]: e.message });
          else toastError(e, "Not saved");
        },
      },
    );
  };

  const onSubmit = (e: FormEvent) => { e.preventDefault(); save(); };

  const reloadTheirs = async () => {
    const fresh = await qc.fetchQuery({ queryKey: qk.project(name), queryFn: () => api.project(name) });
    setV(fromConfig(fresh.config));
    setRevision(fresh.revision);
    setConflict(false);
    toast({ kind: "info", title: "Loaded the latest version", detail: "Your changes were discarded" });
  };
  const overwrite = async () => {
    const fresh = await qc.fetchQuery({ queryKey: qk.project(name), queryFn: () => api.project(name) });
    save(fresh.revision);
  };

  const runTest = (uri?: string) => {
    test.mutate(uri ? { engine: data.config.engine, uri } : { project: name }, {
      onSuccess: setResult,
      onError: (e) => toastError(e, "Check failed"),
    });
  };

  const head = (
    <Section id="sec-conn" title="Connection" description="The connection string is stored only on the NAS. It can be replaced but not viewed.">
      <div className="flex flex-wrap gap-6">
        <div>
          <div className="mb-1.5 text-sm font-medium">Name</div>
          <div className="font-mono text-sm">{name}</div>
        </div>
        <div>
          <div className="mb-1.5 text-sm font-medium">Engine</div>
          <div className="flex items-center gap-2 text-sm"><EngineMark engine={data.config.engine} size="sm" /> {engineLabel(data.config.engine)}</div>
        </div>
      </div>
      <SecretRow name={name} uriSet={data.uri_set} engine={data.config.engine} uriExample={engine?.uri_example} onTest={runTest} testing={test.isPending && !!test.variables && "uri" in test.variables} />
      <div className="space-y-3">
        <Button onClick={() => runTest()} loading={test.isPending && !!test.variables && "project" in test.variables} disabled={!data.uri_set} icon={<PlugZap className="h-4 w-4" aria-hidden />}>
          Check connection
        </Button>
        {result && <ConnectionResult result={result} />}
      </div>
    </Section>
  );

  return (
    <>
      {guard.dialog}
      {conflict && (
        <div role="alert" className="mb-6 flex flex-wrap items-center gap-3 rounded-md border border-warn/30 bg-warn-soft/60 px-4 py-3">
          <AlertTriangle className="h-4 w-4 text-warn" aria-hidden />
          <div className="min-w-0 flex-1">
            <p className="text-sm font-medium">The configuration changed while you were editing</p>
            <p className="text-xs text-muted">Someone saved it in another tab or edited config.yaml by hand.</p>
          </div>
          <div className="flex gap-2">
            <Button size="sm" onClick={() => void reloadTheirs()} icon={<RotateCcw className="h-3.5 w-3.5" />}>Load latest</Button>
            <Button size="sm" variant="danger" onClick={() => void overwrite()} loading={update.isPending}>Overwrite with mine</Button>
          </div>
        </div>
      )}
      <form onSubmit={onSubmit} noValidate>
        <ProjectFormBody v={v} set={set} errors={errors} engine={engine} available={result?.ok ? result.databases : null} tested={!!result?.ok} head={head} />

        <DangerZone name={name} revision={revision} guardAllow={guard.allow} />

        {(dirty || update.isPending) && (
          <div className="sticky bottom-3 z-20 mt-6 flex animate-pop-in flex-wrap items-center gap-2 rounded-lg border border-line bg-surface px-4 py-2.5 shadow-pop">
            <span className="mr-auto text-sm text-muted">{hasErrors ? <span className="text-danger">Some fields have errors</span> : "You have unsaved changes"}</span>
            <Button variant="ghost" onClick={() => { setV(base); setServerErrors({}); }}>Discard</Button>
            <Button type="submit" variant="primary" loading={update.isPending}>Save</Button>
          </div>
        )}
      </form>
    </>
  );
}

function SecretRow({ name, uriSet, engine, uriExample, onTest, testing }: {
  name: string; uriSet: boolean; engine: keyof typeof URI_SCHEMES; uriExample?: string; onTest: (uri: string) => void; testing: boolean;
}) {
  const [editing, setEditing] = useState(!uriSet);
  const [uri, setUri] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const secret = useSetSecret(name);
  const { success } = useToast();
  const schemeOk = !uri.trim() || URI_SCHEMES[engine].some((s) => uri.trim().toLowerCase().startsWith(s));

  const save = () => {
    if (!uri.trim()) { setErr("Paste the connection string"); return; }
    secret.mutate(uri.trim(), {
      onSuccess: () => {
        setUri("");
        setEditing(false);
        setErr(null);
        success(uriSet ? "Connection string replaced" : "Connection string saved");
      },
      onError: (e) => {
        const d = describeError(e);
        setErr(`${d.title}${d.detail ? `: ${d.detail}` : ""}`);
      },
    });
  };

  if (!editing) {
    return (
      <div>
        <div className="mb-1.5 text-sm font-medium">Connection string</div>
        <div className="flex flex-wrap items-center gap-3 rounded border border-line bg-sunken/50 px-3 py-2">
          <KeyRound className="h-4 w-4 text-faint" aria-hidden />
          <span className="font-mono text-sm tracking-widest text-faint" aria-hidden>••••••••••••</span>
          <Badge tone="ok">Set</Badge>
          <Button size="sm" className="ml-auto" onClick={() => setEditing(true)}>Replace</Button>
        </div>
      </div>
    );
  }
  return (
    <Field
      label="Connection string"
      htmlFor="f-uri"
      error={err}
      hint={!schemeOk ? `Usually starts with ${URI_SCHEMES[engine].join(" or ")}` : uriSet ? "The old string will be overwritten. Saved immediately, separately from the other settings." : "The project cannot run without a connection string."}
      aside={!uriSet ? <Badge tone="warn">Not set</Badge> : undefined}
    >
      <div className="flex flex-col gap-2 sm:flex-row">
        <div className="min-w-0 flex-1">
          <SecretInput id="f-uri" value={uri} onChange={(e) => { setUri(e.target.value); setErr(null); }} invalid={!!err} placeholder={uriExample} />
        </div>
        <div className="flex gap-2">
          <Button onClick={() => onTest(uri.trim())} disabled={!uri.trim()} loading={testing}>Check</Button>
          <Button variant="primary" onClick={save} loading={secret.isPending}>Save</Button>
          {uriSet && <Button variant="ghost" onClick={() => { setEditing(false); setUri(""); setErr(null); }}>Cancel</Button>}
        </div>
      </div>
    </Field>
  );
}

function DangerZone({ name, revision, guardAllow }: { name: string; revision: string; guardAllow: () => void }) {
  const [open, setOpen] = useState(false);
  const [typed, setTyped] = useState("");
  const del = useDeleteProject(name);
  const nav = useNavigate();
  const { success, error } = useToast();
  const confirm = () => {
    del.mutate(revision, {
      onSuccess: () => {
        guardAllow();
        setOpen(false);
        success("Project deleted", "Archives remain in storage");
        nav("/", { replace: true });
      },
      onError: (e) => {
        if (e instanceof ApiError && e.status === 409) error(e, "The configuration changed: reload the page and try again");
        else error(e, "Project not deleted");
      },
    });
  };
  return (
    <section aria-labelledby="sec-danger" className="mt-2 grid gap-x-10 gap-y-4 border-t border-line py-7 md:grid-cols-[220px_minmax(0,1fr)]">
      <div>
        <h2 id="sec-danger" className="text-[15px] font-semibold text-danger">Deletion</h2>
      </div>
      <div className="flex flex-wrap items-center justify-between gap-4 rounded-md border border-danger/25 px-4 py-3">
        <div className="min-w-0 max-w-md">
          <p className="text-sm font-medium">Delete project</p>
          <p className="mt-0.5 text-xs text-muted">The schedule stops and the project is removed from config.yaml. Archives in storage are not deleted.</p>
        </div>
        <Button variant="danger-ghost" className="border border-danger/30" icon={<Trash2 className="h-4 w-4" aria-hidden />} onClick={() => { setTyped(""); setOpen(true); }}>
          Delete
        </Button>
      </div>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={`Delete project ${name}?`}
        description="Scheduled backups will stop. Existing archives stay in storage: you can download them, and delete them manually with the storage tools."
        footer={
          <>
            <Button onClick={() => setOpen(false)}>Cancel</Button>
            <Button variant="danger" disabled={typed !== name} loading={del.isPending} onClick={confirm}>Delete project</Button>
          </>
        }
      >
        <Field label={<>Type <span className="font-mono">{name}</span> to confirm</>} htmlFor="del-confirm">
          <TextInput
            id="del-confirm"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            onKeyDown={(e) => { if (e.key === "Enter" && typed === name) { e.preventDefault(); confirm(); } }}
          />
        </Field>
      </Dialog>
    </section>
  );
}
