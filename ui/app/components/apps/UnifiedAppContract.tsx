import { useEffect, useMemo, useState } from "react";
import { Loader2 } from "lucide-react";
import { api } from "~/lib/api";
import { unifiedAppContract, type UnifiedAppContract as Contract } from "~/lib/unified-app-io";
import { schemaExample } from "~/lib/schema-example";
import { SchemaViewer } from "~/components/SchemaViewer";
import { CopyButton } from "~/components/CopyValue";

/** Shows the saved public contract with Fields as the default and copyable example values. */
export function UnifiedAppContract({ appID, version }: { appID: string; version: string }) {
  const [contract, setContract] = useState<Contract | null>(null);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [view, setView] = useState<"fields" | "json">("fields");
  // Abort and discard stale responses when changing versions or leaving the overview.
  useEffect(() => {
    const controller = new AbortController();
    setContract(null); setError("");
    /** Keeps failed contract reads local so usage and execution controls remain available. */
    async function load() {
      try {
        const document = await api.appConfig.openAPI(appID, "execute", controller.signal);
        // A late response must never label another version's schema as the current version.
        if (!controller.signal.aborted) setContract(unifiedAppContract(document));
      } catch (cause) {
        // Navigation cancellation is not a user-visible contract failure.
        if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not load the app contract.");
      }
    }
    void load();
    return () => controller.abort();
  }, [appID, attempt]);

  // Loading and errors stay distinct from genuinely unconstrained JSON schemas.
  return <section className="min-w-0 overflow-hidden rounded-xl border border-slate-200 bg-white" aria-label="Input and output">
    <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 px-5 py-4">
      <div><h2 className="font-semibold text-slate-900">Input and output</h2><p className="mt-1 text-sm text-slate-500">What version {version} accepts and returns.</p></div>
      <div className="flex rounded-lg bg-slate-100 p-1" aria-label="Schema display">
        {/* These controls change presentation only; the saved contract remains authoritative. */}
        {(["fields", "json"] as const).map((mode) => <button key={mode} type="button" aria-pressed={view === mode} onClick={() => setView(mode)} className={`rounded-md px-3 py-1.5 text-sm font-medium ${view === mode ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-900"}`}>{mode === "fields" ? "Fields" : "JSON Schema"}</button>)}
      </div>
    </div>
    {error ? <div className="space-y-3 p-5"><p role="alert" className="text-sm text-red-700">{error}</p><button type="button" className="text-sm font-medium text-violet-700" onClick={() => setAttempt(attempt + 1)}>Retry</button></div> : !contract ? <p role="status" className="flex items-center gap-2 p-5 text-sm text-slate-500"><Loader2 className="h-4 w-4 animate-spin" />Loading schemas…</p> : <div className="grid min-w-0 gap-4 p-5 xl:grid-cols-2">
      {/* Each schema owns its local references, including nested $defs from the authored app. */}
      {(["input", "output"] as const).map((direction) => <ContractSchema key={direction} direction={direction} schema={contract[direction]} view={view} appID={appID} />)}
    </div>}
  </section>;
}

/** Copies editable payload values from Fields and the exact schema from JSON Schema. */
function ContractSchema({ direction, schema, view, appID }: { direction: "input" | "output"; schema: Contract["input"]; view: "fields" | "json"; appID: string }) {
  const example = useMemo(() => schemaExample(schema), [schema]);
  // Schema export is explicit; the ordinary copy affordance provides an editable payload without schema metadata.
  const json = view === "json" ? JSON.stringify(schema, null, 2) : example.json;
  const label = `${direction} ${view === "json" ? "schema" : "example"}`;
  return <div className="flex min-w-0 flex-col overflow-hidden rounded-lg border border-slate-200">
    <div className="flex items-center justify-between bg-slate-50 px-4 py-2"><h3 className="text-sm font-semibold capitalize text-slate-800">{direction}</h3>
      {/* Unavailable examples must never copy a blank or partial payload. */}
      {json !== undefined && <CopyButton label={label} value={json} />}
    </div>
    {/* Fields remain a schema tree; their copy action supplies editable values without adding another view. */}
    {view === "fields" ? <div className="max-h-96 flex-1 overflow-auto bg-[#161c27] p-4 [&>div]:text-sm [&>div]:leading-6"><SchemaViewer schema={schema} serviceId={appID} componentScope={appID} allowRemoteRefs={false} /></div> : <pre className="max-h-96 flex-1 overflow-auto p-4 text-sm leading-6 text-slate-700"><code>{json}</code></pre>}
  </div>;
}
