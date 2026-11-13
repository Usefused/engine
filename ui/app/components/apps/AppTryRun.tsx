import { api } from "~/lib/api";
import { executeAppTry, TryTokenError } from "~/lib/app-try-execution";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission } from "~/lib/current-actor-access";
import { Select } from "~/components/forms/Select";
import { useEffect, useMemo, useRef, useState, type Dispatch, type SetStateAction } from "react";
import { Loader2, Play } from "lucide-react";
import { appExecutionURL } from "~/lib/app-execution-url";
import { appTryRequest, type TryOperation } from "~/lib/app-try-contract";
import { compileInputContract, validateInputText } from "~/lib/schedule-input-validation";
import { UnifiedAppInput } from "./UnifiedAppInput";
import { AppTryReadiness } from "./AppTryReadiness";
import { CopyButton } from "~/components/CopyValue";
import type { AppTryTarget } from "./AppTrySidebar";

const fieldClass = "mt-2 block w-full rounded-lg border border-slate-200 bg-white px-3 py-2.5 text-sm font-normal disabled:opacity-50";

/** Runs one exact operation using transient credentials and preserves uncertain outcomes without retrying. */
export function AppTryRun({ target, operation, onRunningChange }: { target: AppTryTarget; operation: TryOperation; onRunningChange: (running: boolean) => void }) {
  const [input, setInput] = useState("{}");
  const [editing, setEditing] = useState(false);
  const [bucketID, setBucketID] = useState("");
  const [token, setToken] = useState("");
  const [useToken, setUseToken] = useState(false);
  const { canGenerate, loading: accessLoading, failed: accessFailed, authorized } = useTryAuthorization(target, useToken, token);
  const [selector, setSelector] = useState({ end_user_ref: "", resource_id: "", environment: "" });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<{ text: string; status: number; elapsed: number; ok: boolean }>();
  const controller = useRef<AbortController | null>(null);
  const session = useRef<AbortController | null>(null);
  const validator = useMemo(/** Compiles once per immutable contract. */ () => compileInputContract(operation.input), [operation.input]);
  const validation = useMemo(/** Validates committed input before execution. */ () => validateInputText(input, validator), [input, validator]);
  // Session ownership outlives individual responses, so closing also revokes tokens from pending or uncertain runs.
  useEffect(() => {
    session.current = new AbortController();
    const close = () => { session.current?.abort(); controller.current?.abort(); };
    // Back/forward-cache restoration starts a fresh lifetime without reviving old credentials or requests.
    const restore = (event: PageTransitionEvent) => {
      if (!event.persisted) return;
      session.current = new AbortController(); controller.current = null; setBusy(false); setToken(""); onRunningChange(false);
    };
    window.addEventListener("pagehide", close);
    window.addEventListener("pageshow", restore);
    return () => { window.removeEventListener("pagehide", close); window.removeEventListener("pageshow", restore); close(); onRunningChange(false); };
  }, [onRunningChange]);
  const requestJSON = tryRequestJSON(operation, validation, editing, bucketID, target, selector);
  /** Sends once through runtime token authentication; management cookies never authorize execution. */
  async function run() {
    // A synchronous in-flight guard also prevents rapid double clicks before React paints.
    if (controller.current || !requestJSON || !authorized) return;
    const active = new AbortController(); controller.current = active;
    setBusy(true); onRunningChange(true); setError(""); setResult(undefined);
    const started = performance.now();
    try {
      const response = await sendTryRequest(target, operation, requestJSON, active.signal, useToken, token, session.current!.signal);
      const formatted = formatTryResponse(await response.text());
      // Responses from a closed or replaced session must never appear in another operation's panel.
      if (!active.signal.aborted) setResult({ text: formatted, status: response.status, elapsed: Math.round(performance.now() - started), ok: response.ok });
    } catch (cause) {
      // A transport failure cannot prove that provider effects did not happen.
      if (!active.signal.aborted) setError(tryRunError(cause));
    } finally {
      // Token material is intentionally never persisted and is cleared after every completed attempt.
      if (!active.signal.aborted) { setBusy(false); onRunningChange(false); setToken(""); controller.current = null; }
    }
  }
  // Each app kind keeps its existing Requests destination.
  const requestsURL = target.unified ? `/integrations/unified-apps/${target.versionID}?tab=requests` : `/integrations/sdks/${target.versionID}?tab=activity&activity=requests`;
  return <div className="space-y-6" data-fused-visible="false">
    <TryRoutingFields target={target} operation={operation} busy={busy} bucketID={bucketID} setBucketID={setBucketID} selector={selector} setSelector={setSelector} clearResult={/** Routing changes invalidate the response context. */ () => setResult(undefined)} />
    <section className="space-y-3 border-t border-slate-200 pt-5">
      <UnifiedAppInput schema={operation.input} value={input} onChange={setInput} disabled={busy} onEditingChange={setEditing} />
      <TryInputValidation validation={validation} />
    </section>
    <section className="space-y-3 border-t border-slate-200 pt-5">
      <TryAuthentication useToken={useToken} onModeChange={value => { setUseToken(value); setToken(""); setError(""); }} token={token} setToken={setToken} busy={busy} canGenerate={canGenerate} loading={accessLoading} failed={accessFailed} />
      <p className="text-xs leading-5 text-slate-500">Running can create or change real provider data.</p>
      {/* Busy, invalid, and unauthenticated requests stay disabled; copy uses only validated JSON. */}
      <div className="flex flex-wrap items-center gap-3"><button type="button" disabled={busy || !authorized || !requestJSON} onClick={/** Only explicit user action starts execution. */ () => void run()} className="inline-flex min-h-11 items-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white shadow-sm transition-colors hover:bg-slate-800 disabled:opacity-40">{busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <Play className="h-4 w-4" />}{busy ? "Running…" : "Run"}</button>{requestJSON && <CopyButton label="request JSON" value={requestJSON} />}<a href={requestsURL} target="_blank" rel="noreferrer" className="ml-auto text-xs font-medium text-slate-950 hover:underline">View Requests ↗</a></div>
    </section>
    {/* Transport failures remain distinct from HTTP failures. */}
    {error && <p role="alert" className="rounded-lg bg-amber-50 p-4 text-sm text-amber-900">{error}</p>}
    <TryResponse result={result} />
  </div>;
}

/** Prevents invalid input or stale routing from becoming a copied or executable request. */
function tryRequestJSON(operation: TryOperation, validation: ReturnType<typeof validateInputText>, editing: boolean, bucketID: string, target: AppTryTarget, selector: Record<string, string>): string {
  // Incomplete edits must not execute the older committed JSON payload.
  if (!validation.parsed || validation.issues.length || editing) return "";
  try { return JSON.stringify(appTryRequest(operation, validation.value, bucketID, target.versionID, target.unified, selector), null, 2); }
  catch { return ""; /* A scope mismatch requires an explicit corrected selection. */ }
}

/** Preserves plain text errors while formatting structured Engine responses for inspection. */
function formatTryResponse(text: string): string {
  try { return JSON.stringify(JSON.parse(text), null, 2); }
  catch { return text; /* Non-JSON failures must retain their actual response. */ }
}

type TrySelector = { end_user_ref: string; resource_id: string; environment: string };
/** Keeps connection controls separate from the authored/provider input contract. */
function TryRoutingFields({ target, operation, busy, bucketID, setBucketID, selector, setSelector, clearResult }: { target: AppTryTarget; operation: TryOperation; busy: boolean; bucketID: string; setBucketID: (id: string) => void; selector: TrySelector; setSelector: Dispatch<SetStateAction<TrySelector>>; clearResult: () => void }) {
  return (
    <section className="space-y-3">
      <label className="block text-sm font-semibold">Bucket<Select searchable searchPlaceholder="Search permitted buckets…" aria-label="Bucket" disabled={busy || operation.buckets.length === 0} value={bucketID} onChange={/** Changes invocation routing without editing app bindings. */ event => { setBucketID(event.target.value); clearResult(); }} className={fieldClass}><option value="">Configured default and service buckets</option>{operation.buckets.map(/** Offers only deployed scope. */ bucket => <option key={bucket.id} value={bucket.id}>{bucket.name}</option>)}</Select></label>
      {/* Fixed apps remain runnable without silently expanding their deployed credential scope. */}
      <p className="text-xs leading-5 text-slate-500">{operation.buckets.length ? "Choose a permitted bucket for this run, or keep the app’s configured routing." : "This version uses its configured buckets. Publish a version with permitted runtime buckets to choose another."}</p>
      {/* Physical selectors are execution controls; Unified App user references belong to its authored input. */}
      {operation.selector && <><label className="block text-sm font-semibold">Connected user <span className="font-normal text-slate-400">· for OAuth</span><input aria-label="Connected user" autoComplete="off" value={selector.end_user_ref} disabled={busy} onChange={/** Keeps routing controls separate from operation input. */ event => setSelector({ ...selector, end_user_ref: event.target.value })} placeholder="Enter the connected user reference" maxLength={256} className={fieldClass} /></label>
        <details className="text-sm"><summary className="cursor-pointer text-slate-500">Resource and environment</summary><div className="mt-3 space-y-3"><label className="block">Resource ID<input aria-label="Resource ID" value={selector.resource_id} disabled={busy} onChange={/** Keeps routing controls separate from operation input. */ event => setSelector({ ...selector, resource_id: event.target.value })} placeholder="Optional resource UUID" className={fieldClass} /></label><label className="block">Environment<input aria-label="Environment" value={selector.environment} disabled={busy} onChange={/** Keeps routing controls separate from operation input. */ event => setSelector({ ...selector, environment: event.target.value })} maxLength={256} placeholder="Configured default" className={fieldClass} /></label></div></details></>}
      <AppTryReadiness versionID={target.versionID} operation={operation.name} bucketID={bucketID} userRef={selector.end_user_ref} unified={target.unified} />
    </section>
  );
}

/** Reports schema errors without echoing private values or pretending an unchecked schema passed. */
function TryInputValidation({ validation }: { validation: ReturnType<typeof validateInputText> }) {
  return <>
      {/* Full schema validation covers nested shapes and constraints beyond the visible controls. */}
      {validation.issues.length > 0 && <ul aria-live="polite" className="space-y-1 text-xs text-amber-800">{validation.issues.map(/** Reports constraints without private values. */ (issue, index) => <li key={index}>{issue.path}: {issue.message}</li>)}</ul>}
      {/* Unsupported schemas remain visibly unchecked until Engine validation. */}
      {validation.warning && <p className="text-xs text-amber-800">This schema cannot be checked in the browser. Engine will validate when you run.</p>}
  </>;
}

/** Keeps the response attached to its HTTP outcome, including empty and non-JSON failures. */
function TryResponse({ result }: { result?: { text: string; status: number; elapsed: number; ok: boolean } }) {
  return <>
    {/* Preserve both successful and failed HTTP bodies, including empty responses. */}
    {result && <section aria-label="Execution response" className="overflow-hidden rounded-xl border border-slate-200"><div className="flex items-center justify-between gap-3 bg-slate-50 px-4 py-3"><h3 className="text-sm font-semibold">Response</h3><p role="status" className={`text-xs ${result.ok ? "text-emerald-700" : "text-red-700"}`}>HTTP {result.status} · {result.elapsed} ms</p></div><pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words bg-slate-950 p-4 text-xs leading-6 text-slate-200">{result.text || "Empty response"}</pre></section>}
  </>;
}

/** Defaults to the signed-in user's existing issuance authority, with an explicit caller-token override. */
function TryAuthentication({ useToken, onModeChange, token, setToken, busy, canGenerate, loading, failed }: { useToken: boolean; onModeChange: (value: boolean) => void; token: string; setToken: (value: string) => void; busy: boolean; canGenerate: boolean; loading: boolean; failed: boolean }) {
  let message = "Fused authorizes this test automatically using your app permissions.";
  if (loading) message = "Checking your app permissions…";
  else if (failed) message = "Could not check your permissions. Refresh the page or use an execution token.";
  else if (!canGenerate) message = "You need permission to manage execution tokens for this app to test automatically. You can also use an execution token.";
  return <div className="space-y-3">
    {!useToken && <p role="status" className="text-xs leading-5 text-slate-500">{message}</p>}
    <label className="flex items-center gap-2 text-sm font-medium text-slate-700"><input type="checkbox" checked={useToken} disabled={busy} onChange={event => onModeChange(event.target.checked)} className="h-4 w-4 rounded border-slate-300 accent-slate-950" />Use execution token</label>
    {useToken && <><label className="block text-sm font-semibold">Execution token<input aria-label="Execution token" type="password" autoComplete="off" value={token} onChange={event => setToken(event.target.value)} disabled={busy} placeholder="Paste an app execution token" className={fieldClass} /></label><p className="text-xs leading-5 text-slate-500">Your token stays in this sidebar and is cleared after each run.</p></>}
  </div>;
}

/** Uses the same exact family/type permission as the ordinary execution-token controls. */
function useTryAuthorization(target: AppTryTarget, useToken: boolean, token: string) {
  const { access, loading, failed } = useCurrentActorAccess();
  const canGenerate = hasResourcePermission(access, `app.${target.unified ? "unified_app" : "api"}.tokens.manage`, "APP", target.familyID);
  return { canGenerate, loading, failed, authorized: useToken ? !!token.trim() : !loading && !failed && canGenerate };
}

/** Resolves automatic or supplied credentials without sending control-plane cookies to execution. */
function sendTryRequest(target: AppTryTarget, operation: TryOperation, body: string, signal: AbortSignal, useToken: boolean, token: string, sessionSignal: AbortSignal) {
  return executeAppTry({
    familyID: target.familyID, operation: operation.name, token: useToken ? token.trim() : undefined,
    url: appExecutionURL(target.executionID, window.__FUSED_ENV?.ENGINE_PUBLIC_URL || ""), body, signal, sessionSignal,
  }, api.appTokens);
}

/** Distinguishes a pre-dispatch authorization failure from an uncertain runtime response. */
function tryRunError(cause: unknown): string {
  return cause instanceof TryTokenError ? cause.message : "The response is unavailable. The operation may have run. Check Requests before running again.";
}
