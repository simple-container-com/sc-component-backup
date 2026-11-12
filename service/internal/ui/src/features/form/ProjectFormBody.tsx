import type { ReactNode } from "react";
import type { EngineMeta } from "@/api/types";
import { Switch } from "@/components/ui/Switch";
import { CountField, DatabasePicker, DumpArgsEditor, DurationField, RetentionEditor, Section, StartAtField } from "./controls";
import type { Errors, FormValues } from "./model";

export function ProjectFormBody({
  v, set, errors, engine, available, tested, head,
}: {
  v: FormValues;
  set: <K extends keyof FormValues>(k: K, val: FormValues[K]) => void;
  errors: Errors;
  engine: EngineMeta | undefined;
  available: string[] | null;
  tested: boolean;
  head: ReactNode;
}) {
  return (
    <>
      {head}

      <Section id="sec-dbs" title="Databases" description="What to back up from this server. Each database is a separate archive.">
        <DatabasePicker
          engine={v.engine}
          mode={v.dbMode}
          selected={v.databases}
          available={available}
          tested={tested}
          onMode={(m) => set("dbMode", m)}
          onSelected={(d) => set("databases", d)}
          error={errors.databases}
        />
      </Section>

      <Section id="sec-schedule" title="Schedule" description="Either at a set time (and then every interval from it), or one interval after the start of the last successful run.">
        <DurationField
          id="f-interval" kind="interval" label="Interval" value={v.interval} onChange={(x) => set("interval", x)}
          error={errors.interval} presets={["1h", "6h", "12h", "1d", "1w"]}
          hint="Units: m minutes, h hours, d days, w weeks. They can be combined: 1d12h."
        />
        <StartAtField value={v.start_at} onChange={(x) => set("start_at", x)} interval={v.interval} error={errors.start_at} />
        <DurationField
          id="f-timeout" kind="timeout" label="Run timeout" value={v.timeout} onChange={(x) => set("timeout", x)}
          error={errors.timeout} presets={["1h", "3h", "6h", "12h"]}
          hint="Overall limit for finding databases, dumping and uploading. When it expires, the run is aborted without publishing an archive."
        />
      </Section>

      <Section id="sec-retention" title="Retention" description="How many archives to keep per database. The rest are deleted after a successful run.">
        <RetentionEditor v={v} set={(r) => set("retention", r)} errors={errors} />
      </Section>

      <Section id="sec-advanced" title="Advanced" description="Retries on connection loss, size checks and fine-tuning of the dump tool.">
        <div className="grid gap-5 sm:grid-cols-2">
          <CountField
            id="f-retries" label="Retries on failure" value={v.retries} onChange={(x) => set("retries", x)} min={0} max={10}
            error={errors.retries} hint="How many times to retry a database dump after an error, from 0 to 10."
          />
          <CountField
            id="f-shrink" label="Warn when size drops" value={v.shrink} onChange={(x) => set("shrink", x)} min={0} max={99}
            suffix="%" error={errors.shrink_warn_percent} hint="Notify if an archive is smaller than the previous one by this many percent. 0 disables the check."
          />
        </div>
        {engine && engine.dump_args.length > 0 && (
          <DumpArgsEditor key={engine.id} engine={engine} value={v.dumpArgs} onChange={(a) => set("dumpArgs", a)} error={errors.dump_args} />
        )}
        {engine?.supports_globals && (
          <div className="flex items-start justify-between gap-4 rounded-md border border-line px-3.5 py-3">
            <div>
              <label htmlFor="f-globals" className="text-sm font-medium">Global objects</label>
              <p className="mt-0.5 text-xs text-muted">
                Roles, grants and tablespaces (<code>pg_dumpall --globals-only</code>, without role passwords). Needed to restore the whole server.
              </p>
            </div>
            <Switch id="f-globals" checked={v.globals} onChange={(x) => set("globals", x)} />
          </div>
        )}
      </Section>
    </>
  );
}
