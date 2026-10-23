import { PageBackLink } from "~/components/layout/PageBackLink";
import { UnifiedTemplatePublisher } from "~/components/apps/UnifiedTemplatePublisher";
import { useEffect, useState, type FormEvent } from "react";
import { Link, useSearchParams } from "@remix-run/react";
import { ArrowRight, ChevronLeft, ChevronRight, Layers3, Loader2, Search } from "lucide-react";
import { listUnifiedTemplates, setUnifiedTemplateVisibility } from "~/lib/unified-app-api";
import { type UnifiedRelease } from "~/lib/unified-app-contract";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { useToast } from "~/components/Toast";

/** Separates catalogue browsing from the focused template-publication flow. */
export default function UnifiedAppTemplates() {
  const [params, setParams] = useSearchParams();
  const query = params.get("q") ?? "";
  // URL paging remains bounded even for manually edited or malformed links.
  const offset = templateOffset(params.get("offset"));
  const [search, setSearch] = useState(query);
  const [items, setItems] = useState<UnifiedRelease[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [selected, setSelected] = useState<UnifiedRelease | null>(null);
  // URL state resumes the publisher after creating an app and survives ordinary navigation.
  const publishOpen = params.get("publish") === "1";
  const [saving, setSaving] = useState(false);
  const [revision, setRevision] = useState(0);
  const { access } = useCurrentActorAccess();
  const toast = useToast();
  const canPublish = hasWorkspacePermission(access, "catalogue.manage");
  const canCreate = hasWorkspacePermission(access, "app.unified_app.create");
  const canRead = hasWorkspacePermission(access, "catalogue.read");

  // Search and page changes discard stale responses, preserving honest loading and failure states.
  useEffect(() => {
    let current = true;
    setSearch(query); setLoading(true); setError(""); setSelected(null);
    // Publishing has its own data reads and must not mount the unrelated template catalogue.
    if (publishOpen || !canRead) { setLoading(false); return; }
    listUnifiedTemplates(query, offset).then((page) => {
      // A newer search owns the page even if an earlier request finishes later.
      if (current) { setItems(page.items); setTotal(page.total); }
    }).catch((cause) => { if (current) setError(String(cause)); }).finally(() => { if (current) setLoading(false); });
    return () => { current = false; };
  }, [query, offset, revision, canRead, publishOpen]);

  /** Explicit search submission mirrors the Services and Apps catalogue behavior. */
  function submitSearch(event: FormEvent) { event.preventDefault(); setParams({ q: search.trim() }); }

  /** Opens or closes the publisher without discarding catalogue search and paging. */
  function togglePublisher(open: boolean) {
    const next = new URLSearchParams(params);
    // Closing clears only publication context; ordinary catalogue navigation remains intact.
    if (open) next.set("publish", "1");
    else { next.delete("publish"); next.delete("app"); }
    setParams(next);
  }

  /** Refreshes discovery only after Registry confirms the reviewed release was saved. */
  function published() {
    togglePublisher(false); setRevision((value) => value + 1);
    toast.success("Unified App template published.");
  }

  /** Changes only the selected release's visibility after an explicit owner action. */
  async function changeVisibility(release: UnifiedRelease) {
    const confirmed = await toast.confirm(`Make ${release.template.name} ${release.public ? "private" : "public"}?`);
    // Owners can inspect source without committing a discoverability change.
    if (!confirmed) return;
    setSaving(true); setError("");
    try { const updated = await setUnifiedTemplateVisibility(release.id, !release.public); setSelected(updated); setItems((rows) => rows.map((item) => item.id === updated.id ? updated : item)); }
    catch (cause) { setError(String(cause)); }
    finally { setSaving(false); }
  }

  // Denied readers never mount a catalogue that appears merely empty.
  if (!canRead) return <p className="text-slate-500">Catalogue read access is required to browse Unified App templates.</p>;
  // A dedicated publication view removes catalogue search, results, and pagination from the task.
  if (publishOpen && canPublish) return <UnifiedTemplatePublisher key={params.get("app") ?? "choose"} initialAppID={params.get("app") ?? ""} canCreate={canCreate} onPublished={published} backTo={`/integrations/unified-apps/templates?q=${encodeURIComponent(query)}&offset=${offset}`} />;
  return <div className="space-y-6">
    <TemplateHeader canPublish={canPublish} canCreate={canCreate} onPublish={() => togglePublisher(!publishOpen)} />
    <form role="search" onSubmit={submitSearch} className="relative"><Search className="absolute left-3 top-3 h-4 w-4 text-slate-400" /><input aria-label="Search Unified App templates" className="w-full rounded-lg border border-slate-300 bg-white py-2.5 pl-10 pr-24 text-sm focus:outline-none focus:ring-2 focus:ring-[var(--brand-violet)]" placeholder="Search templates by name, description, or category" value={search} maxLength={200} onChange={(event) => setSearch(event.target.value)} /><button type="submit" className="absolute right-3 top-2.5 text-sm font-medium text-slate-950">Search</button></form>
    {error && <p role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{error}</p>}
    {/* Loading and errors never reuse old results as the outcome of a new search. */}
    <TemplateResults loading={loading} error={error} items={items} onSelect={setSelected} />
    <nav aria-label="Template pages" className="flex items-center justify-between border-t border-slate-100 py-3 text-xs text-slate-500"><span>{total} templates</span><div className="flex gap-4"><button disabled={loading || offset === 0} onClick={() => setParams({ q: query, offset: String(Math.max(0, offset - 20)) })} className="inline-flex items-center gap-1 disabled:opacity-30"><ChevronLeft className="h-4 w-4" />Previous</button><button disabled={loading || offset + items.length >= total} onClick={() => setParams({ q: query, offset: String(offset + 20) })} className="inline-flex items-center gap-1 disabled:opacity-30">Next<ChevronRight className="h-4 w-4" /></button></div></nav>
    {/* Inspection exposes source and exact scope before the user chooses to install a release. */}
    {selected && <TemplateDetails selected={selected} canCreate={canCreate} canPublish={canPublish} saving={saving} onClose={() => setSelected(null)} onVisibility={changeVisibility} />}
  </div>;
}

/** Keeps standalone creation alongside the connected template-publishing entry point. */
// Each header action follows its own workspace permission boundary.
function TemplateHeader({ canPublish, canCreate, onPublish }: { canPublish: boolean; canCreate: boolean; onPublish: () => void }) { return <header className="flex flex-wrap items-start justify-between gap-4"><div><PageBackLink to="/integrations/sdks?type=unified_app">Back to Unified Apps</PageBackLink><h1 className="text-2xl font-bold text-slate-900">Unified App templates</h1><p className="mt-1 text-slate-500">Start from a ready-made app and make it yours.</p></div><div className="flex flex-wrap gap-3">{/* Catalogue actions retain their independent permission boundaries. */}{canPublish && <button type="button" onClick={onPublish} className="rounded-lg border border-slate-200 bg-white px-4 py-2 text-sm font-medium">Publish template</button>}{canCreate && <Link to="/integrations/unified-apps/new" className="rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white">Create a Unified App</Link>}</div></header>; }

/** Separates loading, failed searches, and empty catalogues from usable template rows. */
function TemplateResults({ loading, error, items, onSelect }: { loading: boolean; error: string; items: UnifiedRelease[]; onSelect: (release: UnifiedRelease) => void }) { return loading ? <p role="status" className="flex items-center gap-2 text-sm text-slate-500"><Loader2 className="h-4 w-4 animate-spin" />Loading templates…</p> : !error && items.length === 0 ? <section className="rounded-xl border border-slate-200 bg-white px-6 py-14 text-center"><Layers3 className="mx-auto h-8 w-8 text-slate-300" /><h2 className="mt-4 font-semibold text-slate-900">No templates found</h2><p className="mt-1 text-sm text-slate-500">Try another search or describe a Unified App from scratch.</p></section> : !error && <div className="space-y-3">{items.map((release) => <article key={release.id} className="rounded-xl border border-slate-200 bg-white p-5 transition-colors hover:border-slate-300"><div className="flex flex-wrap items-start justify-between gap-4"><div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><h2 className="break-words font-semibold text-slate-900">{release.template.name}</h2><span className="rounded bg-slate-100 px-2 py-0.5 text-xs text-slate-500">{release.template.version}</span><span className="rounded bg-[var(--brand-violet-tint)] px-2 py-0.5 text-xs text-[var(--brand-violet)]">{release.template.category}</span></div><p className="mt-2 text-sm text-slate-500">{release.template.description}</p><p className="mt-3 break-all text-xs text-slate-400">By {release.publisher} · {Object.keys(release.template.services).join(" · ")}</p></div><button type="button" onClick={() => onSelect(release)} className="inline-flex items-center gap-2 text-sm font-medium text-slate-950">View template <ArrowRight className="h-4 w-4" /></button></div></article>)}</div>; }

/** Shows source before installation and reserves visibility changes for the publisher. */
function TemplateDetails({ selected, canCreate, canPublish, saving, onClose, onVisibility }: { selected: UnifiedRelease; canCreate: boolean; canPublish: boolean; saving: boolean; onClose: () => void; onVisibility: (release: UnifiedRelease) => void }) { return <section aria-label="Template details" className="space-y-4 rounded-xl border border-slate-200 bg-white p-5"><div className="flex justify-between gap-4"><h2 className="text-lg font-semibold">{selected.template.name}</h2><button onClick={onClose} className="text-sm text-slate-500">Close details</button></div><p className="text-sm text-slate-500">{selected.template.description}</p><ul className="list-inside list-disc text-sm text-slate-600">{selected.template.requirements.map((requirement) => <li key={requirement}>{requirement}</li>)}</ul><pre className="max-h-96 overflow-auto rounded-lg bg-slate-950 p-4 text-xs text-slate-100">{selected.template.source}</pre><div className="flex flex-wrap items-center gap-4">{canCreate && <Link to={`/integrations/unified-apps/new?template=${encodeURIComponent(selected.id)}`} className="rounded-lg bg-slate-950 px-4 py-2 text-sm text-white">Use template</Link>}{canPublish && selected.is_owner && <button disabled={saving} onClick={() => onVisibility(selected)} className="text-sm font-medium text-slate-600">Make {selected.public ? "private" : "public"}</button>}</div></section>; }

/** Invalid page URLs return to the first bounded catalogue page. */
function templateOffset(value: string | null): number { return Math.max(0, Number.parseInt(value ?? "0", 10) || 0); }
