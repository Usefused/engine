import { AlertTriangle } from "lucide-react";

export function ServiceHeaderWarningIcon({ warningCount }: { warningCount: number }) {
  if (!warningCount) return null;
  return (
    <div className="relative group">
      <AlertTriangle className="w-4 h-4 text-amber-500" />
      <div className="absolute left-0 top-full z-20 mt-2 hidden w-72 rounded-md border border-amber-200 bg-white p-3 text-xs text-slate-700 shadow-lg group-hover:block">
        {warningCount} endpoint{warningCount === 1 ? "" : "s"} need review after docs extraction.
      </div>
    </div>
  );
}

export function WarningRow({ warning }: { warning: any }) {
  return (
    <div className="py-2 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        <span className="rounded bg-white/80 px-1.5 py-0.5 text-[11px] font-semibold uppercase text-amber-800 border border-amber-200">
          {warning.method}
        </span>
        <span className="font-mono text-xs text-slate-800 break-all">{warning.path}</span>
      </div>
      {warning.reasons?.length > 0 && (
        <p className="mt-1 text-xs text-amber-800">{warning.reasons.join(" ")}</p>
      )}
    </div>
  );
}

export function ImportWarningPanel({
  warnings,
  onClear,
}: {
  warnings: any[];
  onClear: () => void;
}) {
  if (!warnings || warnings.length === 0) return null;

  return (
    <div className="border border-amber-200 bg-amber-50 rounded-lg px-4 py-3">
      <div className="flex items-start justify-between gap-4">
        <div className="flex items-start gap-3">
          <AlertTriangle className="w-4 h-4 mt-0.5 shrink-0 text-amber-600" />
          <div>
            <p className="text-sm font-medium text-amber-900">Some endpoints need review</p>
            <p className="text-sm text-amber-800 mt-1">
              The imported source did not contain complete request or response details. Import a corrected source to replace this contract version.
            </p>
          </div>
        </div>
        <div className="flex items-center gap-3">
          <button
            data-track="clear_import_warnings"
            type="button"
            onClick={onClear}
            className="text-xs font-medium text-amber-800 hover:text-amber-950"
          >
            Clear
          </button>
        </div>
      </div>
      <div className="mt-3 divide-y divide-amber-200/70 border-t border-amber-200/70">
        {warnings.slice(0, 8).map((warning: any) => (
          <WarningRow key={warning.id} warning={warning} />
        ))}
        {warnings.length > 8 && (
          <div className="py-2 text-xs text-amber-800">
            +{warnings.length - 8} more endpoint{warnings.length - 8 === 1 ? "" : "s"} need review.
          </div>
        )}
      </div>
    </div>
  );
}
