import { Select } from "~/components/forms/Select";
import type { SpecificationImportDiagnostic } from "~/lib/api";

/** Shows completed source corrections separately from contract details that remain unspecified. */
export function ImportDiagnostics({ diagnostics = [] }: { diagnostics?: SpecificationImportDiagnostic[] }) {
  // Older servers and empty plans need no diagnostic panel.
  if (!Array.isArray(diagnostics) || diagnostics.length === 0) return null;
  const corrected = diagnostics.filter(isCorrected).length;
  const incomplete = diagnostics.filter(isIncomplete).length;
  // Proposed changes are counted independently from repairs already accepted by the plan.
  const suggested = diagnostics.filter((item) => item.disposition === "suggested").length;
  return <section aria-label="Import corrections and review notes" className="rounded-xl border border-slate-200 bg-slate-50 p-4 text-sm">
    <h3 className="font-semibold text-slate-900">Corrections and review notes</h3>
    <p className="mt-1 text-slate-600">{corrected} corrected · {incomplete} unspecified{/* Report-only changes are proposals, not completed corrections. */}{suggested > 0 && ` · ${suggested} suggested`}</p>
    <ul className="mt-3 space-y-3">
      {diagnostics.map(renderDiagnostic)}
    </ul>
  </section>;
}

/** A repair counts as completed only when Registry explicitly records that outcome. */
function isCorrected(item: SpecificationImportDiagnostic): boolean {
  return Boolean(item.correction && item.disposition === "repaired");
}

/** Unspecified schemas remain visible limitations even though their operations are importable. */
function isIncomplete(item: SpecificationImportDiagnostic): boolean {
  return item.disposition === "incomplete";
}

/** Renders Registry explanations as escaped text, never as provider HTML or executable patches. */
function renderDiagnostic(item: SpecificationImportDiagnostic, index: number) {
  // Status labels must not describe an incomplete schema as a successful repair.
  const label = item.disposition === "suggested" ? "Suggested correction" : isCorrected(item) ? "Corrected" : isIncomplete(item) ? "Unspecified" : "Review note";
  return <li key={`${item.code}-${item.pointer}-${index}`} className="space-y-1">
    <span className="font-medium text-slate-900">{label}</span>
    <p className="text-slate-700">{item.message}</p>
    {/* Source coordinates let reviewers locate the correction without exposing raw provider payloads. */}
    {item.pointer && <details className="text-xs text-slate-500"><summary className="cursor-pointer">View source location</summary><code className="mt-1 block break-all">{item.pointer}</code></details>}
  </li>;
}

/** Uses the shared control for source policy; the returned plan locks this choice until replanning. */
export function ImportRepairMode({ value = "safe", onChange, disabled = false }: { value?: "safe" | "report_only"; onChange: (value: "safe" | "report_only") => void; disabled?: boolean }) {
  return <label className="block space-y-2 text-sm"><span className="font-medium text-slate-900">Source corrections</span><Select aria-label="Source corrections" className="w-full" value={value} disabled={disabled} onChange={(event) => onChange(event.target.value as "safe" | "report_only")}><option value="safe">Apply safe corrections automatically</option><option value="report_only">Report only — I’ll correct the source</option></Select><span className="block text-xs text-slate-500">Report only lists required corrections and blocks import until you resolve them.</span></label>;
}

/** Extracts only the diagnostic collection retained by the shared API error boundary. */
export function importErrorDiagnostics(error: unknown): SpecificationImportDiagnostic[] {
  const diagnostics = (error as { details?: { diagnostics?: unknown } })?.details?.diagnostics;
  // Unstructured transport failures have no source diagnostics to display.
  return Array.isArray(diagnostics) ? diagnostics.filter((item) => item && typeof item.message === "string" && typeof item.code === "string") : [];
}
