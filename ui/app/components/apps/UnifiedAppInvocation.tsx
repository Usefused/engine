import { Select } from "~/components/forms/Select";
import { UnifiedAppInput } from "./UnifiedAppInput";
import { useEffect, useMemo, useRef, useState } from "react";
import { CopyButton } from "~/components/CopyValue";
import { appExecutionURL } from "~/lib/app-execution-url";
import { unifiedInvocationBody, type RuntimeBucket } from "~/lib/unified-app-runtime-buckets";
import { unifiedInputFields } from "~/lib/unified-app-input-fields";
import { compileInputContract, validateInputText } from "~/lib/schedule-input-validation";

/** Runs schema-checked, committed input with a caller token; unfinished or invalid edits cannot execute. */
export function UnifiedAppInvocation({ appID, versionID, buckets, example, schema }: { appID: string; versionID: string; buckets: RuntimeBucket[]; example: string; schema?: unknown }) {
  const [bucketID, setBucketID] = useState("");
  // Declared fields start unset instead of sending fabricated example values to a provider.
  const [input, setInput] = useState(unifiedInputFields(schema) !== null ? "{}" : example);
  const [editingInput, setEditingInput] = useState(false);
  const [token, setToken] = useState("");
  const [result, setResult] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const validator = useMemo(() => compileInputContract(schema), [schema]);
  const validation = useMemo(() => validateInputText(input, validator), [input, validator]);
  // Unmounting ends local transport and prevents late results from reaching another app view.
  useEffect(() => () => controller.current?.abort(), []);
  let requestJSON = "";
  try {
    // Syntax and contract errors cannot become executable requests, even through the advanced JSON view.
    if (validation.parsed && validation.issues.length === 0) requestJSON = JSON.stringify({ ...unifiedInvocationBody(validation.value, bucketID, buckets), expected_app_id: versionID }, null, 2);
  } catch { /* Invalid bucket edits cannot be sent or copied as an executable request. */ }
  /** Sends exactly once because retrying a timed-out invocation can repeat provider effects. */
  async function run() {
    setBusy(true); setError(""); setResult("");
    const active = new AbortController(); controller.current = active;
    try {
      const endpoint = appExecutionURL(appID, window.__FUSED_ENV?.ENGINE_PUBLIC_URL || "");
      const response = await fetch(endpoint, { method: "POST", credentials: "omit", cache: "no-store", redirect: "error", signal: active.signal, headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` }, body: requestJSON });
      const body: unknown = await response.json();
      // Only this mounted invocation may display the result or clear its token.
      if (!active.signal.aborted) setResult(JSON.stringify(body, null, 2));
    } catch {
      // A network failure is an unknown outcome, so the UI never retries automatically.
      if (!active.signal.aborted) setError("The execution response is unavailable. Check Requests before trying again.");
    } finally {
      if (!active.signal.aborted) { setBusy(false); setToken(""); }
    }
  }
  return <details className="border-t border-slate-100 p-5"><summary className="cursor-pointer text-sm font-semibold">Run with a runtime bucket</summary>
    <div className="mt-4 space-y-3" data-fused-visible="false">
      <label className="block text-sm">Bucket<Select className="mt-1 block w-full rounded border p-2" disabled={busy} value={bucketID} onChange={(event) => setBucketID(event.target.value)}><option value="">Configured default and service buckets</option>{buckets.map((bucket) => <option key={bucket.id} value={bucket.id}>{bucket.name}</option>)}</Select></label>
      <p className="text-xs text-slate-500">An explicit selection applies to every provider call in this run. Engine checks the permitted scope and current access.</p>
      <UnifiedAppInput value={input} onChange={setInput} disabled={busy} onEditingChange={setEditingInput} schema={schema} />
      {/* Full-contract validation covers required fields, nested types, enums, and numeric constraints. */}
      {validation.issues.length > 0 && <ul aria-live="polite" className="space-y-1 text-xs text-red-700">{validation.issues.map((issue, index) => <li key={index}>{issue.path}: {issue.message}</li>)}</ul>}
      {schema !== undefined && validation.warning && <p className="text-xs text-amber-700">This schema cannot be checked in the browser. Engine will validate the input when you run.</p>}
      <label className="block text-sm">Execution token<input type="password" autoComplete="off" className="mt-1 block w-full rounded border p-2" disabled={busy} value={token} onChange={(event) => setToken(event.target.value)} /></label>
      {/* Pending chip edits must not run or copy the older committed request. */}
      <div className="flex items-center gap-3"><button type="button" disabled={busy || editingInput || !token.trim() || !requestJSON} onClick={() => void run()} className="rounded bg-slate-900 px-3 py-2 text-sm text-white disabled:opacity-50">{busy ? "Running…" : "Run app"}</button>{requestJSON && !editingInput && <CopyButton label="execution request JSON" value={requestJSON} />}</div>
      <p className="text-xs text-slate-500">Running executes the app’s provider operations. The token is cleared after the request and is never saved.</p>
      {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
      {result && <pre className="max-h-80 overflow-auto whitespace-pre-wrap text-xs">{result}</pre>}
    </div>
  </details>;
}
