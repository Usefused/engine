import { useState, type ReactNode } from "react";
import { Check, ChevronDown, ChevronUp, Copy } from "lucide-react";

/** Shares the endpoint inspector's compact identity strip with workflow operations. */
export function OperationDetailsHeader({ badge, badgeClass, identity, copyLabel, kind, kindClass, deprecated, onClose, copyTrack, closeTrack }: {
  badge: string; badgeClass: string; identity: string; copyLabel: string;
  kind?: string; kindClass?: string; deprecated?: boolean; onClose: () => void; copyTrack?: string; closeTrack?: string;
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
    <button type="button" data-track={closeTrack} onClick={onClose} aria-label="Close operation details" className="rounded-full p-2 text-slate-400 hover:bg-slate-100">✕</button>
  </div>;
}

/** Uses the endpoint response section's disclosure treatment for schemas and authored workflow details. */
export function OperationDisclosure({ label, badge, children, track }: { label: string; badge?: ReactNode; children: ReactNode; track?: string }) {
  const [open, setOpen] = useState(false);
  return <div className="overflow-hidden rounded-md border border-slate-200 shadow-sm">
    {/* Expansion highlights the active contract without changing its content. */}
    <button type="button" data-track={track} aria-expanded={open} onClick={() => setOpen(!open)} className={`flex w-full cursor-pointer items-center justify-between gap-2 px-4 py-2.5 text-left transition-colors ${open ? "border-b border-slate-200 bg-slate-50" : "bg-white hover:bg-slate-50/50"}`}>
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
      <h3 className="mb-3 border-b pb-2 text-sm font-semibold">{title}</h3>
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
