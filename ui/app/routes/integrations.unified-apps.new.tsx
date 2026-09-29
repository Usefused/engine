import type { ChooseDescribeService, DescribeProgress, AppDescription } from "~/lib/app-describe-contract";
import { TypeScriptEditor } from "~/components/code/TypeScriptEditor";
import { Select } from "../components/forms/Select.ts";
import { AppCreationFlow } from "~/components/apps/AppCreationFlow";
import { AppOperationPicker } from "~/components/apps/AppServiceBuilder";
import { describeSelectionKey, type AppServicePin } from "~/lib/app-describe-contract";
import { useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "@remix-run/react";
import { ArrowRight, Check, Loader2 } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { ExecutionTokenField } from "~/components/apps/ExecutionTokenField";
import { AppDetailBackLink } from "~/components/apps/AppDetailChrome";
import { describeUnifiedApp, listUnifiedTemplates, planUnifiedApp } from "~/lib/unified-app-api";
import { UnifiedSourceClarificationError, unifiedConfig, type UnifiedDraft } from "~/lib/unified-app-contract";
import { draftAppSource } from "~/lib/app-describe-api";
import { api } from "~/lib/api";
import { listAppBuildSelectors } from "~/lib/app-builder";
import { preferredAppBucket, type AppBuildSelector, type AppPlanResponse } from "~/lib/app-builder-contract";

const fieldClass = "w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]";
const buttonClass = "inline-flex items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:cursor-not-allowed disabled:opacity-50";

/** Owns the describe, review, compile and deploy boundaries without hiding a mutation behind drafting. */
export default function CreateUnifiedApp() {
  const { access } = useCurrentActorAccess();
  const [params] = useSearchParams();
  const templateID = params.get("template");
  const canCreate = hasWorkspacePermission(access, "app.unified_app.create");
  const [pickerSeed, setPickerSeed] = useState<Record<string, AppServicePin>>({});
  const [selecting, setSelecting] = useState(false);
  const [describing, setDescribing] = useState(false);
  const [draftingSource, setDraftingSource] = useState(false);
  const pendingDescription = useRef<{ goal: string; proposal: AppDescription } | null>(null);
  const [draft, setDraft] = useState<UnifiedDraft | null>(null);
  const [name, setName] = useState("");
  const [version, setVersion] = useState("1.0.0");
  const [bucket, setBucket] = useState("");
  const [buckets, setBuckets] = useState<AppBuildSelector[]>([]);
  const [plan, setPlan] = useState<AppPlanResponse | null>(null);
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState("");
  const [error, setError] = useState("");
  const [result, setResult] = useState<{ app_id: string; execution_token?: string } | null>(null);

  // Cancel stale reads when permissions or the selected immutable template change.
  useEffect(() => {
    let current = true;
    // Denied actors must not query credential selectors or private authoring content.
    if (!canCreate) return;
    setDraft(null); setPickerSeed({}); setPlan(null); setResult(null); setError("");
    listAppBuildSelectors("", "BUCKET").then((page) => {
      // Navigation may complete while a credential selector request is in flight.
      if (current) { setBuckets(page.items); setBucket((selected) => preferredAppBucket(page.items, selected)); }
    }).catch((cause) => { if (current) setError(String(cause)); });
    // Describe is the default entry; templates supply an explicit reviewed source baseline.
    if (templateID) {
      setBusy(true);
      listUnifiedTemplates("", 0, [templateID]).then((page) => {
        // An unavailable template cannot silently become a blank creation form.
        if (!page.items[0]) throw new Error("This template is no longer available.");
        if (current) { setDraft(page.items[0].template); setPickerSeed(page.items[0].template.services); setName(page.items[0].template.name); }
      }).catch((cause) => { if (current) setError(String(cause)); }).finally(() => { if (current) setBusy(false); });
    }
    return () => { current = false; };
  }, [canCreate, templateID]);

  /** Keeps edited inputs from reusing a plan that described different source or credentials. */
  function invalidatePlan() { setPlan(null); setError(""); }

  /** Exposes grounded selections immediately and resumes source-only retries without repeating discovery. */
  async function describe(goal: string, progress: DescribeProgress, chooseService: ChooseDescribeService) {
    const previous = { draft, pickerSeed, name };
    setDraftingSource(false);
    /** Reuses the existing picker and locks its incomplete source until drafting finishes. */
    function preview(proposal: AppDescription) {
      pendingDescription.current = { goal: goal.trim(), proposal };
      setDraftingSource(true);
      setDraft({ ...proposal, source: "" }); setPickerSeed(proposal.services); setName(proposal.name); invalidatePlan();
    }
    try {
      const cached = pendingDescription.current;
      let proposal: UnifiedDraft;
      // Only an unchanged failed description may reuse exact pins; Registry rechecks them before drafting.
      if (cached && cached.goal === goal.trim()) {
        preview(cached.proposal);
        proposal = { ...cached.proposal, source: await draftAppSource(goal, cached.proposal.services, progress) };
      } else {
        proposal = await describeUnifiedApp(goal, progress, chooseService, preview);
      }
      setDraft(proposal); setName(proposal.name); invalidatePlan(); pendingDescription.current = null;
      progress("Your source and selected operations are ready.", "review");
    } catch (cause) {
      // Clarification may require different capabilities; only transient failures reuse source-only retries.
      if (cause instanceof UnifiedSourceClarificationError) pendingDescription.current = null;
      // Failed regeneration preserves an earlier editable draft instead of erasing authored work.
      if (previous.draft?.source) { setDraft(previous.draft); setPickerSeed(previous.pickerSeed); setName(previous.name); }
      throw cause;
    } finally { setDraftingSource(false); }
  }

  /** Manual operation changes preserve authored TypeScript and invalidate any earlier compilation receipt. */
  function selectOperations(services: Record<string, AppServicePin>) {
    // Genuine manual edits invalidate a cached source retry; hydration of the same pins does not.
    if (pendingDescription.current && describeSelectionKey(services) !== describeSelectionKey(pendingDescription.current.proposal.services)) pendingDescription.current = null;
    setDraft((previous) => ({ name: "", description: "", source: "", ...previous, services }));
    invalidatePlan();
  }

  /** Keeps source edits in the existing draft and invalidates compilation before another deployment. */
  function updateSource(source: string) {
    // Source editing is only available after a grounded draft exists.
    if (!draft) return;
    setDraft({ ...draft, source }); invalidatePlan();
  }

  /** Creates a durable compilation plan only after the user reviews source and dependencies. */
  async function compile() {
    // No partially loaded template or failed description may enter the mutation boundary.
    if (!draft || selecting || describing) return;
    setBusy(true); setError("");
    try { setPlan(await planUnifiedApp(unifiedConfig(draft, name, version, bucket), draft.services, setProgress)); }
    catch (cause) { setError(String(cause)); }
    finally { setBusy(false); setProgress(""); }
  }

  /** Applies only the exact reviewed receipt and keeps the one-time execution token in memory. */
  async function deploy() {
    // A source edit invalidates the receipt and must force a fresh compilation.
    if (!plan || selecting || describing) return;
    setBusy(true); setError(""); setProgress("Deploying your Unified App…");
    try { setResult(await api.appConfig.apply("unified-app", { plan_id: plan.plan_id, source_hash: plan.source_hash })); }
    catch (cause) { setError(String(cause)); }
    finally { setBusy(false); setProgress(""); }
  }

  // Render permission failures separately from the describe and credential UI.
  if (!canCreate) return <div className="space-y-4"><AppDetailBackLink to="/integrations/sdks?type=unified_app" /><p className="text-slate-500">Unified App creation access is required.</p></div>;
  // Success deliberately retains the one-time token until the user leaves this page.
  if (result) return <CreatedUnifiedApp result={result} name={name} version={version} />;
  return <div className="mx-auto max-w-5xl space-y-6">
    <AppDetailBackLink to="/integrations/sdks?type=unified_app" />
    <header className="flex flex-wrap items-start justify-between gap-4"><div><h1 className="text-2xl font-bold text-slate-900">Create a Unified App</h1><p className="mt-1 text-slate-500">Describe what you want to build. Review it, then run it on Engine.</p></div><Link to="/integrations/unified-apps/templates" className="text-sm font-medium text-[var(--brand-violet)] hover:underline">Browse templates</Link></header>
    <AppCreationFlow generatesSource onDescribe={describe} onBusyChange={setDescribing} disabled={busy} initialManual={Boolean(templateID) || params.get("mode") === "manual"} hasSelection={Boolean(draft)}>
    <AppOperationPicker seed={pickerSeed} onChange={selectOperations} onPendingChange={setSelecting} />
    {/* Async stages are announced without replacing the user's editable description. */}
    {busy && <p role="status" className="flex items-center gap-2 text-sm text-slate-600"><Loader2 className="h-4 w-4 animate-spin" />{progress || "Loading template…"}</p>}
    {error && <p role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{error}</p>}
    {/* A grounded draft is the only entry to credential selection and deployment. */}
    {draft && <fieldset disabled={busy || describing || selecting} className="space-y-6 rounded-xl border border-slate-200 bg-white p-5 sm:p-6">
      <legend className="px-2 text-lg font-semibold text-slate-900">Unified App setup</legend>
      <p className="text-sm text-slate-500">{draft.description}</p>
      <div className="grid gap-4 sm:grid-cols-3"><label className="space-y-2 text-sm font-medium">App name<input className={fieldClass} value={name} onChange={(event) => { setName(event.target.value); invalidatePlan(); }} /></label><label className="space-y-2 text-sm font-medium">Version<input className={fieldClass} value={version} onChange={(event) => { setVersion(event.target.value); invalidatePlan(); }} /></label><label className="space-y-2 text-sm font-medium">Credential bucket<Select className={fieldClass} value={bucket} onChange={(event) => { setBucket(event.target.value); invalidatePlan(); }}><option value="">Choose a bucket</option>{buckets.map((item) => <option key={item.resource_id} value={item.resource_id}>{item.display_name}</option>)}</Select></label></div>
      <div className="space-y-2 text-sm font-medium"><label htmlFor="unified-app-source" className="block">TypeScript source</label>{/* The selection is reviewable while hosted source is still being generated. */}{draftingSource && !draft.source && <span role="status" className="block font-normal text-slate-500">Generating TypeScript from your selected operations…</span>}<TypeScriptEditor id="unified-app-source" value={draft.source} disabled={busy || describing || selecting} onChange={updateSource} /></div>
      <p className="text-xs text-slate-500">Review and compile enables any missing pinned service versions in this workspace. Deployment promotes this version to receive traffic. Credentials stay in the selected bucket.</p>
      {/* The plan is invalidated on every edit, so deployment cannot apply stale reviewed content. */}
      {plan ? <section className="space-y-4 rounded-lg border border-emerald-200 bg-emerald-50 p-4"><h2 className="font-semibold text-emerald-900">Ready to deploy</h2><pre className="max-h-60 overflow-auto whitespace-pre-wrap break-words text-xs text-slate-700">{JSON.stringify(plan.summary, null, 2)}</pre><button type="button" className={buttonClass} onClick={deploy}>Deploy Unified App <ArrowRight className="h-4 w-4" /></button></section> : <button type="button" className={buttonClass} disabled={!canCompile(name, version, bucket, draft.source) || !Object.values(draft.services).some((pin) => pin.operations.length)} onClick={compile}>Review and compile <ArrowRight className="h-4 w-4" /></button>}
    </fieldset>}
    </AppCreationFlow>
  </div>;
}

/** Groups the created identity, one-time credential and next step in one completion panel. */
function CreatedUnifiedApp({ result, name, version }: { result: { app_id: string; execution_token?: string }; name: string; version: string }) {
  const [copied, setCopied] = useState(false);

  /** Confirms copying without persisting the one-time credential beyond this page. */
  function confirmCopy() { setCopied(true); }

  return <div className="mx-auto max-w-3xl space-y-6">
    <AppDetailBackLink to="/integrations/sdks?type=unified_app" />
    <section className="overflow-hidden rounded-xl border border-slate-200 bg-white">
      <header className="space-y-4 p-5 sm:p-6">
        <div className="flex items-center gap-2 text-sm font-medium text-emerald-700"><Check className="h-4 w-4" aria-hidden="true" />Your Unified App is ready</div>
        <div className="flex flex-wrap items-center gap-3"><h1 className="text-2xl font-bold text-slate-900">{name}</h1><span className="rounded-md bg-slate-100 px-2 py-1 text-xs font-medium text-slate-600">v{version}</span></div>
        <p className="text-sm text-slate-500">Your app is deployed and ready to use.</p>
      </header>
      {/* Existing versions can complete without issuing a new credential. */}
      {result.execution_token && <div className="border-t border-slate-200 p-5 sm:p-6">
        <h2 className="text-sm font-semibold text-slate-900">Execution token</h2>
        <p className="mt-1 text-sm text-slate-500">Shown only once. Copy and store it securely before leaving this page.</p>
        <ExecutionTokenField token={result.execution_token} copied={copied} onCopy={confirmCopy} />
        {/* Announce successful copying without adding visual clutter. */}
        <span role="status" className="sr-only">{copied && "Execution token copied"}</span>
      </div>}
      <footer className="flex flex-wrap items-center justify-between gap-4 border-t border-slate-200 bg-slate-50 px-5 py-4 sm:px-6">
        <p className="text-sm text-slate-500">View activity, requests, and versions.</p>
        <Link className={buttonClass} to={`/integrations/unified-apps/${result.app_id}`}>Open app <ArrowRight className="h-4 w-4" aria-hidden="true" /></Link>
      </footer>
    </section>
  </div>;
}

/** All reviewed inputs must be present before dependency activation and compilation. */
function canCompile(name: string, version: string, bucket: string, source: string): boolean { return [name, version, bucket, source].every((value) => Boolean(value.trim())); }
