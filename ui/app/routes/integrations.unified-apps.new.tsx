import type { ChooseDescribeService, DescribeProgress, AppDescription } from "~/lib/app-describe-contract";
import { FieldLabel } from "~/components/forms/FieldLabel";
import { AppCredentialWarning } from "~/components/apps/AppCredentialWarning";
import { useAppCredentialReview } from "~/components/apps/useAppCredentialReview";
import { UnifiedAppCodeEditor } from "~/components/apps/UnifiedAppCodeEditor";
import { UnifiedAppAIAssistant } from "~/components/apps/UnifiedAppAIAssistant";
import { AppServiceAuthFields, type AppAuthSelection } from "~/components/apps/AppServiceAuthFields";
import { unifiedEditorYAML, readUnifiedEditorYAML } from "~/lib/unified-app-yaml";
import { CreateCredentialButton } from "~/components/buckets/CreateCredentialButton";
import { useCredentialSetCreation } from "~/components/buckets/useCredentialSetCreation";
import { Select } from "../components/forms/Select.ts";
import { AppCreationFlow } from "~/components/apps/AppCreationFlow";
import { AppOperationPicker } from "~/components/apps/AppServiceBuilder";
import { UnifiedAppCompileAction } from "~/components/apps/UnifiedAppCompileAction";
import { describeSelectionKey, type AppServicePin } from "~/lib/app-describe-contract";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useSearchParams } from "@remix-run/react";
import { ArrowRight, Check, Loader2 } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { ExecutionTokenField } from "~/components/apps/ExecutionTokenField";
import { ExecutionTokenOption } from "~/components/apps/ExecutionTokenOption";
import { AppDetailBackLink } from "~/components/apps/AppDetailChrome";
import { describeUnifiedApp, listUnifiedTemplates, planUnifiedApp } from "~/lib/unified-app-api";
import { UnifiedSourceClarificationError, unifiedConfig, unifiedDraftHasEvents, unifiedServiceSettings, type UnifiedDraft } from "~/lib/unified-app-contract";
import { draftAppSource } from "~/lib/app-describe-api";
import { api } from "~/lib/api";
import { unifiedEditDraft, unifiedEditConfig, unifiedEditSelections, nextUnifiedVersion, type UnifiedAppSource } from "~/lib/unified-app-edit";
import { applyApp, listAppBuildSelectors } from "~/lib/app-builder";
import { preferredAppBucket, type AppBuildSelector, type AppPlanResponse } from "~/lib/app-builder-contract";

const fieldClass = "w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]";
const buttonClass = "inline-flex items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:cursor-not-allowed disabled:opacity-50";

/** Retains a reviewed registration across source regeneration without inventing one for a new draft. */
function draftAttachment(draft: UnifiedDraft | null): string { return draft?.webhookAttachment ?? ""; }

/** Detects whether the selected capability scope needs inbound registration coverage. */
function draftHasWebhookEvents(draft: UnifiedDraft): boolean {
  return unifiedDraftHasEvents(draft);
}

/** Gates validation on a complete event or operation scope and its required registration. */
function canValidateUnifiedDraft(draft: UnifiedDraft, name: string, version: string, bucket: string, savedVersion?: string): boolean {
  // Immutable successors require a new version before source can be compiled.
  if (version.trim() === savedVersion || !canCompile(name, version, bucket, draft.source)) return false;
  // An empty picker selection cannot create a callable or triggered app.
  if (!Object.values(draft.services).some((pin) => Boolean(pin.operations.length)) && !draftHasWebhookEvents(draft)) return false;
  // Event-only and mixed scopes both need an applied registration name.
  return !draftHasWebhookEvents(draft) || Boolean(draft.webhookAttachment?.trim());
}

/** Shares labeled source authoring, selection, compilation, and deployment for new apps and immutable successors. */
export default function CreateUnifiedApp() {
  const { access } = useCurrentActorAccess();
  const credentialReview = useAppCredentialReview();
  const compiledInput = useRef<{ config: Record<string, unknown>; services: UnifiedDraft["services"]; ownerTeam?: string } | null>(null);
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
  const [generateExecutionToken, setGenerateExecutionToken] = useState(true);
  const [buckets, setBuckets] = useState<AppBuildSelector[]>([]);
  // A newly created set becomes the selection and invalidates any earlier compilation receipt.
  const credentialCreation = useCredentialSetCreation("", (item) => {
    setBuckets((current) => [...current.filter((bucket) => bucket.resource_id !== item.resource_id), item]);
    setBucket(item.resource_id); invalidatePlan();
  });
  const yamlBaseline = useRef<UnifiedDraft | null>(null);
  const [yamlActive, setYAMLActive] = useState(false);
  const [yamlError, setYAMLError] = useState("");
  const [plan, setPlan] = useState<AppPlanResponse | null>(null);
  const [busy, setBusy] = useState(false);
  const [compiling, setCompiling] = useState(false);
  const [progress, setProgress] = useState("");
  const [error, setError] = useState("");
  const [result, setResult] = useState<{ app_id: string; app_family_id: string; execution_token?: string } | null>(null);

  // Source authorization comes from the same private endpoint used by the other app editors.
  useEffect(() => {
    let current = true;
    // A source read enforces family manage permission; a new app still requires workspace create permission.
    if (!editID && !canCreate) return;
    setYAMLError(""); setYAMLActive(false);
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
  function invalidatePlan() { compiledInput.current = null; setPlan(null); setError(""); }

  /** Exposes grounded selections immediately and resumes source-only retries without repeating discovery. */
  async function describe(goal: string, progress: DescribeProgress, chooseService: ChooseDescribeService) {
    const previous = { draft, pickerSeed, name };
    setDraftingSource(false);
    /** Reuses the existing picker and locks its incomplete source until drafting finishes. */
    function preview(proposal: AppDescription) {
      pendingDescription.current = { goal: goal.trim(), proposal };
      setDraftingSource(true);
      // Re-describing keeps reviewed auth settings bound to their original provider identities.
      const services = editSource ? unifiedEditSelections(editSource, proposal.services) : proposal.services;
      setDraft({ ...proposal, services, serviceSettings: previous.draft?.serviceSettings, configSettings: previous.draft?.configSettings, source: "", webhookAttachment: draftAttachment(previous.draft) }); setPickerSeed(services);
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
      // Retain saved aliases and auth after regeneration just as manual operation edits do.
      const services = editSource ? unifiedEditSelections(editSource, proposal.services) : proposal.services;
      setDraft({ ...proposal, services, serviceSettings: previous.draft?.serviceSettings, configSettings: previous.draft?.configSettings, webhookAttachment: draftAttachment(previous.draft) });
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

  /** Pins the same service auth pair used by SDK and MCP config without modifying stored secrets. */
  function updateAuth(key: string, auth?: AppAuthSelection) {
    // A delayed selector cannot update a service removed from the draft.
    if (!draft?.services[key]) return;
    const config = { ...unifiedServiceSettings(draft, key) };
    // Omitting auth deliberately restores the provider's declared order.
    if (auth) config.auth = auth;
    else delete config.auth;
    setDraft({ ...draft, serviceSettings: { ...draft.serviceSettings, [key]: { service_id: draft.services[key].service_id, config } } });
    invalidatePlan();
  }

  /** Keeps the original pins available for YAML undo and hydrates the picker only after editing finishes. */
  function changeEditorView(yaml: boolean) {
    // YAML edits may remove and restore entries without needing to rediscover the same provider.
    if (yaml) yamlBaseline.current = draft;
    else if (draft) setPickerSeed(draft.services);
    setYAMLActive(yaml);
  }

  /** Applies valid YAML to the same form draft; invalid text cannot reuse an earlier deployment plan. */
  function updateYAML(raw: string) {
    invalidatePlan();
    // YAML is only available after service selection has established immutable pins.
    if (!draft) return;
    try {
      const parsed = readUnifiedEditorYAML(raw, { ...draft, services: yamlBaseline.current?.services ?? draft.services }, editSource);
      const selected = buckets.find((item) => item.display_name === parsed.bucket);
      // New apps may only choose buckets available to the current builder actor.
      if (!editSource && !selected) throw new Error("Choose an available bucket by name.");
      setDraft(parsed.draft); setName(parsed.name); setVersion(parsed.version);
      setBucket(editSource ? parsed.bucket : selected!.resource_id);
      setYAMLError("");
    } catch (cause) { setYAMLError(cause instanceof Error ? cause.message : String(cause)); }
  }

  /** Shows immediate local feedback while resolving credentials and compiling the reviewed draft. */
  async function compile() {
    // Pending work and incomplete drafts cannot start a second compilation request.
    if (busy || yamlError || !draft || selecting || describing || (editID && !editSource)) return;
    setCompiling(true); setBusy(true); setError(""); setProgress("Checking selected services…");
    try {
      const selectedBucket = buckets.find((item) => item.resource_id === bucket);
      // Selectors use IDs for UI identity, while portable app configuration resolves buckets by name.
      if (!editSource && !selectedBucket) throw new Error("Choose an available bucket before compiling.");
      const config = editSource ? unifiedEditConfig(editSource, draft, version) : unifiedConfig(draft, name, version, selectedBucket!.display_name);
      setPlan(await planUnifiedApp(config, draft.services, setProgress, editSource?.owner_team));
      compiledInput.current = { config, services: draft.services, ownerTeam: editSource?.owner_team };
    }
    catch (cause) { setError(String(cause)); }
    // Success and failure both restore the action so errors can be corrected and retried.
    finally { setCompiling(false); setBusy(false); setProgress(""); }
  }

  /** Applies only the exact reviewed receipt and keeps the one-time execution token in memory. */
  async function deploy() {
    // A source edit invalidates the receipt and must force a fresh compilation.
    if (!plan || !compiledInput.current || busy || yamlError || selecting || describing) return;
    const input = compiledInput.current;
    setBusy(true); setError(""); setProgress("Reviewing credential setup…");
    try {
      /** Rechecking reuses the exact reviewed source and service identities while refreshing mutable credentials. */
      const recheck = async () => {
        const fresh = await planUnifiedApp(input.config, input.services, () => {}, input.ownerTeam);
        setPlan(fresh);
        return fresh;
      };
      const approved = await credentialReview.review(plan, recheck);
      // Cancelling the warning leaves the compiled draft intact and never deploys it.
      if (!approved) return;
      setProgress("Deploying your Unified App…");
      setResult(await applyApp("unified-app", approved, generateExecutionToken));
    }
    catch (cause) { setError(String(cause)); }
    finally { setBusy(false); setProgress(""); }
  }

  // Deep-linked editors stay locked until the private source endpoint has authorized and returned the baseline.
  if (editID && editSource?.app_id !== editID) return <div className="space-y-4"><AppDetailBackLink to={`/integrations/unified-apps/${editID}`} />{loadingSource ? <p role="status">Loading app source…</p> : <p role="alert" className="text-red-700">{error || "App source is unavailable."}</p>}</div>;
  // Render permission failures separately from the describe and credential UI.
  if (!editID && !canCreate) return <div className="space-y-4"><AppDetailBackLink to="/integrations/sdks?type=unified_app" /><p className="text-slate-500">Unified App creation access is required.</p></div>;
  // Success deliberately retains the one-time token until the user leaves this page.
  if (result) return <CreatedUnifiedApp result={result} name={name} version={version} credentialWarning={credentialReview.warning} />;
  return <div className="mx-auto max-w-5xl space-y-6">
    <AppDetailBackLink to={editID ? `/integrations/unified-apps/${editID}` : "/integrations/sdks?type=unified_app"} />
    <header className="flex flex-wrap items-start justify-between gap-4"><div><h1 className="text-2xl font-bold text-slate-900">{editID ? "Edit Unified App" : "Create a Unified App"}</h1><p className="mt-1 text-slate-500">{editSource ? `Editing version ${editSource.config.version}. Deploy your changes as a new version; your app URL and tokens stay the same.` : "Describe what you want to build. Review it, then run it on Fused."}</p></div>{/* Templates start a separate app rather than replacing a saved editor baseline. */}{!editID && <Link to="/integrations/unified-apps/templates" className="text-sm font-medium text-[var(--brand-violet)] hover:underline">Browse templates</Link>}</header>
    <AppCreationFlow generatesSource onDescribe={describe} onBusyChange={setDescribing} disabled={busy || describing} lockIntent={yamlActive} initialManual={Boolean(editID) || Boolean(templateID) || params.get("mode") === "manual"} hasSelection={Boolean(draft)}>
    <fieldset disabled={yamlActive || busy || describing} className="min-w-0"><AppOperationPicker seed={pickerSeed} onChange={selectOperations} onPendingChange={setSelecting} allowWebhooks /></fieldset>
    {/* Compilation reports progress by its button; other loading stages remain above the form. */}
    {busy && !compiling && <p role="status" className="flex items-center gap-2 text-sm text-slate-600"><Loader2 className="h-4 w-4 animate-spin" />{progress || "Loading template…"}</p>}
    {error && <p role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{error}</p>}
    {/* A grounded draft is the only entry to credential selection and deployment. */}
    {draft && <fieldset aria-labelledby="unified-app-setup-heading" disabled={busy || describing || selecting} className="space-y-6 rounded-xl border border-slate-200 bg-white p-5 sm:p-6">
      {/* An interior heading keeps the card border continuous while naming the grouped controls accessibly. */}
      <h2 id="unified-app-setup-heading" className="text-lg font-semibold text-slate-900">Unified App setup</h2>
      <fieldset disabled={yamlActive} className="space-y-6">
      <label className="block space-y-2 text-sm font-medium">
        <FieldLabel required>App name</FieldLabel>
        <input required className={fieldClass} readOnly={Boolean(editID)} value={name} onChange={(event) => { setName(event.target.value); invalidatePlan(); }} />
      </label>
      <label className="block space-y-2 text-sm font-medium">
        <FieldLabel>Description</FieldLabel>
        <textarea className={fieldClass} maxLength={1024} value={draft.description} onChange={(event) => updateDescription(event.target.value)} />
      </label>
      {/* Stack complete fields before the bucket action can wrap and offset the neighboring input. */}
      <div className="flex flex-wrap gap-4">
        <label className="min-w-0 flex-[1_1_16rem] space-y-2 text-sm font-medium">
          {/* Existing apps require a successor version rather than an overwrite of their saved source. */}
          <FieldLabel required>{editID ? "New version" : "Version"}</FieldLabel>
          <input required className={fieldClass} value={version} onChange={(event) => { setVersion(event.target.value); invalidatePlan(); }} />
        </label>
        <div className="min-w-0 flex-[1_1_16rem] space-y-2 text-sm font-medium">
          <div className="flex flex-wrap items-center justify-between gap-3">
            {/* Fill the available header width so Required aligns with the neighboring version badge. */}
            <label className="min-w-0 flex-1" htmlFor="unified-credential-bucket"><FieldLabel required>Bucket</FieldLabel></label>
            {/* Immutable successors cannot change the family's credential binding. */}
            {!editID && credentialCreation.create && <CreateCredentialButton onClick={credentialCreation.create} />}
          </div>
          <Select id="unified-credential-bucket" required className={fieldClass} disabled={Boolean(editID)} value={bucket} onChange={(event) => { setBucket(event.target.value); invalidatePlan(); }}>
            {/* Existing families retain their bucket; new apps choose an authorized selector. */}
            {editSource ? <option value={editSource.config.bucket}>{editSource.config.bucket}</option> : <option value="">Choose a bucket</option>}
            {!editSource && buckets.map((item) => <option key={item.resource_id} value={item.resource_id}>{item.display_name}</option>)}
          </Select>
        </div>
      </div>
      {/* The worker subscribes through an applied registration; event names alone do not provision ingress. */}
      {draftHasWebhookEvents(draft) && <label className="block space-y-2 text-sm font-medium">
        <FieldLabel required>Webhook registration</FieldLabel>
        <input required className={fieldClass} placeholder="team-events" value={draftAttachment(draft)} onChange={(event) => updateWebhookAttachment(event.target.value)} />
        <span className="block text-xs font-normal text-slate-500">Enter a registration covering every selected event service. <Link className="text-[var(--brand-violet)] hover:underline" to="/integrations/webhooks/new" target="_blank" rel="noreferrer">Create a webhook registration</Link> if you need one.</span>
      </label>}
      <AppServiceAuthFields services={Object.entries(draft.services).filter(([, pin]) => pin.operations.length).map(([key, pin]) => ({ key, service_id: pin.service_id, version: pin.version, auth: unifiedServiceSettings(draft, key).auth as AppAuthSelection | undefined }))} onChange={updateAuth} disabled={yamlActive || busy || describing || selecting} />
      </fieldset>
      {/* Source drafting keeps the service review visible until code generation finishes. */}
      {draftingSource && !draft.source && <p role="status" className="text-sm text-slate-500">Generating TypeScript from your selected operations and events…</p>}
      <UnifiedAppCodeEditor source={draft.source} yaml={unifiedEditorYAML(draft, name, version, editSource?.config.bucket ?? buckets.find((item) => item.resource_id === bucket)?.display_name ?? "", editSource)} disabled={busy || describing || selecting} error={yamlError} onSource={updateSource} onYAML={updateYAML} onViewChange={changeEditorView} />
      {/* AI edits retain the exact selected contracts and require explicit source review before recompilation. */}
      <UnifiedAppAIAssistant source={draft.source} services={draft.services} disabled={busy || describing || selecting || yamlActive || Boolean(yamlError)} onApply={updateSource} onBusyChange={setDescribing} />
      <p className="text-xs text-slate-500">Validate and compile checks TypeScript and selected operation bindings without running provider calls. It enables missing pinned service versions. Deploying is a separate step.</p>
      {/* The plan is invalidated on every edit, so deployment cannot apply stale reviewed content. */}
      {editSource && <p className="text-sm text-slate-600">Deploying switches new traffic to this version. Earlier versions remain available in version history.</p>}
      {/* Initial issuance is an apply choice; changing it needs no recompilation and never rotates existing tokens. */}
      {!editID && <ExecutionTokenOption checked={generateExecutionToken} onChange={setGenerateExecutionToken} />}
      {plan ? <section className="space-y-4 rounded-lg border border-emerald-200 bg-emerald-50 p-4"><h2 className="font-semibold text-emerald-900">Ready to deploy</h2><p className="text-sm text-emerald-800">TypeScript validation and compilation passed. Provider operations have not been run.</p><AppCredentialWarning readiness={plan.credential_readiness} canManageBucket={credentialReview.canManageBucket} /><pre className="max-h-60 overflow-auto whitespace-pre-wrap break-words text-xs text-slate-700">{JSON.stringify(plan.summary, null, 2)}</pre><button type="button" className={buttonClass} onClick={deploy}>{editID ? "Deploy new version" : "Deploy Unified App"} <ArrowRight className="h-4 w-4" /></button></section> : <UnifiedAppCompileAction className={buttonClass} compiling={compiling} progress={progress} disabled={Boolean(yamlError) || !canValidateUnifiedDraft(draft, name, version, bucket, editSource?.config.version)} onCompile={compile} />}
    </fieldset>}
    </AppCreationFlow>
    {credentialCreation.dialog}
    {credentialReview.dialog}
  </div>;
}

/** Groups the created identity, one-time credential and next step in one completion panel. */
function CreatedUnifiedApp({ result, name, version, credentialWarning }: { result: { app_id: string; app_family_id: string; execution_token?: string }; name: string; version: string; credentialWarning?: ReactNode }) {
  const [copied, setCopied] = useState(false);

  /** Confirms copying without persisting the one-time credential beyond this page. */
  function confirmCopy() { setCopied(true); }

  return <div className="mx-auto max-w-3xl space-y-6">
    <AppDetailBackLink to="/integrations/sdks?type=unified_app" />
    {credentialWarning}
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
