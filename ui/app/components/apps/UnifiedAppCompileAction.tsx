import { ArrowRight, Loader2 } from "lucide-react";

type Props = {
  compiling: boolean;
  progress: string;
  disabled: boolean;
  className: string;
  onCompile: () => void;
};

/** Keeps compilation feedback beside the action even when the form header is off screen. */
export function UnifiedAppCompileAction({ compiling, progress, disabled, className, onCompile }: Props) {
  return <div className="space-y-3">
    {/* The running label and spinner distinguish pending work from an incomplete form. */}
    <button type="button" className={className} disabled={disabled || compiling} aria-busy={compiling} onClick={onCompile}>
      {compiling ? <><Loader2 aria-hidden="true" className="h-4 w-4 shrink-0 animate-spin" /> Validating and compiling…</> : <>Validate and compile <ArrowRight aria-hidden="true" className="h-4 w-4" /></>}
    </button>
    {/* A persistent live region announces new stages without moving focus away from the action. */}
    <p role="status" aria-live="polite" aria-atomic="true" className="text-sm text-slate-600">
      {compiling ? progress : ""}
    </p>
  </div>;
}
