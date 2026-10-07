import { useEffect, useState } from "react";
import { TypeScriptEditor } from "~/components/code/TypeScriptEditor";

/** Shares the route-owned view with the agent while preserving invalid YAML for correction. */
export function UnifiedAppCodeEditor({ source, yaml, view, disabled, error, onSource, onYAML, onViewChange }: {
  source: string; yaml: string; view: "typescript" | "yaml"; disabled?: boolean; error: string;
  onSource: (source: string) => void; onYAML: (yaml: string) => void; onViewChange: (yaml: boolean) => void;
}) {
  const [text, setText] = useState(yaml);
  // One comparison baseline survives tab switches and agent edits for this editor session.
  const [baseline] = useState(() => ({ source, yaml }));
  // Both manual and agent view changes snapshot the latest config without overwriting in-progress YAML edits.
  useEffect(() => {
    // Leaving YAML prepares the next entry from the authoritative form draft.
    if (view === "typescript") setText(yaml);
  }, [view, yaml]);
  /** YAML snapshots the current form when entered; invalid edits remain visible until repaired. */
  function select(next: "typescript" | "yaml") {
    // Prevent leaving invalid text behind while showing a deployable-looking TypeScript view.
    if (error || disabled || next === view) return;
    // Start each YAML session from the latest form state, including auth changes.
    if (next === "yaml") setText(yaml);
    onViewChange(next === "yaml");
  }
  /** Every keystroke invalidates the reviewed plan, including incomplete YAML. */
  function edit(value: string) { setText(value); onYAML(value); }
  return <section className="space-y-3" aria-label="App code and configuration">
    <div className="inline-flex rounded-lg bg-slate-100 p-1" aria-label="Editor view">
      <button type="button" aria-pressed={view === "typescript"} disabled={disabled || Boolean(error)} onClick={() => select("typescript")} className={`rounded-md px-3 py-1.5 text-sm ${view === "typescript" ? "bg-white font-semibold text-slate-900 shadow-sm" : "text-slate-600"}`}>TypeScript</button>
      <button type="button" aria-pressed={view === "yaml"} disabled={disabled} onClick={() => select("yaml")} className={`rounded-md px-3 py-1.5 text-sm ${view === "yaml" ? "bg-white font-semibold text-slate-900 shadow-sm" : "text-slate-600"}`}>YAML config</button>
    </div>
    {/* Each view edits its own file; the same draft supplies the compile request. */}
    {view === "typescript" ? <TypeScriptEditor id="unified-app-source" baseline={baseline.source} required value={source} disabled={disabled} onChange={onSource} /> : <>
      <p className="text-xs text-slate-500">App settings and service authentication. <code>source_path: app.ts</code> points to the TypeScript view. Use the service picker to change providers or versions.</p>
      {/* The visible YAML buffer stays intact, including incomplete input, until the user corrects it. */}
      <TypeScriptEditor id="unified-app-yaml" baseline={baseline.yaml} language="yaml" value={text} disabled={disabled} onChange={edit} />
    </>}
    {error ? <p role="alert" className="text-sm text-red-700">{error}</p> : null}
  </section>;
}
