import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { useFusedAgent } from "~/components/agent/FusedAgentContext";
import { Check, ChevronDown, ChevronUp, ChevronRight, Code2, Copy } from "lucide-react";

/** Shares the endpoint inspector identity strip with workflow and MCP definitions. */
export function OperationDetailsHeader({ badge, badgeClass, identity, copyLabel, kind, kindClass, deprecated, onClose, copyTrack, closeTrack, closeLabel = "Close operation details" }: {
  badge: string; badgeClass: string; identity: string; copyLabel: string;
  kind?: string; kindClass?: string; deprecated?: boolean; onClose: () => void; copyTrack?: string; closeTrack?: string; closeLabel?: string;
}) {
  const [copied, setCopied] = useState(false);
  // Copy stays local and only reports success after the clipboard accepts the value.
  async function copyIdentity() {
    try {
      await navigator.clipboard.writeText(identity);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // A denied clipboard request must never claim the identity was copied.
      setCopied(false);
    }
  }
  return <div className="sticky top-0 z-10 flex min-w-0 items-center justify-between gap-2 border-b border-slate-100 bg-white/90 p-4 backdrop-blur sm:p-6">
    <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
      <span className={`shrink-0 rounded px-2 py-1 text-xs font-bold ${badgeClass}`}>{badge}</span>
      <code className="min-w-0 flex-1 break-all text-sm text-slate-800">{identity}</code>
      <button type="button" data-track={copyTrack} onClick={copyIdentity} className="rounded p-1 text-slate-400 hover:bg-slate-100" title={copyLabel} aria-label={copyLabel}>
        {/* Clipboard confirmation is transient and never alters operation selection. */}
        {copied ? <Check className="h-3.5 w-3.5 text-green-500" /> : <Copy className="h-3.5 w-3.5" />}
      </button>
      {/* Some protocols are fully identified by the primary badge. */}
      {kind && <span className={`rounded border px-1.5 py-0.5 text-[10px] font-bold uppercase ${kindClass}`}>{kind}</span>}
      {/* Only an explicitly deprecated contract receives this warning. */}
      {deprecated && <span className="rounded border border-red-200 bg-red-50 px-2 py-0.5 text-xs font-semibold uppercase text-red-700">Deprecated</span>}
    </div>
    <button type="button" data-track={closeTrack} onClick={onClose} aria-label={closeLabel} className="rounded-full p-2 text-slate-400 hover:bg-slate-100">✕</button>
  </div>;
}

/** Uses the endpoint response section's disclosure treatment for schemas and authored workflow details. */
export function OperationDisclosure({ label, badge, children, track }: { label: string; badge?: ReactNode; children: ReactNode; track?: string }) {
  const [open, setOpen] = useState(false);
  return <div className="overflow-hidden rounded-md bg-slate-50">
    {/* Expansion highlights the active contract without changing its content. */}
    <button type="button" data-track={track} aria-expanded={open} onClick={() => setOpen(!open)} className={`flex w-full cursor-pointer items-center justify-between gap-2 px-4 py-2.5 text-left transition-colors ${open ? "bg-slate-50" : "bg-slate-50 hover:bg-slate-100"}`}>
      <span className="flex min-w-0 flex-wrap items-center gap-2">{badge}<span className="text-xs font-medium text-slate-900">{label}</span></span>
      {/* The disclosure icon always reflects the locally expanded state. */}
      {open ? <ChevronUp className="h-4 w-4 shrink-0 text-slate-500" /> : <ChevronDown className="h-4 w-4 shrink-0 text-slate-500" />}
    </button>
    {/* Unopened schemas remain unmounted, matching endpoint response behavior. */}
    {open && children}
  </div>;
}

export interface OperationParameter {
  name: string;
  in: string;
  type: string;
  required: boolean;
  path_encoding?: string;
}

// ParameterEncodingNote surfaces only an explicit non-default wire decision so
// ordinary textual parameters are not burdened with a meaningless default.
function ParameterEncodingNote({ pathEncoding }: { pathEncoding?: string }) {
  // Default encoding needs no extra annotation in a compact row.
  if (!pathEncoding) return null;
  // Slash preservation is the only currently supported override and deserves
  // product language instead of its internal contract identifier.
  const label = pathEncoding === "preserve_slashes"
    ? "Preserves slashes"
    : `Encoding: ${pathEncoding.replace(/[_-]+/g, " ")}`;
  return <span className="mt-1 block break-words font-sans text-[10px] text-slate-500">{label}</span>;
}

// ParameterRequirementBadge makes required state scannable without consuming
// the width of separate Yes and No values.
function ParameterRequirementBadge({ required }: { required: boolean }) {
  // Requirement badges use the same semantic colors for physical and unified inputs.
  const style = required
    ? "bg-blue-50 text-blue-700 ring-blue-200"
    : "bg-slate-50 text-slate-500 ring-slate-200";
  return <span className={`inline-flex rounded-full px-2 py-0.5 text-[10px] font-semibold ring-1 ring-inset ${style}`}>{required ? "Required" : "Optional"}</span>;
}

// ParameterRow keeps long identifiers contained while retaining the complete
// name in both visible wrapped text and the native hover tooltip.
function ParameterRow({ parameter }: { parameter: OperationParameter }) {
  return (
    <tr className="border-t border-slate-200 align-top">
      <td className="w-[46%] px-3 py-3 sm:px-4">
        <code className="break-all text-xs text-slate-900" title={parameter.name}>{parameter.name}</code>
        <ParameterEncodingNote pathEncoding={parameter.path_encoding} />
      </td>
      <td className="w-[18%] px-2 py-3 text-xs capitalize text-slate-600 sm:px-4">{parameter.in}</td>
      <td className="w-[18%] break-all px-2 py-3 font-mono text-xs text-slate-700 sm:px-4">{parameter.type}</td>
      <td className="w-[18%] px-2 py-3 sm:px-4"><ParameterRequirementBadge required={parameter.required} /></td>
    </tr>
  );
}

// ParameterTable presents exact type and location data without forcing the
// sidebar into a horizontally scrolling desktop-width table.
export function OperationParameterTable({ parameters, title = "Parameters" }: { parameters: OperationParameter[]; title?: string }) {
  // Empty contracts do not imply that parameters have been inferred.
  if (parameters.length === 0) return null;
  return (
    <div>
      <h3 className="mb-3 text-sm font-semibold">{title}</h3>
      <div className="overflow-hidden rounded-lg border border-slate-200">
        <table className="w-full table-fixed text-left text-sm">
          <thead className="bg-slate-100 text-xs text-slate-700">
            <tr>
              <th className="w-[46%] px-3 py-2 sm:px-4">Name</th>
              <th className="w-[18%] px-2 py-2 sm:px-4">Location</th>
              <th className="w-[18%] px-2 py-2 sm:px-4">Type</th>
              <th className="w-[18%] px-2 py-2 sm:px-4">Requirement</th>
            </tr>
          </thead>
          <tbody>{parameters.map((parameter) => <ParameterRow key={`${parameter.in}:${parameter.name}`} parameter={parameter} />)}</tbody>
        </table>
      </div>
    </div>
  );
}

/** Give service, webhook, and Bridge code a quiet shared disclosure with an explicit expansion affordance. */
export function OperationCodeExample({ label, language, code, copyLabel, description }: { label: string; language: string; code: string; copyLabel: string; description?: string }) {
  const [copyState, setCopyState] = useState<"idle" | "copied" | "failed">("idle");
  useEffect(/** Copy feedback belongs only to the displayed source and expires without retaining timers. */ () => {
    setCopyState("idle");
  }, [code]);
  useEffect(/** Restore the neutral copy control after its acknowledgement has been readable. */ () => {
    // Idle controls do not need a timer.
    if (copyState === "idle") return;
    const timer = setTimeout(/** Expire this acknowledgement without modifying the source. */ () => setCopyState("idle"), 2000);
    return /** Unmounting or copying again cancels obsolete feedback. */ () => clearTimeout(timer);
  }, [copyState]);
  /** Report clipboard success only after the browser accepts the exact displayed code. */
  async function copyExample() {
    try { await navigator.clipboard.writeText(code); setCopyState("copied"); }
    catch { setCopyState("failed"); }
  }
  return <details className="group/code overflow-hidden rounded-lg bg-slate-50">
    <summary className="flex cursor-pointer list-none items-center justify-between gap-4 rounded-lg px-4 py-3 text-xs font-medium text-slate-700 transition-colors hover:bg-slate-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-blue-500 [&::-webkit-details-marker]:hidden">
      <span className="flex min-w-0 items-center gap-2.5"><Code2 aria-hidden="true" className="h-4 w-4 shrink-0 text-slate-400" /><span>{label}</span></span>
      <span className="flex shrink-0 items-center gap-3"><span className="font-mono text-[10px] font-normal text-slate-400">{language}</span><ChevronRight aria-hidden="true" className="h-4 w-4 text-slate-400 transition-transform group-open/code:rotate-90 motion-reduce:transition-none" /></span>
    </summary>
    {/* Guidance belongs to the expanded example rather than competing with the contract sections. */}
    {description && <p className="px-3 pb-3 pt-1 text-xs leading-5 text-slate-500">{description}</p>}
    <div className="relative bg-slate-950">
      <button type="button" onClick={copyExample} className="absolute right-2 top-2 rounded p-1.5 text-slate-400 hover:bg-slate-800 hover:text-white" title={copyLabel} aria-label={copyLabel}>
        {/* Copy feedback never changes the underlying example or its transport. */}
        {copyState === "copied" ? <Check className="h-3.5 w-3.5 text-emerald-400" /> : <Copy className="h-3.5 w-3.5" />}
      </button>
      <span role="status" className="sr-only">{copyState === "copied" ? "Copied" : copyState === "failed" ? "Copy failed. Select the code to copy it." : ""}</span>
      <pre tabIndex={0} aria-label={`${label} code`} className="max-h-[28rem] overflow-auto overscroll-contain p-4 pr-10 text-[11px] leading-5 text-slate-200"><code>{code}</code></pre>
    </div>
  </details>;
}

/** Keep inspector facts readable through alignment and typography rather than borders or repeated cards. */
export function OperationMetadataList({ facts }: { facts: [string, string][] }) {
  return <dl className="grid min-w-0 grid-cols-[auto_minmax(0,1fr)] gap-x-5 gap-y-3 text-xs">
    {facts.map(/** Stable labels preserve identity when optional metadata appears or disappears. */ ([label, value]) => <div key={label} className="contents"><dt className="text-slate-500">{label}</dt><dd className="min-w-0 break-all font-mono leading-5 text-slate-700">{value}</dd></div>)}
  </dl>;
}

/** Own one inspector shell, focus lifecycle, and scrolling model for every operation source. */
export function OperationDetailsPanel({ label, onClose, header, children }: { label: string; onClose: () => void; header: ReactNode; children: ReactNode }) {
  const agent = useFusedAgent();
  const panel = useRef<HTMLElement>(null);
  useEffect(/** Move focus into the inspector without losing the invoking row or page position. */ () => {
    const trigger = document.activeElement as HTMLElement | null;
    const overflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    panel.current?.querySelector<HTMLButtonElement>('button[aria-label^="Close"]')?.focus();
    return /** Restore the user's position after dismissal. */ () => { document.body.style.overflow = overflow; trigger?.focus({ preventScroll: true }); };
  }, []);
  return <div className="contents">
    <div className="fixed inset-0 z-40 bg-slate-900/20" onClick={onClose} aria-hidden="true" />
    <aside ref={panel} data-fused-detail-sidebar role="dialog" aria-modal={!agent?.isOpen} aria-label={label} className="fixed inset-y-0 right-0 z-50 flex w-full flex-col overflow-hidden border-l border-slate-200 bg-white shadow-2xl md:w-[600px]" onKeyDown={/** Keep assistant access and keyboard dismissal consistent across sources. */ event => operationDetailsKeyboard(event, panel.current, onClose, !agent?.isOpen)}>
      {header}
      <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain">{children}</div>
    </aside>
  </div>;
}

/** Trap only modal inspector navigation; never invoke the selected operation. */
function operationDetailsKeyboard(event: KeyboardEvent<HTMLElement>, panel: HTMLElement | null, close: () => void, modal: boolean) {
  // Dismissal returns to the invoking row through the panel lifecycle.
  if (event.key === "Escape") { event.stopPropagation(); close(); return; }
  // An open assistant and ordinary keys retain normal browser navigation.
  if (!modal || event.key !== "Tab" || !panel) return;
  const controls = Array.from(panel.querySelectorAll<HTMLElement>('button:not(:disabled), a[href], summary, [tabindex="0"]')).filter(/** Collapsed contract controls cannot receive focus. */ element => element.getClientRects().length > 0);
  const first = controls[0], last = controls[controls.length - 1];
  // Wrap only at the visible edges of the modal inspector.
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
}

/** Apply one content rhythm to service, webhook, and Bridge inspectors. */
export function OperationDetailsBody({ children }: { children: ReactNode }) {
  return <div className="space-y-8 p-4 sm:p-6">{children}</div>;
}

/** Keep the operation identity separate from optional implementation details. */
export function OperationSummary({ title, description, notice, children }: { title: string; description?: ReactNode; notice?: ReactNode; children?: ReactNode }) {
  return <section className="space-y-5">
    <div><h2 className="break-words text-lg font-semibold tracking-tight text-slate-900">{title}</h2>
      {/* Unknown descriptions must not create an empty summary row. */}
      {description && <div className="mt-2 text-sm leading-6 text-slate-500">{description}</div>}
    </div>
    {notice}
    {children}
  </section>;
}

/** Present all protocol-specific identity behind the same optional disclosure. */
export function OperationTechnicalDetails({ facts, children }: { facts: [string, string][]; children?: ReactNode }) {
  return <OperationDisclosure label="Technical details"><div className="space-y-5 px-4 pb-4 pt-2"><OperationMetadataList facts={facts} />{children}</div></OperationDisclosure>;
}

/** Share section headings and spacing without changing the protocol's terminology. */
export function OperationSection({ title, children }: { title: string; children: ReactNode }) {
  return <section className="space-y-3"><h3 className="text-sm font-semibold">{title}</h3>{children}</section>;
}

/** Keep schema disclosure framing identical while retaining source-specific schema interpretation. */
export function OperationSchemaDisclosure({ label, badge, track, children }: { label: string; badge?: ReactNode; track?: string; children: ReactNode }) {
  return <OperationDisclosure label={label} badge={badge} track={track}><div className="overflow-x-auto bg-[#161c27] p-4">{children}</div></OperationDisclosure>;
}
