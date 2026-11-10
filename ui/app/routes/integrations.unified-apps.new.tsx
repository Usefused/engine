import { PageBackLink } from "~/components/layout/PageBackLink";
import { templatePublishPath } from "~/lib/unified-template";
import { UnifiedAppBucketScope } from "~/components/apps/UnifiedAppBucketScope";
import { runtimeBucketNames } from "~/lib/unified-app-runtime-buckets";
import { AppExecutionEndpoint } from "~/components/apps/AppExecutionEndpoint";
import type { ChooseDescribeService, DescribeProgress, AppDescription } from "~/lib/app-describe-contract";
import { AppCredentialWarning } from "~/components/apps/AppCredentialWarning";
import { useAppCredentialReview } from "~/components/apps/useAppCredentialReview";
import { UnifiedAppCodeEditor } from "~/components/apps/UnifiedAppCodeEditor";
import { UnifiedAppBuilderLayout, type UnifiedAppBuilderTab } from "~/components/apps/UnifiedAppBuilderLayout";
import { useFusedAgent } from "~/components/agent/FusedAgentContext";
import { AppServiceAuthFields, type AppAuthSelection } from "~/components/apps/AppServiceAuthFields";
import { CreateCredentialButton } from "~/components/buckets/CreateCredentialButton";
import { useCredentialSetCreation } from "~/components/buckets/useCredentialSetCreation";
import { AppOperationPicker } from "~/components/apps/AppServiceBuilder";
import { WebhookRegistrationPicker } from "~/components/apps/WebhookRegistrationPicker";
import { UnifiedAppCompileAction } from "~/components/apps/UnifiedAppCompileAction";
import { describeSelectionKey, type AppServicePin } from "~/lib/app-describe-contract";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useSearchParams } from "@remix-run/react";
import { ArrowRight, Check, ChevronDown, Loader2, Sparkles } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { ExecutionTokenField } from "~/components/apps/ExecutionTokenField";
import { ExecutionTokenOption } from "~/components/apps/ExecutionTokenOption";
import { AppDetailBackLink } from "~/components/apps/AppDetailChrome";
import { describeUnifiedApp, reviseUnifiedApp, listUnifiedTemplates, planUnifiedApp } from "~/lib/unified-app-api";
import { UnifiedSourceClarificationError, unifiedConfig, unifiedMetadata, unifiedDraftHasEvents, unifiedServiceSettings, type UnifiedDraft } from "~/lib/unified-app-contract";
import { readUnifiedAppSourceConfig, unifiedAppSourceView } from "~/lib/unified-app-source-config";
import { unifiedAppDraftFromSource } from "~/lib/unified-app-source-draft";
import { resolveUnifiedSourceService } from "~/lib/unified-app-source-services";
import { draftAppSource } from "~/lib/app-describe-api";
import { api } from "~/lib/api";
import { unifiedEditDraft, unifiedEditConfig, unifiedEditSelections, nextUnifiedVersion, type UnifiedAppSource } from "~/lib/unified-app-edit";
import { applyApp, listAppBuildSelectors } from "~/lib/app-builder";
import { preferredAppBucket, type AppBuildSelector, type AppPlanResponse } from "~/lib/app-builder-contract";

const fieldClass = "h-9 w-full rounded-md border border-slate-300 bg-white px-2.5 text-sm focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]";
const buttonClass = "inline-flex items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:cursor-not-allowed disabled:opacity-50";

/** Retains a reviewed registration across source regeneration without inventing one for a new draft. */
function draftAttachment(draft: UnifiedDraft | null): string { return draft?.webhookAttachment ?? ""; }

/** Detects whether the selected capability scope needs inbound registration coverage. */
function draftHasWebhookEvents(draft: UnifiedDraft): boolean {
  return unifiedDraftHasEvents(draft);
}

/** Gates validation on selected provider or hosted capabilities and any required ingress registration. */
function canValidateUnifiedDraft(draft: UnifiedDraft, name: string, version: string, bucket: string, savedVersion?: string): boolean {
  // Immutable successors require a new version before source can be compiled.
  if (version.trim() === savedVersion || !canCompile(name, version, bucket, draft.source)) return false;
  // Named hosted dependencies can supply the app scope without a provider selection.
  const attached = draft.configSettings?.unified_apps;
  const hasAttachedApps = attached !== null && typeof attached === "object" && !Array.isArray(attached) && Object.keys(attached).length > 0;
  if (!Object.values(draft.services).some((pin) => Boolean(pin.operations.length)) && !draftHasWebhookEvents(draft) && !hasAttachedApps) return false;
  // Event-only and mixed scopes both need an applied registration name.
  return !draftHasWebhookEvents(draft) || Boolean(draft.webhookAttachment?.trim());
}

/** Keeps code and form edits on one reviewed draft with flat capabilities and preserved incomplete metadata. */
export default function CreateUnifiedApp() {
  const { access } = useCurrentActorAccess();
  const credentialReview = useAppCredentialReview();
  const compiledInput = useRef<{ config: Record<string, unknown>; services: UnifiedDraft["services"]; ownerTeam?: string } | null>(null);
  const [params] = useSearchParams();
  const [builderTab, setBuilderTab] = useState<UnifiedAppBuilderTab>("config");
  const editID = params.get("edit");
  // Registration choices seed only an existing app draft and never mutate its deployed configuration.
  const requestedAttachment = editID ? params.get("webhook") ?? undefined : undefined;
  // Only the fixed publication return intent is supported; arbitrary redirect URLs are ignored.
  const returnToPublisher = !editID && params.get("returnTo") === "publish-template";
  // Private source takes precedence over a template supplied in the same URL.
  const templateID = editID ? null : params.get("template");
  const [editSource, setEditSource] = useState<UnifiedAppSource | null>(null);
  const [loadingSource, setLoadingSource] = useState(Boolean(editID));
  const canCreate = hasWorkspacePermission(access, "app.unified_app.create");
  const [pickerSeed, setPickerSeed] = useState<Record<string, AppServicePin>>({});
  const [selecting, setSelecting] = useState(false);
  const [describing, setDescribing] = useState(false);
  const draftRequest = useRef<AbortController | null>(null);
  const draftRoute = useRef(0);
  // Leaving this editor cancels late model results without cancelling ordinary state updates.
  useEffect(() => {
    draftRoute.current++;
    setBuilderTab("config");
    setDescribing(false); setDraftingSource(false);
    return () => { draftRoute.current++; draftRequest.current?.abort(); draftRequest.current = null; };
  }, [editID]);
  const [draftingSource, setDraftingSource] = useState(false);
  const pendingDescription = useRef<{ goal: string; proposal: AppDescription } | null>(null);
  const [draft, setDraft] = useState<UnifiedDraft | null>(null);
  const [sourceEdit, setSourceEdit] = useState<{ source: string; error: string } | null>(null);
  const [sourceSyncing, setSourceSyncing] = useState(false);
  const sourceRevision = useRef(0);
  const currentDraft = useRef(draft);
  currentDraft.current = draft;
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
  const [plan, setPlan] = useState<AppPlanResponse | null>(null);
  const [busy, setBusy] = useState(false);
  const [compiling, setCompiling] = useState(false);
  const [progress, setProgress] = useState("");
  const [error, setError] = useState("");
  const [result, setResult] = useState<{ app_id: string; app_family_id: string; execution_token?: string } | null>(null);
  // Portable source uses bucket names, while the creation selector stores authorized resource IDs.
  const sourceBucket = editSource?.config.bucket ?? buckets.find((item) => item.resource_id === bucket)?.display_name ?? "";
  // Raw code owns its editing buffer; form changes project only after the author has finished valid metadata.
  const sourceView = sourceEdit ?? (draft ? unifiedAppSourceView(draft.source, unifiedMetadata(draft, name, version, sourceBucket)) : { source: "", error: "" });

  // Source authorization comes from the same private endpoint used by the other app editors.
  useEffect(() => {
    let current = true;
    // A source read enforces family manage permission; a new app still requires workspace create permission.
    if (!editID && !canCreate) return;
    sourceRevision.current++; setSourceEdit(null); setSourceSyncing(false);
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
          const baseline = unifiedEditDraft(saved, requestedAttachment);
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
    return () => { current = false; sourceRevision.current++; };
  }, [canCreate, templateID, editID, requestedAttachment]);

  /** Invalidates old plans and pending source hydration whenever a newer edit takes ownership. */
  function invalidatePlan(preserveSource = false) {
    sourceRevision.current++; setSourceSyncing(false);
    // Form edits update metadata in the last authored file; code edits retain their exact current buffer.
    if (!preserveSource) setSourceEdit(null);
    compiledInput.current = null; setPlan(null); setError("");
  }

  /** Revises the same source shown in Code while preserving the original draft and exact capabilities on failure. */
  async function revise(goal: string, signal: AbortSignal) {
    // Saved-source authorization and form readiness apply equally to conversation-driven edits.
    if (!draft?.source || busy || describing || selecting || (editID && editSource?.app_id !== editID)) throw new Error("Open a ready TypeScript draft before revising the app.");
    // A synchronous ownership guard closes the gap before React renders the disabled form.
    if (draftRequest.current) throw new Error("A Unified App draft is already being generated.");
    const baseline = draft, route = draftRoute.current;
    const controller = new AbortController();
    const combined = AbortSignal.any([signal, controller.signal]);
    draftRequest.current = controller; setDescribing(true);
    try {
      combined.throwIfAborted();
      /** Report progress only while this request still owns the current editor. */
      const report: DescribeProgress = (message) => { combined.throwIfAborted(); setProgress(message); };
      /** Existing identities resolve automatically; new ambiguous publishers need an explicit user choice. */
      const choosePublisher: ChooseDescribeService = async (reference, candidates) => {
        throw new Error(`Choose the publisher for ${reference}: ${candidates.map(item => `@${item.provider?.handle}/${item.slug}`).join(", ")}`);
      };
      const revised = await reviseUnifiedApp(goal, { ...baseline, source: sourceView.source, name }, report, choosePublisher);
      combined.throwIfAborted();
      // Navigation or intervening edits invalidate the proposal instead of overwriting newer user work.
      if (route !== draftRoute.current || currentDraft.current !== baseline) throw new Error("The app changed while drafting. Read the current app and retry.");
      setDraft(revised); setPickerSeed(revised.services); pendingDescription.current = null;
      invalidatePlan();
      return { revised: revised.source !== baseline.source || describeSelectionKey(revised.services) !== describeSelectionKey(baseline.services), saved: false, explanation: revised.explanation, next: "Read the updated source and selected capabilities, then validate/compile. Do not save or deploy." };
    } finally {
      // Route cleanup can already have handed ownership to another draft request.
      if (draftRequest.current === controller) { draftRequest.current = null; setDescribing(false); setProgress(""); }
    }
  }

  /** Shares grounded drafting with the form and agent, rejecting cancelled results before they change the draft. */
  async function describe(goal: string, progress: DescribeProgress, chooseService: ChooseDescribeService, signal?: AbortSignal) {
    const previous = { draft, pickerSeed, name };
    const route = draftRoute.current;
    setDraftingSource(false);
    /** Reuses the existing picker and locks its incomplete source until drafting finishes. */
    function preview(proposal: AppDescription) {
      // A stopped agent response must not install late service selections.
      signal?.throwIfAborted();
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
      // Cancellation cannot replace the user's current draft with a late model result.
      signal?.throwIfAborted();
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
      if (route === draftRoute.current && (signal?.aborted || previous.draft?.source)) { setDraft(previous.draft); setPickerSeed(previous.pickerSeed); setName(previous.name); }
      throw cause;
    } finally {
      // A completed request from an abandoned route cannot unlock another editor.
      if (route === draftRoute.current) setDraftingSource(false);
    }
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

  /** Preserves every keystroke and hydrates valid metadata back into the form without accepting stale resolutions. */
  async function updateSource(source: string) {
    // Source editing is only available after a grounded draft exists.
    if (!draft) return;
    invalidatePlan(true);
    const revision = sourceRevision.current;
    setDraft({ ...draft, source }); setSourceEdit({ source, error: "" }); setSourceSyncing(true);
    try {
      const metadata = readUnifiedAppSourceConfig(source);
      const next = await unifiedAppDraftFromSource(draft, source, metadata, buckets, resolveUnifiedSourceService, editSource?.config);
      // A newer keystroke, form edit or navigation supersedes this read-only catalogue lookup.
      if (revision !== sourceRevision.current) return;
      setDraft(next.draft); setName(next.name); setVersion(next.version); setBucket(next.bucket);
      // Body-only edits do not rehydrate the picker or interrupt its focus and disclosure state.
      if (describeSelectionKey(next.draft.services) !== describeSelectionKey(draft.services)) setPickerSeed(next.draft.services);
    } catch (cause) {
      // Invalid code stays visible, while the form retains its last valid configuration for recovery.
      if (revision === sourceRevision.current) setSourceEdit({ source, error: `Config could not be synchronized: ${String(cause)}` });
    } finally {
      // An older response cannot unlock validation for a newer unresolved source edit.
      if (revision === sourceRevision.current) setSourceSyncing(false);
    }
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

  /** Compiles exactly the visible source and current form after rejecting unsynchronized metadata. */
  async function compile(allowDependencyChanges = true) {
    // Pending work and incomplete drafts cannot start a second compilation request.
    if (busy || !draft || selecting || describing || (editID && !editSource)) return { ok: false, error: "The draft is not ready to compile." };
    setCompiling(true); setBusy(true); setError(""); setProgress("Checking selected services…");
    try {
      // Malformed inline metadata must be corrected before planning can activate any provider dependencies.
      if (sourceView.error || sourceSyncing) throw new Error(sourceView.error || "Wait for source settings to finish synchronizing.");
      const selectedBucket = buckets.find((item) => item.resource_id === bucket);
      // Selectors use IDs for UI identity, while portable app configuration resolves buckets by name.
      if (!editSource && !selectedBucket) throw new Error("Choose an available bucket before compiling.");
      const config = editSource ? unifiedEditConfig(editSource, draft, version) : unifiedConfig(draft, name, version, selectedBucket!.display_name);
      // The reviewed file must contain the same bucket, runtime scope, version, and services as the plan envelope.
      config.source = sourceView.source;
      setPlan(await planUnifiedApp(config, draft.services, setProgress, editSource?.owner_team, allowDependencyChanges));
      compiledInput.current = { config, services: draft.services, ownerTeam: editSource?.owner_team };
      return { ok: true, compiled: true, deployed: false };
    }
    catch (cause) { setError(String(cause)); return { ok: false, error: String(cause) }; }
    // Success and failure both restore the action so errors can be corrected and retried.
    finally { setCompiling(false); setBusy(false); setProgress(""); }
  }

  /** Applies only the exact reviewed receipt and keeps the one-time execution token in memory. */
  async function deploy() {
    // A source edit invalidates the receipt and must force a fresh compilation.
    if (!plan || !compiledInput.current || busy || selecting || describing) return;
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

  const agent = useFusedAgent();
  // Creation is available before manual selection; existing apps retain their dedicated source-edit tools.
  useEffect(() => {
    // Saved apps use contract-aware source editing instead of replacing their immutable identity.
    if (!agent || editID || !canCreate) return;
    /** Runs the same Describe API flow while preserving explicit publisher choices and unsaved state. */
    async function agentDescribe(goal: string, signal: AbortSignal) {
      // Only one drafting request may own this form, even before React commits its busy state.
      if (draftRequest.current) throw new Error("A Unified App draft is already being generated.");
      const controller = new AbortController();
      draftRequest.current = controller;
      const combined = AbortSignal.any([signal, controller.signal]);
      setDescribing(true);
      try {
        combined.throwIfAborted();
        /** Publisher ambiguity requires a follow-up choice rather than silently selecting a service. */
        const choosePublisher: ChooseDescribeService = async (reference, candidates) => {
          // Ambiguous providers require a user choice, never the model's guess from a UUID.
          throw new Error(`Choose the publisher for ${reference}, then describe using its qualified name: ${candidates.map(item => `@${item.provider?.handle}/${item.slug}`).join(", ")}`);
        };
        await describe(goal, setProgress, choosePublisher, combined);
        return { drafted: true, saved: false, next: "Read the page, review the TypeScript and validate/compile. Do not deploy." };
      } finally {
        // A newer route owns its own request state after navigation.
        if (draftRequest.current === controller) { draftRequest.current = null; setDescribing(false); }
      }
    }
    const unregister = agent.registerDraftBuilder({ available: !busy && !describing, describe: agentDescribe });
    return () => { unregister(); };
  });
  // The active form remains the only source of truth; navigation or draft changes replace its bridge.
  useEffect(() => {
    // Incomplete selections cannot expose a compile action to the conversation.
    if (!agent || !draft) return;
    /** Keeps agent editor requests on the only user-visible source view. */
    const setEditorView = (view: "typescript" | "yaml") => {
      // Generated deployment metadata has no editable YAML view in the Unified App editor.
      if (view !== "typescript") throw new Error("Edit this Unified App in TypeScript.");
      setBuilderTab("code");
    };
    // Agent edits stay on the TypeScript source; deployment metadata comes from the reviewed setup controls.
    return agent.registerEditor({ services: draft.services, fieldID: "unified-app-source", view: "typescript", configError: sourceView.error, setView: setEditorView, compile: () => compile(false), revise, available: !busy && !selecting && !describing });
  });

  /** Opens source review before attaching the assistant so its existing editor bridge sees the visible TypeScript. */
  function openAssistant() {
    // Existing drafts use source revision; an empty builder keeps the creation conversation on configuration.
    if (draft) setBuilderTab("code");
    agent?.open(draft ? "Help me review and improve this Unified App." : "Help me build a Unified App.");
  }

  // Deep-linked editors stay locked until the private source endpoint has authorized and returned the baseline.
  if (editID && editSource?.app_id !== editID) return <div className="space-y-4"><AppDetailBackLink to={`/integrations/unified-apps/${editID}`} />{loadingSource ? <p role="status">Loading app source…</p> : <p role="alert" className="text-red-700">{error || "App source is unavailable."}</p>}</div>;
  // Render permission failures separately from the describe and credential UI.
  if (!editID && !canCreate) return <div className="space-y-4"><AppDetailBackLink to="/integrations/sdks?type=unified_app" /><p className="text-slate-500">Unified App creation access is required.</p></div>;
  // Success deliberately retains the one-time token until the user leaves this page.
  if (result) return <CreatedUnifiedApp result={result} name={name} version={version} credentialWarning={credentialReview.warning} returnToPublisher={returnToPublisher} />;
  return <div className="mx-auto min-w-0 w-full max-w-5xl space-y-4">
    {/* Returning from creation keeps the template-publishing intent visible. */}
    <PageBackLink to={editID ? `/integrations/unified-apps/${editID}` : returnToPublisher ? templatePublishPath() : "/integrations/sdks?type=unified_app"}>{returnToPublisher ? "Back to template publishing" : "Back to apps"}</PageBackLink>
    {returnToPublisher && <p className="rounded-lg bg-violet-50 p-4 text-sm text-violet-900">Create and deploy your Unified App, then continue to review and publish its template.</p>}
    <header className="flex flex-wrap items-start justify-between gap-3"><div><h1 className="text-xl font-semibold text-slate-900">{editID ? "Edit Unified App" : "Create a Unified App"}</h1><p className="mt-1 text-xs text-slate-500">{editSource ? `Version ${editSource.config.version} · Changes deploy as a new version.` : "Configure your app, edit its code, then validate and deploy."}</p></div>{/* Templates start a separate app rather than replacing a saved editor baseline. */}{!editID && <Link to="/integrations/unified-apps/templates" className="text-xs font-medium text-[var(--brand-violet)] hover:underline">Browse templates</Link>}</header>
    {/* Describe shares the same draft state, so its progress remains visible outside the conversation. */}
    {describing && <p role="status" className="flex items-center gap-2 text-sm text-slate-600"><Loader2 className="h-4 w-4 animate-spin" />{progress || "Drafting your Unified App…"}</p>}
    <UnifiedAppBuilderLayout tab={builderTab} onTabChange={setBuilderTab}
      tools={agent && <button type="button" disabled={busy || describing || selecting} onClick={openAssistant} className="inline-flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-100 disabled:opacity-50"><Sparkles className="h-3.5 w-3.5" aria-hidden="true" />Ask Fused</button>}
      config={<fieldset disabled={Boolean(sourceView.error) || sourceSyncing} className="min-w-0 space-y-4">
        {/* Metadata stays concise; native required controls retain validation without oversized badges. */}
        {draft && <fieldset aria-label="App settings" disabled={busy || describing || selecting} className="min-w-0 space-y-3">
          <div className="grid min-w-0 gap-3 sm:grid-cols-[minmax(0,2fr)_7rem_minmax(0,1fr)]">
            <label className="min-w-0 space-y-1 text-xs font-medium text-slate-600"><span className="block">App name</span>
              <input required className={fieldClass} readOnly={Boolean(editID)} value={name} onChange={/** Metadata edits invalidate the previously compiled configuration. */ (event) => { setName(event.target.value); invalidatePlan(); }} />
            </label>
            <label className="min-w-0 space-y-1 text-xs font-medium text-slate-600">
              {/* Existing families require an immutable successor label. */}
              <span className="block">{editID ? "New version" : "Version"}</span>
              <input required className={fieldClass} value={version} onChange={/** Version changes require a fresh compilation receipt. */ (event) => { setVersion(event.target.value); invalidatePlan(); }} />
            </label>
            {/* Runtime routing needs room for permitted-bucket tags and the default selector as the form expands. */}
            <div className={`min-w-0 space-y-1 text-xs font-medium text-slate-600 ${runtimeBucketNames(draft.configSettings?.runtime_buckets).length ? "sm:col-span-full" : ""}`}>
              <div className="flex items-center justify-between gap-2"><label htmlFor="unified-credential-bucket">Bucket</label>
                {/* Only new families may choose a newly created default credential binding. */}
                {!editID && credentialCreation.create && <CreateCredentialButton onClick={credentialCreation.create} />}
              </div>
              <UnifiedAppBucketScope value={runtimeBucketNames(draft.configSettings?.runtime_buckets)} buckets={buckets} bucket={bucket} lockedBucketName={editSource?.config.bucket}
                onBucketChange={/** Credential routing changes invalidate the same plan as source edits. */ (selected) => { setBucket(selected); invalidatePlan(); }}
                onChange={/** Runtime scope remains in the reviewed configuration rather than local presentation state. */ (names) => { setDraft({ ...draft, configSettings: { ...draft.configSettings, runtime_buckets: names } }); invalidatePlan(); }} />
            </div>
          </div>
          <label className="block space-y-1 text-xs font-medium text-slate-600"><span className="block">Description</span>
            <textarea rows={1} className={`${fieldClass} min-h-9 resize-y py-2`} maxLength={1024} value={draft.description} onChange={/** Description edits share the existing draft and plan invalidation path. */ (event) => updateDescription(event.target.value)} placeholder="What does this app do?" />
          </label>
        </fieldset>}
        {/* New apps start in discovery; saved scopes and webhook deep links remain accessible in the Config tab. */}
        <section aria-label="Services & events" className="space-y-3 border-t border-slate-200 pt-4">
          <h3 className="text-sm font-medium text-slate-800">Services & events<span className="ml-2 text-xs font-normal text-slate-400">{Object.keys(draft?.services ?? pickerSeed).length} selected</span></h3>
          <fieldset id="app-service-selection" disabled={busy || describing} className="min-w-0 scroll-mt-6"><AppOperationPicker seed={pickerSeed} onChange={selectOperations} onPendingChange={setSelecting} allowWebhooks /></fieldset>
        </section>
        {/* Registration selection can precede event selection, while compilation still requires an explicit event scope. */}
        {draft && (draftHasWebhookEvents(draft) || draftAttachment(draft)) && <details className="group rounded-lg border border-slate-200" open={!draftAttachment(draft)}>
          <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-3 py-2.5 text-sm font-medium text-slate-800"><span>Webhook<span className="ml-2 break-all text-xs font-normal text-slate-500">{draftAttachment(draft)}</span></span><ChevronDown className="h-4 w-4 shrink-0 text-slate-400 transition-transform group-open:rotate-180" /></summary>
          <fieldset disabled={busy || describing || selecting} className="max-h-80 overflow-y-auto border-t border-slate-100 p-3"><WebhookRegistrationPicker selected={draftAttachment(draft)} onSelect={updateWebhookAttachment} /></fieldset>
        </details>}
        {/* Credential overrides and initial token issuance remain available without expanding the everyday form. */}
        {draft && <details className="group rounded-lg border border-slate-200">
          <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-3 py-2.5 text-sm font-medium text-slate-800">Authentication & access<ChevronDown className="h-4 w-4 shrink-0 text-slate-400 transition-transform group-open:rotate-180" /></summary>
          <fieldset disabled={busy || describing || selecting} className="space-y-3 border-t border-slate-100 p-3">
            <AppServiceAuthFields services={Object.entries(draft.services).filter(/** Event-only capabilities need no outbound credential override. */ ([, pin]) => pin.operations.length).map(/** Each override stays bound to its selected provider identity and version. */ ([key, pin]) => ({ key, service_id: pin.service_id, version: pin.version, auth: unifiedServiceSettings(draft, key).auth as AppAuthSelection | undefined }))} onChange={updateAuth} disabled={busy || describing || selecting} />
            {/* Existing app edits preserve their tokens; initial issuance remains a creation choice. */}
            {!editID && <ExecutionTokenOption checked={generateExecutionToken} onChange={setGenerateExecutionToken} />}
          </fieldset>
        </details>}
      </fieldset>}
      code={<fieldset disabled={busy || describing || selecting} className="min-w-0">
        {/* Source is mounted once per draft so tab changes preserve its change-review baseline and editor state. */}
        {draft ? <><p className="mb-2 text-xs text-slate-500">Edit <code>config</code> here or in the Config tab. Changes update both views.</p><UnifiedAppCodeEditor source={sourceView.source} disabled={busy || describing || selecting} onSource={updateSource} /></> : <p className="px-3 py-12 text-center text-sm text-slate-500">Choose services in Config or ask Fused to draft your app.</p>}
        {/* Source generation remains explicit while the shared draft is temporarily incomplete. */}
        {draftingSource && !draft?.source && <p role="status" className="mt-2 text-xs text-slate-500">Generating TypeScript…</p>}
      </fieldset>}
      footer={<fieldset disabled={busy || describing || selecting} className="space-y-3">
        {/* Read failures and compilation errors remain visible regardless of the selected authoring tab. */}
        {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
        {/* Incomplete inline JSON remains editable, but cannot silently diverge from the configuration being deployed. */}
        {sourceView.error && <p role="alert" className="text-sm text-red-700">{sourceView.error}</p>}
        {/* Compilation owns its own live status; other loading states stay in the shared footer. */}
        {busy && !compiling && <p role="status" className="flex items-center gap-2 text-xs text-slate-500"><Loader2 className="h-3.5 w-3.5 animate-spin" />{progress || "Loading app…"}</p>}
        {/* Only a grounded draft can be compiled, and every content edit invalidates this deployment receipt. */}
        {draft && (plan ? <section className="space-y-3">
          <div className="flex flex-wrap items-center justify-between gap-3"><p className="inline-flex items-center gap-1.5 text-sm font-medium text-emerald-700"><Check className="h-4 w-4" />Ready to deploy</p><button type="button" className={buttonClass} onClick={deploy}>{editID ? "Deploy new version" : "Deploy Unified App"}<ArrowRight className="h-4 w-4" /></button></div>
          <AppCredentialWarning readiness={plan.credential_readiness} canManageBucket={credentialReview.canManageBucket} />
          <details><summary className="cursor-pointer text-xs text-slate-500">Review deployment details</summary><pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md bg-slate-50 p-3 text-xs text-slate-600">{JSON.stringify(plan.summary, null, 2)}</pre></details>
        </section> : <div className="flex flex-wrap items-start justify-between gap-3"><p className="max-w-sm pt-2 text-xs text-slate-500">Validate without running provider calls. Review before deploying.</p><UnifiedAppCompileAction className={buttonClass} compiling={compiling} progress={progress} disabled={sourceSyncing || Boolean(sourceView.error) || !canValidateUnifiedDraft(draft, name, version, bucket, editSource?.config.version)} onCompile={/** Both tabs compile the same current draft through the existing reviewed workflow. */ () => compile()} /></div>)}
        {/* Empty drafts have no deployment action but still explain how to begin. */}
        {!draft && <p className="text-xs text-slate-500">Choose services or ask Fused to get started.</p>}
      </fieldset>}
    />
    {credentialCreation.dialog}
    {credentialReview.dialog}
  </div>;
}

/** Retains the one-time token before the user resumes publication with the exact deployed version. */
function CreatedUnifiedApp({ result, name, version, credentialWarning, returnToPublisher = false }: { result: { app_id: string; app_family_id: string; execution_token?: string }; name: string; version: string; credentialWarning?: ReactNode; returnToPublisher?: boolean }) {
  const [copied, setCopied] = useState(false);

  /** Confirms copying without persisting the one-time credential beyond this page. */
  function confirmCopy() { setCopied(true); }

  return <div className="mx-auto max-w-3xl space-y-6">
    {/* The completed creation keeps the original publishing destination available. */}
    <PageBackLink to={returnToPublisher ? templatePublishPath(result.app_id) : "/integrations/sdks?type=unified_app"}>{returnToPublisher ? "Back to template publishing" : "Back to apps"}</PageBackLink>
    {credentialWarning}
    <section className="overflow-hidden rounded-xl border border-slate-200 bg-white">
      <header className="space-y-4 p-5 sm:p-6">
        <div className="flex items-center gap-2 text-sm font-medium text-emerald-700"><Check className="h-4 w-4" aria-hidden="true" />Your Unified App is ready</div>
        <div className="flex flex-wrap items-center gap-3"><h1 className="text-2xl font-bold text-slate-900">{name}</h1><span className="rounded-md bg-slate-100 px-2 py-1 text-xs font-medium text-slate-600">v{version}</span></div>
        <p className="text-sm text-slate-500">Your app is deployed. This URL follows the version receiving traffic.</p>
        <AppExecutionEndpoint appID={result.app_family_id} appName={name} />
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
        {/* Publication is a separate reviewed action, so creation never automatically shares source. */}
        <p className="text-sm text-slate-500">{returnToPublisher ? "Continue to prepare and review your template." : "View activity, requests, and versions."}</p>
        <Link className={buttonClass} to={returnToPublisher ? templatePublishPath(result.app_id) : `/integrations/unified-apps/${result.app_id}`}>{returnToPublisher ? "Continue to publish template" : "Open app"} <ArrowRight className="h-4 w-4" aria-hidden="true" /></Link>
      </footer>
    </section>
  </div>;
}

/** All reviewed inputs must be present before dependency activation and compilation. */
function canCompile(name: string, version: string, bucket: string, source: string): boolean { return [name, version, bucket, source].every((value) => Boolean(value.trim())); }
