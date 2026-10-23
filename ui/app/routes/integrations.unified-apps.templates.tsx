import { CataloguePageHeader, catalogueActionClassName } from "~/components/layout/CataloguePageHeader";
import { Select } from "~/components/forms/Select";
import { AnchoredPopover } from "~/components/forms/AnchoredPopover";
import { PageBackLink } from "~/components/layout/PageBackLink";
import { UnifiedTemplatePublisher } from "~/components/apps/UnifiedTemplatePublisher";
import { useEffect, useState, useRef, useId, type FormEvent } from "react";
import { Link, useSearchParams } from "@remix-run/react";
import { ChevronLeft, ChevronRight, ChevronDown, Search, Layers3, Plus, Upload } from "lucide-react";
import { listUnifiedTemplates, setUnifiedTemplateVisibility } from "~/lib/unified-app-api";
import { type UnifiedRelease } from "~/lib/unified-app-contract";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { TemplateResults, TemplateDetailsSidebar } from "~/components/apps/UnifiedTemplateCatalogue";
import { useToast } from "~/components/Toast";

/** Keeps a paginated template library separate from source inspection in a modal sidebar. */
export default function UnifiedAppTemplates() {
  const [params, setParams] = useSearchParams();
  const query = params.get("q") ?? "";
  const privateOnly = params.get("private") === "1";
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
  const [detailError, setDetailError] = useState("");
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
    setSearch(query); setLoading(true); setError("");
    // Publishing has its own data reads and must not mount the unrelated template catalogue.
    if (publishOpen || !canRead) { setLoading(false); return; }
    listUnifiedTemplates(query, offset, [], privateOnly).then((page) => {
      // A newer search owns the page even if an earlier request finishes later.
      if (current) { setItems(page.items); setTotal(page.total); }
    }).catch((cause) => { if (current) setError(String(cause)); }).finally(() => { if (current) setLoading(false); });
    return () => { current = false; };
  }, [query, offset, privateOnly, revision, canRead, publishOpen]);

  // Navigation closes inspection; a visibility refresh may keep the current release open.
  useEffect(() => { setSelected(null); setDetailError(""); }, [query, offset, privateOnly, publishOpen]);

  /** Updates the catalogue URL while preserving private-only discovery and resetting paging for new searches. */
  function browse(next: { q?: string; offset?: number; privateOnly?: boolean }) {
    setParams({ q: next.q ?? query, private: String(Number(next.privateOnly ?? privateOnly)), offset: String(next.offset ?? 0) });
  }

  /** Explicit search submission mirrors the Services and Apps catalogue behavior. */
  function submitSearch(event: FormEvent) { event.preventDefault(); browse({ q: search.trim() }); }

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

  /** Applies a visibility change confirmed inside the sidebar and refreshes filtered totals. */
  async function changeVisibility(release: UnifiedRelease) {
    setSaving(true); setDetailError("");
    try {
      const updated = await setUnifiedTemplateVisibility(release.id, !release.public);
      // A completed mutation must not reopen a sidebar the user has already dismissed.
      setSelected((current) => current?.id === updated.id ? updated : current);
      setRevision((value) => value + 1);
      toast.success("Template visibility updated.");
    } catch (cause) { setDetailError(String(cause)); }
    finally { setSaving(false); }
  }

  // Denied readers never mount a catalogue that appears merely empty.
  if (!canRead) return <p className="text-slate-500">Catalogue read access is required to browse Unified App templates.</p>;
  // A dedicated publication view removes catalogue search, results, and pagination from the task.
  if (publishOpen && canPublish) return <UnifiedTemplatePublisher key={params.get("app") ?? "choose"} initialAppID={params.get("app") ?? ""} canCreate={canCreate} onPublished={published} backTo={`/integrations/unified-apps/templates?q=${encodeURIComponent(query)}&offset=${offset}&private=${Number(privateOnly)}`} />;
  return <div className="space-y-6">
    <TemplateHeader canPublish={canPublish} canCreate={canCreate} onPublish={() => togglePublisher(!publishOpen)} />
    <TemplateLibraryContent privateOnly={privateOnly} search={search} setSearch={setSearch} submitSearch={submitSearch} browse={browse} loading={loading} error={error} total={total} offset={offset} items={items} onSelect={(release) => { setDetailError(""); setSelected(release); }} />
    {/* Source inspection overlays the catalogue instead of adding a second full-width page section. */}
    {selected && <TemplateDetailsSidebar key={selected.id} selected={selected} canCreate={canCreate} canPublish={canPublish} saving={saving} error={detailError} onClose={() => setSelected(null)} onVisibility={changeVisibility} />}

  </div>;
}

type TemplateHeaderProps = { canPublish: boolean; canCreate: boolean; onPublish: () => void };

/** Matches the Services header with one compact creation dropdown at every viewport size. */
function TemplateHeader(props: TemplateHeaderProps) {
  return <><PageBackLink to="/integrations/sdks?type=unified_app">Back to Unified Apps</PageBackLink><CataloguePageHeader title="Unified App templates" icon={Layers3} description="Start from a ready-made app and make it yours." actions={<TemplateHeaderActions {...props} />} /></>;
}

/** Groups the two creation paths without relaxing either workspace permission. */
function TemplateHeaderActions({ canPublish, canCreate, onPublish }: TemplateHeaderProps) {
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const id = useId();
  /** Dismisses the dropdown and returns keyboard focus to its single trigger. */
  function close() { setOpen(false); trigger.current?.focus(); }
  /** Closes the menu before switching to the dedicated template publisher. */
  function publish() { close(); onPublish(); }
  // Readers without either creation permission should not see an empty dropdown.
  if (!canPublish && !canCreate) return null;
  const actionClass = "flex min-h-11 w-full items-center gap-3 rounded-lg px-3 py-2 text-left text-sm text-slate-700 hover:bg-slate-50 focus:bg-slate-50";
  return <>
    <button ref={trigger} type="button" aria-expanded={open} aria-controls={id} aria-haspopup="dialog" className={catalogueActionClassName} onClick={() => setOpen(previous => !previous)}><Plus className="h-3.5 w-3.5" aria-hidden="true" />New<ChevronDown className="h-3.5 w-3.5" aria-hidden="true" /></button>
    {/* The shared popover handles viewport placement, outside clicks, and Escape dismissal. */}
    {open && <AnchoredPopover anchor={trigger} onClose={close} width={232}><div id={id} role="dialog" aria-label="New template or app" className="space-y-1 p-1.5">
      {/* Only authorized commands enter the dropdown and its initial focus order. */}
      {canPublish && <button type="button" autoFocus onClick={publish} className={actionClass}><Upload className="h-4 w-4" aria-hidden="true" />Publish template</button>}
      {canCreate && <Link to="/integrations/unified-apps/new" autoFocus={!canPublish} onClick={close} className={actionClass}><Plus className="h-4 w-4" aria-hidden="true" />Create app</Link>}
    </div></AnchoredPopover>}
  </>;
}

/** Invalid page URLs return to the first bounded catalogue page. */
function templateOffset(value: string | null): number { return Math.floor(Math.max(0, Number.parseInt(value ?? "0", 10) || 0) / 20) * 20; }

interface LibraryProps {
  privateOnly: boolean; search: string; setSearch: (value: string) => void; submitSearch: (event: FormEvent) => void;
  browse: (next: { q?: string; offset?: number; privateOnly?: boolean }) => void;
  loading: boolean; error: string; total: number; offset: number; items: UnifiedRelease[]; onSelect: (release: UnifiedRelease) => void;
}

/** Keeps the combined library and pagination mounted independently of the detail sidebar. */
function TemplateLibraryContent({ privateOnly, search, setSearch, submitSearch, browse, loading, error, total, offset, items, onSelect }: LibraryProps) {
  // The Services-style collection keeps filtering above the grid and server totals beneath it.
  return (
    <div className="min-w-0 space-y-6">
      <div className="min-w-0 space-y-6">
        <div className="flex flex-wrap items-center gap-3"><form role="search" onSubmit={submitSearch} className="relative min-w-[200px] flex-1"><button type="submit" aria-label="Search" className="absolute left-2.5 top-1/2 -translate-y-1/2 cursor-pointer text-slate-400 hover:text-slate-600"><Search className="h-3.5 w-3.5" /></button><input aria-label="Search Unified App templates" className="w-full rounded-lg border border-slate-300 py-2 pl-9 pr-8 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500" placeholder="Search templates" value={search} maxLength={200} onChange={(event) => setSearch(event.target.value)} /></form><label className="inline-flex min-h-9 shrink-0 cursor-pointer items-center gap-2 text-sm text-slate-600"><input type="checkbox" checked={privateOnly} onChange={(event) => browse({ privateOnly: event.target.checked })} className="h-4 w-4 rounded border-slate-300 accent-slate-950" />Private only</label></div>
        {/* Failed queries stay distinct from a successful empty search. */}
        {error && <p role="alert" className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{error}</p>}
        <TemplateResults loading={loading} error={error} items={items} onSelect={onSelect} />
        {/* Failed or pending requests must not advertise stale page counts. */}
        {!loading && !error && <TemplatePagination total={total} offset={offset} count={items.length} onPage={(nextOffset) => browse({ offset: nextOffset })} />}

      </div>
    </div>
  );
}

/** Keeps Services-style pagination visible even when the current library fits on one page. */
function TemplatePagination({ total, offset, count, onPage }: { total: number; offset: number; count: number; onPage: (offset: number) => void }) {
  const page = Math.floor(offset / 20) + 1;
  const pages = Math.max(1, Math.ceil(total / 20));
  return <nav aria-label="Template pages" className="flex flex-wrap items-center justify-between gap-3 border-t border-slate-100 px-1 py-3">
    {/* Empty and stale page URLs should never display an inverted result range. */}
    <p className="text-xs text-slate-500">{count === 0 ? "0" : `${offset + 1}–${offset + count}`} of {total}</p>
    <div className="flex items-center gap-1">
      <button type="button" aria-label="Previous page" title="Previous" disabled={offset === 0} onClick={() => onPage(Math.max(0, offset - 20))} className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40"><ChevronLeft className="h-4 w-4" /></button>
      <span className="pl-2 text-xs text-slate-500">Page</span>
      <Select aria-label="Template page" density="compact" className="mx-1" value={page} onChange={(event) => onPage((Number(event.target.value) - 1) * 20)}>
        {/* Preserve navigation out of a stale page URL without allocating options beyond the known total. */}
        {page > pages && <option value={page}>{page}</option>}
        {Array.from({ length: pages }, (_, index) => <option key={index + 1} value={index + 1}>{index + 1}</option>)}
      </Select>
      <span className="pr-2 text-xs text-slate-500">of {pages}</span>
      <button type="button" aria-label="Next page" title="Next" disabled={offset + count >= total} onClick={() => onPage(offset + 20)} className="rounded-md p-1.5 text-slate-500 hover:bg-slate-100 disabled:opacity-40"><ChevronRight className="h-4 w-4" /></button>
    </div>
  </nav>;
}
