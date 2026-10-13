import { useEffect, useId, useMemo, useState } from "react";
import { CheckCircle2, CircleAlert } from "lucide-react";
import { api } from "~/lib/api";
import { unifiedAppContract, type AppSchema } from "~/lib/unified-app-io";
import { schemaExample } from "~/lib/schema-example";
import { compileInputContract, validateInputText } from "~/lib/schedule-input-validation";
import { TypeScriptEditor } from "~/components/code/TypeScriptEditor";

const action = "rounded-md px-2.5 py-1.5 text-xs font-medium text-slate-600 transition hover:bg-slate-100 focus-visible:outline-violet-500 disabled:opacity-40";

/** Edits one compact JSON payload and validates it against the version that will actually execute. */
export function ScheduleInputEditor({ appID, text, onChange }: { appID: string; text: string; onChange: (value: string) => void }) {
  const id = useId();
  const [schema, setSchema] = useState<AppSchema | null>(null);
  const [loading, setLoading] = useState(true);
  const [notice, setNotice] = useState("");
  const [exampleOpen, setExampleOpen] = useState(false);
  const [attempt, setAttempt] = useState(0);
  // Leaving the draft cancels private schema responses and never changes the input document.
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setSchema(null); setNotice("");
    /** Reads the serving version's authored contract, even when viewing a retained version. */
    async function load() {
      try {
        const target = await api.appConfig.traffic(appID);
        // A family without traffic cannot supply an authoritative input contract.
        if (!target.active_app_id) throw new Error("No active app version. Activate a version to validate input.");
        const document = await api.appConfig.openAPI(target.active_app_id, "execute", controller.signal);
        // A response for a closed editor must never replace another draft's contract.
        if (!controller.signal.aborted) setSchema(unifiedAppContract(document).input);
      } catch (cause) {
        // Preserve editing on transport failure and explicitly disclose the missing validation.
        if (!controller.signal.aborted) setNotice(cause instanceof Error ? cause.message : "Could not load the input schema.");
      } finally {
        // Cleanup owns the last state transition after navigation.
        if (!controller.signal.aborted) setLoading(false);
      }
    }
    void load();
    return () => controller.abort();
  }, [appID, attempt]);
  const contract = useMemo(() => compileInputContract(schema), [schema]);
  const result = useMemo(() => validateInputText(text, contract), [text, contract]);
  const example = useMemo(() => schemaExample(schema), [schema]);
  // While loading, saving waits for validation; failures remain explicit and Engine is authoritative on save.
  const validationMessage = loading ? "Wait for the input schema to load." : result.issues.map(issue => `${issue.path}: ${issue.message}`).join("\n");
  const warning = notice || result.warning;
  const verified = !loading && !warning && result.issues.length === 0;

  /** Formatting changes whitespace only and never repairs or coerces the user's values. */
  function format() {
    // Invalid JSON must remain intact so the user can fix the original syntax error.
    if (result.parsed) onChange(JSON.stringify(result.value, null, 2));
  }

  return <section aria-label="Scheduled input" className="min-w-0 space-y-4">
    <div className="flex flex-wrap items-center justify-between gap-2"><div className="flex items-center gap-2.5"><span className="flex h-6 w-6 items-center justify-center rounded-full bg-slate-100 text-xs font-semibold text-slate-500">2</span><h4 className="text-sm font-semibold text-slate-950">App input</h4></div><div className="flex gap-1"><button type="button" className={action} disabled={!result.parsed} onClick={format}>Format</button><button type="button" className={action} aria-expanded={exampleOpen} disabled={!example.json} onClick={() => setExampleOpen(!exampleOpen)}>Example</button></div></div>
    <p className="text-xs leading-relaxed text-slate-500">The JSON your app receives on every run. Types and required properties are checked as you edit.</p>
    {/* The example is previewed first; inserting it is an explicit action that never happens during schema loading. */}
    {exampleOpen && example.json && <div className="rounded-lg border border-violet-100 bg-violet-50/50 p-3"><p className="text-xs text-slate-600">A starting point from this app’s schema. Review placeholder values before saving.</p><pre className="my-3 max-h-48 overflow-auto text-xs leading-5 text-slate-700">{example.json}</pre><button type="button" className={action} onClick={() => { onChange(example.json!); setExampleOpen(false); }}>Use this example</button></div>}
    <TypeScriptEditor id={id} language="json" value={text} onChange={onChange} required validationMessage={validationMessage} describedBy={`${id}-validation`} />
    <div id={`${id}-validation`} aria-live="polite" className="text-xs leading-relaxed">
      {/* Distinct loading, error, warning, and success states avoid claiming unchecked input is valid. */}
      {loading ? <p className="text-slate-500">Loading the active app’s input schema…</p> : result.issues.length > 0 ? <div className="rounded-lg border border-red-100 bg-red-50 p-3 text-red-800"><p className="mb-1 flex items-center gap-2 font-medium"><CircleAlert className="h-4 w-4" aria-hidden="true" />{result.parsed ? "Input doesn’t match the schema" : "Check your JSON"}</p><ul className="space-y-1">{result.issues.map((issue, index) => <li key={index}><code className="break-all font-semibold">{issue.path}</code> {issue.message}</li>)}</ul></div> : verified ? <p className="flex items-center gap-2 text-emerald-700"><CheckCircle2 className="h-4 w-4" aria-hidden="true" />Matches the input schema</p> : <p className="text-amber-800">{warning} <button type="button" className="underline" onClick={() => setAttempt(attempt + 1)}>Reload schema</button></p>}
    </div>
    {/* The full contract remains available for nested types, descriptions, enums, and validation constraints. */}
    {schema !== null && <details className="text-xs text-slate-500"><summary className="cursor-pointer font-medium">View input schema</summary><pre className="mt-3 max-h-64 overflow-auto rounded-lg bg-slate-50 p-3 font-mono leading-5">{JSON.stringify(schema, null, 2)}</pre></details>}
  </section>;
}
