import type { ChooseDescribeService, DescribeProgress, AppDescription } from "~/lib/app-describe-contract";
import { FieldLabel } from "~/components/forms/FieldLabel";
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
import { unifiedEditDraft, unifiedEditConfig, unifiedEditSelections, nextUnifiedVersion, type UnifiedAppSource } from "~/lib/unified-app-edit";
import { listAppBuildSelectors } from "~/lib/app-builder";
import { preferredAppBucket, type AppBuildSelector, type AppPlanResponse } from "~/lib/app-builder-contract";

const fieldClass = "w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]";
const buttonClass = "inline-flex items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:cursor-not-allowed disabled:opacity-50";

/** Retains a reviewed registration across source regeneration without inventing one for a new draft. */
function draftAttachment(draft: UnifiedDraft | null): string { return draft?.webhookAttachment ?? ""; }

/** Detects whether the selected capability scope needs inbound registration coverage. */
function draftHasWebhookEvents(draft: UnifiedDraft): boolean {
  return Object.values(draft.services).some((pin) => Boolean(pin.webhooks?.length));
}

/** Gates validation on a complete event or operation scope and its required registration. */
function canValidateUnifiedDraft(draft: UnifiedDraft, name: string, version: string, bucket: string, savedVersion?: string): boolean {
  // Immutable successors require a new version before source can be compiled.
  if (version.trim() === savedVersion || !canCompile(name, version, bucket, draft.source)) return false;
  // An empty picker selection cannot create a callable or triggered app.
  if (!Object.values(draft.services).some((pin) => Boolean(pin.operations.length || pin.webhooks?.length))) return false;
  // Event-only and mixed scopes both need an applied registration name.
  return !draftHasWebhookEvents(draft) || Boolean(draft.webhookAttachment?.trim());
}

/** Shares labeled source authoring, selection, compilation, and deployment for new apps and immutable successors. */
export default function CreateUnifiedApp() {
  const { access } = useCurrentActorAccess();
  const [params] = useSearchParams();
  const editID = params.get("edit");
  // Private source takes precedence over a template supplied in the same URL.
  const templateID = editID ? null : params.get("template");
  const [editSource, setEditSource] = useState<UnifiedAppSource | null>(null);
  const [loadingSource, setLoadingSource] = useState(Boolean(editID));
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
  const [result, setResult] = useState<{ app_id: string; app_family_id: string; execution_token?: string } | null>(null);

  // Source authorization comes from the same private endpoint used by the other app editors.
  useEffect(() => {
    let current = true;
    // A source read enforces family manage permission; a new app still requires workspace create permission.
    if (!editID && !canCreate) return;
    setDraft(null); setEditSource(null); setPickerSeed({}); setPlan(null); setResult(null); setError("");
    setLoadingSource(Boolean(editID)); setBusy(true);
    pendingDescription.current = null;
    /** Loads the authoritative source before credential selectors and ignores responses after navigation. */
    async function load() {
      try {
        const saved = editID ? await api.appConfig.source<UnifiedAppSource>(editID) : null;
        const page = await listAppBuildSelectors("", "BUCKET");
        // A stale source cannot replace edits belonging to another route or immutable version.
        if (!current) return;
        setBuckets(page.items);
        // Editing preserves the family's credential binding, even when it is not in the first selector page.
        if (saved) {
          const baseline = unifiedEditDraft(saved);
          setEditSource(saved); setDraft(baseline); setPickerSeed(baseline.services);
          setName(baseline.name); setVersion(nextUnifiedVersion(saved.config.version)); setBucket(saved.config.bucket);
        } else {
          setName(""); setVersion("1.0.0"); setBucket(preferredAppBucket(page.items, ""));
          // Templates are explicit new-app baselines and never override an existing app's private source.
          if (templateID) {
            const templates = await listUnifiedTemplates("", 0, [templateID]);
            // A missing template must not silently become an empty creation form.
            if (!templates.items[0]) throw new Error("This template is no longer available.");
            // Template loading may complete after the user leaves this route.
            if (current) { setDraft(templates.items[0].template); setPickerSeed(templates.items[0].template.services); setName(templates.items[0].template.name); }
          }
        }
      } catch (cause) {
        // Only the current route may display errors from its private source request.
        if (current) setError(String(cause));
      } finally {
        // Unmounted requests cannot unlock or reset a newer editor.
        if (current) { setBusy(false); setLoadingSource(false); }
      }
    }
    void load();
    return () => { current = false; };
  }, [canCreate, templateID, editID]);

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
      setDraft({ ...proposal, source: "", webhookAttachment: draftAttachment(previous.draft) }); setPickerSeed(proposal.services);
      // Regeneration must never rename an existing family.
      if (!editID) setName(proposal.name);
      invalidatePlan();
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
      // Re-describing capabilities retains the explicitly chosen registration for review.
      setDraft({ ...proposal, webhookAttachment: draftAttachment(previous.draft) });
      // Existing family identity remains fixed when its implementation is regenerated.
      if (!editID) setName(proposal.name);
      invalidatePlan(); pendingDescription.current = null;
      progress("Your source and selected capabilities are ready.", "review");
    } catch (cause) {
      // Clarification may require different capabilities; only transient failures reuse source-only retries.
      if (cause instanceof UnifiedSourceClarificationError) pendingDescription.current = null;
      // Failed regeneration preserves an earlier editable draft instead of erasing authored work.
      if (previous.draft?.source) { setDraft(previous.draft); setPickerSeed(previous.pickerSeed); setName(previous.name); }
      throw cause;
    } finally { setDraftingSource(false); }
  }

  /** Manual operation changes retain saved aliases and source while invalidating earlier compilation receipts. */
  function selectOperations(services: Record<string, AppServicePin>) {
    // Existing source can refer to authored aliases that differ from the catalogue's qualified service names.
    if (editSource) services = unifiedEditSelections(editSource, services);
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

  /** Description edits belong to the new version and invalidate the same reviewed plan as source edits. */
  function updateDescription(description: string) {
    // Only a loaded draft can supply the rest of the immutable successor configuration.
    if (!draft) return;
    setDraft({ ...draft, description }); invalidatePlan();
  }

  /** Keeps the selected registration in the same reviewed draft as its event allowlist. */
  function updateWebhookAttachment(webhookAttachment: string) {
    // The attachment is meaningful only after service and event selection created a draft.
    if (!draft) return;
    setDraft({ ...draft, webhookAttachment }); invalidatePlan();
  }

  /** Resolves the selected credential name before compiling the reviewed source and dependencies. */
  async function compile() {
    // No partially loaded template or failed description may enter the mutation boundary.
    if (!draft || selecting || describing || (editID && !editSource)) return;
    setBusy(true); setError("");
    try {
      const selectedBucket = buckets.find((item) => item.resource_id === bucket);
      // Selectors use IDs for UI identity, while portable app configuration resolves buckets by name.
      if (!editSource && !selectedBucket) throw new Error("Choose an available credential bucket before compiling.");
      const config = editSource ? unifiedEditConfig(editSource, draft, version) : unifiedConfig(draft, name, version, selectedBucket!.display_name);
      setPlan(await planUnifiedApp(config, draft.services, setProgress, editSource?.owner_team));
    }
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

  // Deep-linked editors stay locked until the private source endpoint has authorized and returned the baseline.
  if (editID && editSource?.app_id !== editID) return <div className="space-y-4"><AppDetailBackLink to={`/integrations/unified-apps/${editID}`} />{loadingSource ? <p role="status">Loading app source…</p> : <p role="alert" className="text-red-700">{error || "App source is unavailable."}</p>}</div>;
  // Render permission failures separately from the describe and credential UI.
  if (!editID && !canCreate) return <div className="space-y-4"><AppDetailBackLink to="/integrations/sdks?type=unified_app" /><p className="text-slate-500">Unified App creation access is required.</p></div>;
  // Success deliberately retains the one-time token until the user leaves this page.
  if (result) return <CreatedUnifiedApp result={result} name={name} version={version} />;
  return <div className="mx-auto max-w-5xl space-y-6">
    <AppDetailBackLink to={editID ? `/integrations/unified-apps/${editID}` : "/integrations/sdks?type=unified_app"} />
    <header className="flex flex-wrap items-start justify-between gap-4"><div><h1 className="text-2xl font-bold text-slate-900">{editID ? "Edit Unified App" : "Create a Unified App"}</h1><p className="mt-1 text-slate-500">{editSource ? `Editing version ${editSource.config.version}. Deploy your changes as a new version; your app URL and tokens stay the same.` : "Describe what you want to build. Review it, then run it on Engine."}</p></div>{/* Templates start a separate app rather than replacing a saved editor baseline. */}{!editID && <Link to="/integrations/unified-apps/templates" className="text-sm font-medium text-[var(--brand-violet)] hover:underline">Browse templates</Link>}</header>
    <AppCreationFlow generatesSource onDescribe={describe} onBusyChange={setDescribing} disabled={busy} initialManual={Boolean(editID) || Boolean(templateID) || params.get("mode") === "manual"} hasSelection={Boolean(draft)}>
    <AppOperationPicker seed={pickerSeed} onChange={selectOperations} onPendingChange={setSelecting} allowWebhooks />
    {/* Async stages are announced without replacing the user's editable description. */}
    {busy && <p role="status" className="flex items-center gap-2 text-sm text-slate-600"><Loader2 className="h-4 w-4 animate-spin" />{progress || "Loading template…"}</p>}
    {error && <p role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{error}</p>}
    {/* A grounded draft is the only entry to credential selection and deployment. */}
    {draft && <fieldset aria-labelledby="unified-app-setup-heading" disabled={busy || describing || selecting} className="space-y-6 rounded-xl border border-slate-200 bg-white p-5 sm:p-6">
      {/* An interior heading keeps the card border continuous while naming the grouped controls accessibly. */}
      <h2 id="unified-app-setup-heading" className="text-lg font-semibold text-slate-900">Unified App setup</h2>
      <label className="block space-y-2 text-sm font-medium">
        <FieldLabel required>App name</FieldLabel>
        <input required className={fieldClass} readOnly={Boolean(editID)} value={name} onChange={(event) => { setName(event.target.value); invalidatePlan(); }} />
      </label>
      <label className="block space-y-2 text-sm font-medium">
        <FieldLabel>Description</FieldLabel>
        <textarea className={fieldClass} maxLength={1024} value={draft.description} onChange={(event) => updateDescription(event.target.value)} />
      </label>
      <div className="grid gap-4 sm:grid-cols-2">
        <label className="space-y-2 text-sm font-medium">
          {/* Existing apps require a successor version rather than an overwrite of their saved source. */}
          <FieldLabel required>{editID ? "New version" : "Version"}</FieldLabel>
          <input required className={fieldClass} value={version} onChange={(event) => { setVersion(event.target.value); invalidatePlan(); }} />
        </label>
        <label className="space-y-2 text-sm font-medium">
          <FieldLabel required>Credential bucket</FieldLabel>
          <Select required className={fieldClass} disabled={Boolean(editID)} value={bucket} onChange={(event) => { setBucket(event.target.value); invalidatePlan(); }}>
            {/* Existing families retain their bucket; new apps choose an authorized selector. */}
            {editSource ? <option value={editSource.config.bucket}>{editSource.config.bucket}</option> : <option value="">Choose a bucket</option>}
            {!editSource && buckets.map((item) => <option key={item.resource_id} value={item.resource_id}>{item.display_name}</option>)}
          </Select>
        </label>
      </div>
      {/* The worker subscribes through an applied registration; event names alone do not provision ingress. */}
      {draftHasWebhookEvents(draft) && <label className="block space-y-2 text-sm font-medium">
        <FieldLabel required>Webhook registration</FieldLabel>
        <input required className={fieldClass} placeholder="team-events" value={draftAttachment(draft)} onChange={(event) => updateWebhookAttachment(event.target.value)} />
        <span className="block text-xs font-normal text-slate-500">Enter a registration covering every selected event service. <Link className="text-[var(--brand-violet)] hover:underline" to="/integrations/webhooks/new" target="_blank" rel="noreferrer">Create a webhook registration</Link> if you need one.</span>
      </label>}
      <div className="space-y-2 text-sm font-medium"><label htmlFor="unified-app-source" className="block"><FieldLabel required>TypeScript source</FieldLabel></label>{/* The selection is reviewable while hosted source is still being generated. */}{draftingSource && !draft.source && <span role="status" className="block font-normal text-slate-500">Generating TypeScript from your selected operations and events…</span>}<TypeScriptEditor required id="unified-app-source" value={draft.source} disabled={busy || describing || selecting} onChange={updateSource} /></div>
      <p className="text-xs text-slate-500">Validate and compile checks TypeScript and selected operation bindings without running provider calls. It enables missing pinned service versions. Deploying is a separate step.</p>
      {/* The plan is invalidated on every edit, so deployment cannot apply stale reviewed content. */}
      {editSource && <p className="text-sm text-slate-600">Deploying switches new traffic to this version. Earlier versions remain available in version history.</p>}
      {plan ? <section className="space-y-4 rounded-lg border border-emerald-200 bg-emerald-50 p-4"><h2 className="font-semibold text-emerald-900">Ready to deploy</h2><p className="text-sm text-emerald-800">TypeScript validation and compilation passed. Provider operations have not been run.</p><pre className="max-h-60 overflow-auto whitespace-pre-wrap break-words text-xs text-slate-700">{JSON.stringify(plan.summary, null, 2)}</pre><button type="button" className={buttonClass} onClick={deploy}>{editID ? "Deploy new version" : "Deploy Unified App"} <ArrowRight className="h-4 w-4" /></button></section> : <button type="button" className={buttonClass} disabled={!canValidateUnifiedDraft(draft, name, version, bucket, editSource?.config.version)} onClick={compile}>Validate and compile <ArrowRight className="h-4 w-4" /></button>}
    </fieldset>}
    </AppCreationFlow>
  </div>;
}

/** Groups the created identity, one-time credential and next step in one completion panel. */
function CreatedUnifiedApp({ result, name, version }: { result: { app_id: string; app_family_id: string; execution_token?: string }; name: string; version: string }) {
  const [copied, setCopied] = useState(false);

  /** Confirms copying without persisting the one-time credential beyond this page. */
  function confirmCopy() { setCopied(true); }

  return <div className="mx-auto max-w-3xl space-y-6">
    <AppDetailBackLink to="/integrations/sdks?type=unified_app" />
    <section className="overflow-hidden rounded-xl border border-slate-200 bg-white">
      <header className="space-y-4 p-5 sm:p-6">
        <div className="flex items-center gap-2 text-sm font-medium text-emerald-700"><Check className="h-4 w-4" aria-hidden="true" />Your Unified App is ready</div>
        <div className="flex flex-wrap items-center gap-3"><h1 className="text-2xl font-bold text-slate-900">{name}</h1><span className="rounded-md bg-slate-100 px-2 py-1 text-xs font-medium text-slate-600">v{version}</span></div>
        <p className="text-sm text-slate-500">Your app is deployed. This URL follows the version receiving traffic.</p>
        <code className="block break-all rounded-lg bg-slate-50 p-3 text-xs">POST /v1/apps/{result.app_family_id}/executions</code>
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
