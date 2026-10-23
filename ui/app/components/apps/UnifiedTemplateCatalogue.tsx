import { useLayoutEffect, useRef, useState, type KeyboardEvent } from "react";
import { Link } from "@remix-run/react";
import { Code2, Layers3, Loader2, X } from "lucide-react";
import type { UnifiedRelease } from "~/lib/unified-app-contract";

/** Matches service visibility badges so discoverability reads consistently across catalogues. */
export function TemplateVisibilityBadge({ isPublic }: { isPublic: boolean }) {
  // Public discovery uses the service catalogue's blue badge; private releases stay neutral.
  return <span className={`rounded-full px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${isPublic ? "bg-blue-100 text-blue-700" : "bg-slate-100 text-slate-600"}`}>{isPublic ? "Public" : "Private"}</span>;
}

/** Bounds card content so several releases remain scannable without exposing their source inline. */
export function TemplateResults({ loading, error, items, onSelect }: { loading: boolean; error: string; items: UnifiedRelease[]; onSelect: (release: UnifiedRelease) => void }) {
  // A new query must never display an earlier page as its results.
  if (loading) return <p role="status" className="flex items-center gap-2 py-10 text-sm text-slate-500"><Loader2 className="h-4 w-4 animate-spin" />Loading templates…</p>;
  // Failed requests have their own alert and must not masquerade as empty libraries.
  if (error) return null;
  // The combined library shares one empty state after server-side search.
  if (!items.length) return <section className="rounded-xl border border-dashed border-slate-200 bg-white px-6 py-14 text-center"><Layers3 className="mx-auto h-8 w-8 text-slate-300" /><h3 className="mt-4 font-semibold text-slate-900">No templates found</h3><p className="mt-1 text-sm text-slate-500">Try searching for a different name, description, or category.</p></section>;
  return <div className="grid grid-cols-1 items-start gap-4 md:grid-cols-2 xl:grid-cols-3">{items.map((release) => <TemplateCard key={release.id} release={release} onSelect={onSelect} />)}</div>;
}

/** Uses the Services card hierarchy and a single full-card action to keep larger libraries scannable. */
function TemplateCard({ release, onSelect }: { release: UnifiedRelease; onSelect: (release: UnifiedRelease) => void }) {
  const services = Object.values(release.template.services);
  const operationCount = services.reduce((total, pin) => total + pin.operations.length, 0);
  return <article className="group relative flex cursor-pointer flex-col overflow-hidden rounded-2xl border border-slate-200/90 bg-white p-5 shadow-sm transition-[border-color,box-shadow] duration-200 hover:border-black hover:shadow-lg hover:shadow-slate-200/70">
    <button type="button" aria-label={`View template: ${release.template.name}`} onClick={() => onSelect(release)} className="absolute inset-0 z-10 cursor-pointer rounded-2xl focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-blue-600" />
    <div className="pointer-events-none">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-slate-100 text-slate-600"><Layers3 className="h-5 w-5" aria-hidden="true" /></span>
          <div className="min-w-0"><h3 title={release.template.name} className="truncate text-sm font-semibold text-slate-900 group-hover:text-blue-700">{release.template.name}</h3><p className="mt-1 truncate text-[11px] text-slate-400">By {release.publisher}</p></div>
        </div>
        <div className="flex shrink-0 items-center gap-1.5"><TemplateVisibilityBadge isPublic={release.public} /><span className="rounded-md bg-slate-100 px-2 py-0.5 text-[10px] font-medium text-slate-600">{release.template.version}</span></div>
      </div>
      <p className="mt-4 line-clamp-2 break-words text-sm leading-6 text-slate-600">{release.template.description}</p>
      <div className="mt-4 flex overflow-hidden rounded-lg border border-slate-100 bg-slate-50/80 text-slate-600" aria-label="Template contents">
        {/* Singular labels keep one-service templates consistent with service contract totals. */}
        <span className="flex-1 px-3 py-2"><strong className="block text-sm font-semibold text-slate-800">{services.length}</strong><span className="text-[10px] font-medium uppercase tracking-wide">{services.length === 1 ? "Service" : "Services"}</span></span>
        <span className="flex-1 border-l border-slate-200/70 px-3 py-2"><strong className="block text-sm font-semibold text-slate-800">{operationCount}</strong><span className="text-[10px] font-medium uppercase tracking-wide">{operationCount === 1 ? "Operation" : "Operations"}</span></span>
      </div>
    </div>
  </article>;
}

interface DetailsProps { selected: UnifiedRelease; canCreate: boolean; canPublish: boolean; saving: boolean; error: string; onClose: () => void; onVisibility: (release: UnifiedRelease) => Promise<void> }

/** Uses a native modal sidebar for focus containment, Escape dismissal, and focus restoration to the invoking card. */
export function TemplateDetailsSidebar({ selected, canCreate, canPublish, saving, error, onClose, onVisibility }: DetailsProps) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [confirming, setConfirming] = useState(false);
  // Native modal behavior keeps background cards out of keyboard navigation while the inspector is open.
  useLayoutEffect(() => {
    const panel = dialog.current;
    const trigger = document.activeElement;
    const overflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    panel?.showModal();
    // Restore focus before React detaches the modal, and preserve the page's previous scroll policy.
    return () => {
      panel?.close();
      document.body.style.overflow = overflow;
      // Navigation may remove the invoking card while a request is in flight.
      if (trigger instanceof window.HTMLElement && trigger.isConnected) trigger.focus({ preventScroll: true });
    };
  }, []);
  return <dialog ref={dialog} aria-labelledby="template-detail-title" onCancel={onClose} onKeyDown={containTemplateFocus} className="fixed inset-y-0 left-auto right-0 m-0 h-dvh max-h-dvh w-full max-w-[640px] border-0 border-l border-slate-200 bg-white p-0 text-slate-900 shadow-2xl backdrop:bg-slate-900/25">
    <div className="flex h-full min-w-0 flex-col">
      <header className="flex shrink-0 items-center justify-between gap-4 border-b border-slate-100 px-6 py-4"><span className="text-sm font-medium text-slate-500">Template details</span><button type="button" onClick={onClose} aria-label="Close template details" className="rounded-lg p-2 text-slate-400 hover:bg-slate-100 hover:text-slate-900"><X className="h-5 w-5" /></button></header>
      <div className="min-h-0 flex-1 space-y-7 overflow-y-auto overscroll-contain p-6">
        <section><div className="mb-4 flex items-center gap-3"><TemplateVisibilityBadge isPublic={selected.public} /><span className="text-xs text-slate-400">v{selected.template.version}</span></div><h2 id="template-detail-title" className="break-words text-xl font-semibold">{selected.template.name}</h2><p className="mt-2 text-sm leading-6 text-slate-500">{selected.template.description}</p><p className="mt-3 text-xs text-slate-400">Published by {selected.publisher}</p></section>
        <section><h3 className="text-sm font-semibold">Included services</h3><ul className="mt-3 divide-y divide-slate-100 rounded-lg border border-slate-200 px-4">{Object.entries(selected.template.services).map(([key, pin]) => <li key={key} className="min-w-0 py-3"><p className="break-all text-sm font-medium">{key}</p><p className="mt-1 text-xs text-slate-500">{pin.version} · {pin.operations.length} selected {/* Match the operation count without implying extra bindings. */}{pin.operations.length === 1 ? "operation" : "operations"}</p><p className="mt-2 break-words text-xs leading-5 text-slate-500">{pin.operations.join(", ")}</p></li>)}</ul></section>
        {/* Only authored setup requirements deserve a section; an empty list adds visual noise. */}
        {selected.template.requirements.length > 0 && <section><h3 className="text-sm font-semibold">Setup requirements</h3><ul className="mt-3 list-inside list-disc space-y-2 text-sm text-slate-500">{selected.template.requirements.map((requirement) => <li key={requirement}>{requirement}</li>)}</ul></section>}
        <details className="rounded-lg border border-slate-200"><summary className="flex cursor-pointer items-center gap-2 p-4 text-sm font-medium"><Code2 className="h-4 w-4 text-slate-400" />View source code</summary><pre className="max-h-96 overflow-auto bg-slate-950 p-4 text-xs leading-6 text-slate-100"><code>{selected.template.source}</code></pre></details>
        <p className="text-xs leading-5 text-slate-400">Credentials and bucket bindings are not included. Connect your own accounts when creating an app.</p>
      </div>
      <footer className="shrink-0 space-y-3 border-t border-slate-200 bg-white p-5">
        {/* Mutation errors belong beside the action, not behind the modal in the catalogue. */}
        {error && <p role="alert" className="rounded-lg bg-red-50 p-3 text-sm text-red-700">{error}</p>}
        {/* Visibility confirmation stays inside the focus-contained sidebar. */}
        {confirming ? <div className="space-y-3"><p className="text-sm text-slate-600">{selected.public ? "Make this template private? Other catalogue users will no longer discover it." : "Make this template public? Other catalogue users will be able to read its source and use it."}</p><div className="flex gap-3"><button disabled={saving} onClick={async () => { await onVisibility(selected); setConfirming(false); }} className="rounded-lg bg-slate-950 px-4 py-2 text-sm font-medium text-white disabled:opacity-50">{saving ? "Saving…" : "Confirm visibility change"}</button><button disabled={saving} onClick={() => setConfirming(false)} className="px-3 py-2 text-sm text-slate-500">Cancel</button></div></div> : <div className="flex flex-wrap items-center gap-3">
          {/* Creation and visibility changes retain separate workspace and ownership permissions. */}
          {canCreate && <Link to={`/integrations/unified-apps/new?template=${encodeURIComponent(selected.id)}`} className="rounded-lg bg-slate-950 px-5 py-2.5 text-sm font-medium text-white">Use template</Link>}
          {canPublish && selected.is_owner && <button disabled={saving} onClick={() => setConfirming(true)} className="rounded-lg border border-slate-200 px-4 py-2.5 text-sm font-medium text-slate-600">Manage visibility</button>}
        </div>}
      </footer>
    </div>
  </dialog>;
}

/** Keeps keyboard traversal inside the inspector, including the browser chrome boundary. */
function containTemplateFocus(event: KeyboardEvent<HTMLDialogElement>) {
  // Native dialog behavior continues to own Escape and all non-navigation keys.
  if (event.key !== "Tab") return;
  // Hidden source content must not enter the tab sequence until its disclosure is expanded.
  const targets = Array.from(event.currentTarget.querySelectorAll<HTMLElement>('button:not(:disabled), a[href], summary, input:not(:disabled), [tabindex="0"]')).filter(element => element.getClientRects().length > 0);
  const first = targets[0], last = targets[targets.length - 1];
  // A temporarily empty inspector must not let focus escape to the inert catalogue.
  if (!first || !last) { event.preventDefault(); return; }
  // Wrap both boundaries while leaving normal traversal between controls to the browser.
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
}
