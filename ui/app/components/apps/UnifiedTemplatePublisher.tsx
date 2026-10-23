import { Select } from "~/components/forms/Select";
import { PageBackLink } from "~/components/layout/PageBackLink";
import { useEffect, useState, type FormEvent } from "react";
import { Link } from "@remix-run/react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasResourcePermission } from "~/lib/current-actor-access";
import type { UnifiedRelease, UnifiedTemplate } from "~/lib/unified-app-contract";
import { publishUnifiedTemplate } from "~/lib/unified-app-api";
import { checkTemplate, listTemplateApps, listTemplateAppVersions, readTemplateApp, type TemplateApp, type TemplateAppVersion } from "~/lib/unified-template-api";
import { parseTemplate, templateFromApp, type TemplateChecks } from "~/lib/unified-template";

const fieldClass = "w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm";
const actionClass = "rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white disabled:opacity-50";

/** Uses the shared page shell while showing app selection, details, and review one stage at a time. */
export function UnifiedTemplatePublisher({ initialAppID, canCreate, onPublished, backTo = "/integrations/unified-apps/templates" }: { initialAppID: string; canCreate: boolean; onPublished: (release: UnifiedRelease) => void; backTo?: string }) {
  const [appID, setAppID] = useState(initialAppID);
  const [familyVersionID, setFamilyVersionID] = useState(initialAppID);
  const [versions, setVersions] = useState<TemplateAppVersion[]>([]);
  const [draft, setDraft] = useState<UnifiedTemplate | null>(null);
  const [checks, setChecks] = useState<TemplateChecks | null>(null);
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [historyError, setHistoryError] = useState("");
  const [checkError, setCheckError] = useState("");
  const [isPublic, setPublic] = useState(false);
  const [reviewing, setReviewing] = useState(false);
  const [retry, setRetry] = useState(0);
  const [checkRetry, setCheckRetry] = useState(0);

  // History belongs to the selected family, so changing a version does not reset the version picker.
  useEffect(() => {
    let current = true;
    setVersions([]); setHistoryError("");
    // The initial chooser has no deployed version history.
    if (!familyVersionID) return;
    listTemplateAppVersions(familyVersionID).then((items) => {
      // Late history cannot overwrite a newer family selection.
      if (current) setVersions(items);
    }).catch((cause) => {
      // A failed history read must remain visible instead of looking like a single-version app.
      if (current) setHistoryError(String(cause));
    });
    return () => { current = false; };
  }, [familyVersionID, retry]);

  // A source response may update the form only while its exact selected version is current.
  useEffect(() => {
    let current = true;
    // Returning to app selection cancels the previous source request.
    if (!appID) return;
    setDraft(null); setChecks(null); setError(""); setReviewing(false); setLoading(true);
    readTemplateApp(appID).then((saved) => {
      // The private source endpoint owns permission checks; extraction never broadens access.
      if (current) setDraft(templateFromApp(saved));
    }).catch((cause) => {
      // Errors from a previously selected app must not replace the current draft.
      if (current) setError(String(cause));
    }).finally(() => {
      // Only the latest selection can unlock its source form.
      if (current) setLoading(false);
    });
    return () => { current = false; };
  }, [appID, retry]);

  const services = draft?.services;
  // Metadata edits do not change dependency admission; selecting another source does.
  useEffect(() => {
    let current = true;
    setChecks(null); setCheckError("");
    // The chooser has no dependency scope to inspect yet.
    if (!services) { setChecking(false); return; }
    setChecking(true);
    checkTemplate({ services }).then((result) => {
      // A response for an earlier selection cannot approve a replacement's dependencies.
      if (current) setChecks(result);
    }).catch((cause) => {
      // An unavailable Registry must never appear as a clean publication check.
      if (current) setCheckError(`Could not check dependencies: ${String(cause)}`);
    }).finally(() => {
      // Stale checks cannot enable publication for the new selection.
      if (current) setChecking(false);
    });
    return () => { current = false; };
  }, [services, checkRetry]);

  /** Starts source selection with private visibility and no inherited review approval. */
  function chooseApp(id: string) {
    // Re-selecting the current version must not clear its draft without triggering a source reload.
    if (id === appID) return;
    setFamilyVersionID(id); setAppID(id); setDraft(null); setChecks(null); setError(""); setReviewing(false); setPublic(false);
  }

  /** Returns to app selection without retaining source or approvals from the previous app. */
  function changeApp() {
    setAppID(""); setFamilyVersionID(""); setDraft(null); setChecks(null); setError(""); setReviewing(false); setPublic(false); setLoading(false);
  }

  /** Updates presentation metadata without altering source or exact dependency pins. */
  function editMetadata(field: "name" | "slug" | "version" | "description" | "category" | "requirements", value: string) {
    // The source chooser cannot accept metadata before a draft exists.
    if (!draft) return;
    // Requirements use one line per item while other metadata remains plain text.
    setDraft({ ...draft, [field]: field === "requirements" ? value.split("\n") : value });
    setReviewing(false);
  }

  /** Rechecks mutable contracts before showing review and again immediately before publishing. */
  async function submit(event: FormEvent) {
    event.preventDefault();
    // Incomplete or in-flight selections must never publish a stale snapshot.
    if (!draft || saving || loading || checking) return;
    setSaving(true); setError("");
    try {
      const template = parseTemplate(JSON.stringify(draft));
      const fresh = await checkTemplate(template);
      setChecks(fresh);
      // Private dependency warnings block only public releases; structural failures block every release.
      const blockers = [...fresh.blockers, ...(isPublic ? fresh.publicBlockers : [])];
      if (blockers.length) { setReviewing(false); throw new Error(blockers.join(" ")); }
      // The first submission is a source review, never a publication side effect.
      if (!reviewing) { setReviewing(true); return; }
      onPublished(await publishUnifiedTemplate(template, isPublic));
    } catch (cause) { setError(String(cause)); }
    finally { setSaving(false); }
  }

  // Public eligibility is separate from the validity of a private template.
  const blocked = publicationBlocked(checks, isPublic);
  return <section aria-label="Publish a Unified App template" className="space-y-6">
    {/* The shared back link joins the utility row, keeping titles aligned across both template pages. */}
    <PageBackLink to={backTo}>Back to templates</PageBackLink>
    <header><h1 className="text-2xl font-bold text-slate-900">Publish a template</h1><p className="mt-1 text-slate-500">Share a reusable version of your Unified App.</p></header>
    <fieldset disabled={saving} className="min-w-0 space-y-6">
      {/* App selection takes the full page until a source is chosen, rather than staying above the editor. */}
      {!appID && <TemplateAppChooser canCreate={canCreate} onChoose={chooseApp} />}
      {/* The details step retains one way to change the app, without repeating the app catalogue. */}
      {appID && !reviewing && <div className="flex flex-wrap items-end justify-between gap-4"><TemplateVersionPicker appID={appID} versions={versions} reviewing={reviewing} onChange={(id) => { setAppID(id); setDraft(null); setReviewing(false); }} /><button type="button" onClick={changeApp} className="text-sm text-[var(--brand-violet)]">Change app</button></div>}
      <PublisherAlerts messages={[error, historyError, checkError]} onRetry={() => {
        // Dependency failures retain edited metadata; only missing source needs reloading.
        if (draft) setCheckRetry((value) => value + 1);
        else setRetry((value) => value + 1);
      }} />
      {loading && <p role="status" className="text-sm text-slate-500">Loading saved app source…</p>}
      {/* A failed source read never leaves the previous app's publish controls mounted. */}
      {draft && !loading && <TemplateEditor draft={draft} checks={checks} checking={checking} saving={saving} blocked={blocked} reviewing={reviewing} isPublic={isPublic} onPublic={setPublic} onEdit={editMetadata} onBack={() => setReviewing(false)} onSubmit={submit} />}
    </fieldset>
  </section>;
}

/** Shows only useful selection controls, with a single creation action when no current apps exist. */
function TemplateAppChooser({ canCreate, onChoose }: { canCreate: boolean; onChoose: (id: string) => void }) {
  const { access } = useCurrentActorAccess();
  const [search, setSearch] = useState("");
  const [offset, setOffset] = useState(0);
  const [hasApps, setHasApps] = useState<boolean | null>(null);
  const [page, setPage] = useState<{ items: TemplateApp[]; total: number }>({ items: [], total: 0 });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  // Search and paging each own their response, preventing stale catalogue rows.
  useEffect(() => {
    let current = true;
    setLoading(true); setError("");
    listTemplateApps(search, offset, false).then((result) => {
      // Earlier searches cannot replace the currently requested page.
      if (current) {
        setPage(result);
        // An unfiltered first page distinguishes an empty workspace from a search with no matches.
        if (!search && offset === 0) setHasApps(result.total > 0);
      }
    }).catch((cause) => {
      // Read failures must not be presented as having no apps.
      if (current) setError(String(cause));
    }).finally(() => {
      // Only the active request owns the loading state.
      if (current) setLoading(false);
    });
    return () => { current = false; };
  }, [search, offset, retry]);
  // An initial load or failure must not flash empty-workspace guidance.
  if (hasApps === null) return <ChooserStatus loading={loading} error={error} onRetry={() => setRetry((value) => value + 1)} />;
  // With no apps there is nothing to search, filter, or page through.
  if (!hasApps) return <EmptyTemplateApps canCreate={canCreate} />;
  return <div className="space-y-5">
    <div className="flex flex-wrap items-center justify-between gap-3"><h2 className="text-lg font-semibold">Choose an app</h2>
      {/* Creation is an alternative to selecting an existing app and returns to this same flow. */}
      {canCreate && <Link className="text-sm font-medium text-[var(--brand-violet)]" to="/integrations/unified-apps/new?returnTo=publish-template">Create new app</Link>}
    </div>
    <input aria-label="Search your Unified Apps" placeholder="Search apps" className={fieldClass} value={search} onChange={(event) => { setSearch(event.target.value); setOffset(0); }} maxLength={200} />
    {/* Transport failures never reuse old results as if they matched the current search. */}
    {loading || error ? <ChooserStatus loading={loading} error={error} onRetry={() => setRetry((value) => value + 1)} /> : <TemplateAppRows items={page.items} access={access} onChoose={onChoose} />}
    {/* A single page needs no pagination controls or redundant app counter. */}
    {(page.total > 20 || offset > 0) && <nav aria-label="Your Unified App pages" className="flex justify-between border-t border-slate-100 pt-4 text-sm"><button type="button" className="disabled:opacity-40" disabled={loading || offset === 0} onClick={() => setOffset(Math.max(0, offset - 20))}>Previous apps</button><button type="button" className="disabled:opacity-40" disabled={loading || offset + 20 >= page.total} onClick={() => setOffset(offset + 20)}>Next apps</button></nav>}
  </div>;
}

/** Makes the first useful action the only action in an empty app catalogue. */
function EmptyTemplateApps({ canCreate }: { canCreate: boolean }) {
  return <div className="rounded-xl border border-slate-200 bg-white px-6 py-14 text-center">
    <h2 className="text-lg font-semibold text-slate-900">Create your first Unified App</h2>
    <p className="mx-auto mt-2 max-w-sm text-sm leading-6 text-slate-500">A template starts with an app. Once yours is ready, you can publish it here.</p>
    {/* Creation remains permission-aware without exposing unrelated configuration controls. */}
    {canCreate ? <Link className={`${actionClass} mt-6 inline-flex`} to="/integrations/unified-apps/new?returnTo=publish-template">Create a Unified App</Link> : <p className="mt-6 text-sm text-slate-500">Ask your workspace administrator for app creation access.</p>}
  </div>;
}

/** Keeps loading and failed discovery distinct from a successful empty result. */
function ChooserStatus({ loading, error, onRetry }: { loading: boolean; error: string; onRetry: () => void }) {
  // A new request owns the status until it either resolves or fails.
  if (loading) return <p role="status" className="py-8 text-sm text-slate-500">Loading your apps…</p>;
  return <div role="alert" className="space-y-3 text-sm text-red-700"><p>{error}</p><button type="button" onClick={onRetry}>Retry apps</button></div>;
}

/** Displays authorized source choices without mounting the next step's editor. */
function TemplateAppRows({ items, access, onChoose }: { items: TemplateApp[]; access: ReturnType<typeof useCurrentActorAccess>["access"]; onChoose: (id: string) => void }) {
  // Search misses keep the search input available instead of pretending the workspace has no apps.
  if (!items.length) return <p className="py-6 text-sm text-slate-500">No apps match your search.</p>;
  return <div className="space-y-2">{items.map((app) => {
    // Listing permission alone never grants access to private app source.
    const canUse = Boolean(app.app_id) && hasResourcePermission(access, "app.unified_app.manage", "APP", app.app_family_id);
    return <button type="button" key={app.app_family_id} disabled={!canUse} onClick={() => onChoose(app.app_id!)} className="flex w-full items-center justify-between gap-3 rounded-lg border border-slate-200 bg-white p-4 text-left text-sm hover:border-violet-400 disabled:opacity-50"><span className="break-words font-medium">{app.name}</span><span className="shrink-0 text-xs text-slate-500">{canUse ? app.version : "App management access required"}</span></button>;
  })}</div>;
}

/** Keeps editing and final source review visually separate while retaining one explicit publish action. */
function TemplateEditor({ draft, checks, checking, saving, blocked, reviewing, isPublic, onPublic, onEdit, onBack, onSubmit }: {
  draft: UnifiedTemplate; checks: TemplateChecks | null; checking: boolean; saving: boolean; blocked: boolean; reviewing: boolean; isPublic: boolean;
  onPublic: (value: boolean) => void; onEdit: TemplateMetadataProps["onEdit"];
  onBack: () => void; onSubmit: (event: FormEvent) => void;
}) {
  // Private-dependency warnings matter only when the author chooses a public release.
  const blockers = visibleTemplateBlockers(checks, isPublic);
  return <form onSubmit={onSubmit} className="space-y-6 rounded-xl border border-slate-200 bg-white p-6">
    {/* Only the active step is mounted; review does not repeat a disabled editing form. */}
    {reviewing ? <TemplateReview draft={draft} isPublic={isPublic} /> : <>
      <h2 className="text-lg font-semibold">Template details</h2>
      <TemplateMetadata draft={draft} onEdit={onEdit} />
      <label className="flex items-start gap-3 text-sm"><input type="checkbox" checked={isPublic} onChange={(event) => onPublic(event.target.checked)} /><span>Make publicly discoverable<span className="mt-1 block text-xs text-slate-500">Let other catalogue users find and use this template.</span></span></label>
    </>}
    {/* Admission failures remain visible in either step and are never concealed by the simplified layout. */}
    {checking && <p role="status" className="text-sm text-slate-500">Checking dependencies…</p>}
    {blockers.length > 0 && <ul className="space-y-1 text-sm text-amber-800">{blockers.map((message) => <li key={message}>{message}</li>)}</ul>}
    <div className="flex flex-wrap items-center justify-between gap-3 border-t border-slate-100 pt-5">
      {/* Editing discards review approval while preserving the prepared template. */}
      {reviewing && <button type="button" onClick={onBack} className="text-sm text-slate-600">Back to details</button>}
      <button type="submit" disabled={saving || checking || blocked} className={`${actionClass} ml-auto`}>{saving ? "Checking…" : reviewing ? "Publish template" : "Continue to review"}</button>
    </div>
  </form>;
}

type TemplateMetadataProps = { draft: UnifiedTemplate; onEdit: (field: "name" | "slug" | "version" | "description" | "category" | "requirements", value: string) => void };

/** Keeps common presentation fields visible and puts optional release metadata behind one disclosure. */
function TemplateMetadata({ draft, onEdit }: TemplateMetadataProps) {
  return <div className="space-y-5">
    <label className="block space-y-2 text-sm font-medium">Name<input aria-label="Template name" required maxLength={200} className={fieldClass} value={draft.name} onChange={(event) => onEdit("name", event.target.value)} /></label>
    <label className="block space-y-2 text-sm font-medium">Description<textarea aria-label="Description" maxLength={16000} rows={3} className={fieldClass} value={draft.description} onChange={(event) => onEdit("description", event.target.value)} /></label>
    <label className="block space-y-2 text-sm font-medium">Release version<input aria-label="Release version" required maxLength={128} className={fieldClass} value={draft.version} onChange={(event) => onEdit("version", event.target.value)} /></label>
    <details><summary className="cursor-pointer text-sm text-slate-600">More details</summary><div className="mt-4 space-y-4">
      <label className="block space-y-2 text-sm font-medium">Slug<input aria-label="Template slug" maxLength={100} className={fieldClass} value={draft.slug} onChange={(event) => onEdit("slug", event.target.value)} /></label>
      <label className="block space-y-2 text-sm font-medium">Category<input aria-label="Category" maxLength={100} className={fieldClass} value={draft.category} onChange={(event) => onEdit("category", event.target.value)} /></label>
      <label className="block space-y-2 text-sm font-medium">Setup requirements (one per line)<textarea aria-label="Setup requirements" className={fieldClass} value={draft.requirements.join("\n")} onChange={(event) => onEdit("requirements", event.target.value)} /></label>
    </div></details>
  </div>;
}

/** Summarizes exactly what will be shared without repeating metadata inputs or app selection. */
function TemplateReview({ draft, isPublic }: { draft: UnifiedTemplate; isPublic: boolean }) {
  return <div className="space-y-5">
    <div><h2 className="text-lg font-semibold">Review and publish</h2><p className="mt-1 text-sm text-slate-500">Check the included source for secrets or private data before sharing.</p></div>
    <div className="space-y-1"><h3 className="font-medium">{draft.name}</h3><p className="text-sm text-slate-500">{draft.version} · {isPublic ? "Public template" : "Private template"}</p><p className="text-sm text-slate-600">{draft.description}</p></div>
    <details><summary className="cursor-pointer text-sm font-medium">Included services</summary><ul className="mt-3 space-y-2 break-words text-sm text-slate-600">{Object.entries(draft.services).map(([key, pin]) => <li key={key}>{key} · {pin.version} · {pin.operations.join(", ")}</li>)}</ul></details>
    <details open><summary className="cursor-pointer text-sm font-medium">Included source</summary><pre className="mt-3 max-h-80 overflow-auto rounded-lg bg-slate-950 p-4 text-xs text-slate-100">{draft.source}</pre></details>
    <p className="text-xs text-slate-500">Credential configuration and bucket bindings are excluded. Each installer connects their own accounts.</p>
  </div>;
}

/** Keeps catalogue and private-source errors visible without discarding the user's draft. */
function PublisherAlerts({ messages, onRetry }: { messages: string[]; onRetry: () => void }) {
  const failures = messages.filter(Boolean);
  // A successful read needs no alert or retry control.
  if (!failures.length) return null;
  return <div role="alert" className="space-y-2 rounded-lg bg-red-50 p-3 text-sm text-red-700">{failures.map((message) => <p key={message}>{message}</p>)}<button type="button" onClick={onRetry}>Retry loading and checks</button></div>;
}

/** Blocks publication until checks exist and the requested visibility is admissible. */
function publicationBlocked(checks: TemplateChecks | null, isPublic: boolean): boolean {
  return !checks || checks.blockers.length > 0 || (isPublic && checks.publicBlockers.length > 0);
}

/** Uses the shared control to keep historical versions selectable even when publication is unavailable. */
function TemplateVersionPicker({ appID, versions, reviewing, onChange }: { appID: string; versions: TemplateAppVersion[]; reviewing: boolean; onChange: (id: string) => void }) {
  // The chooser has no saved app history.
  if (!appID) return null;
  return <label className="block space-y-2 text-sm font-medium">App version<Select aria-label="App version" className="w-full" value={appID} onChange={(event) => onChange(event.target.value)} disabled={reviewing || !versions.length}>
    {/* Keep the selected ID represented while history is loading or unavailable. */}
    {!versions.some((item) => item.app_id === appID) && <option value={appID}>Selected app version</option>}
    {versions.map((item) => <option key={item.app_id} value={item.app_id}>{item.version}</option>)}
  </Select></label>;
}

/** Shows only admission failures relevant to the selected visibility. */
function visibleTemplateBlockers(checks: TemplateChecks | null, isPublic: boolean): string[] {
  // Pending checks are represented by their loading state, not speculative warnings.
  if (!checks) return [];
  return [...checks.blockers, ...(isPublic ? checks.publicBlockers : [])];
}
