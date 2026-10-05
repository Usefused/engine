import { useState } from "react";
import { TypeScriptEditor } from "~/components/code/TypeScriptEditor";

/** Keeps source and portable YAML in adjacent views while preserving invalid YAML for correction. */
export function UnifiedAppCodeEditor({ source, yaml, disabled, error, onSource, onYAML, onViewChange }: {
  source: string; yaml: string; disabled?: boolean; error: string;
  onSource: (source: string) => void; onYAML: (yaml: string) => void; onViewChange: (yaml: boolean) => void;
}) {
  const [view, setView] = useState<"typescript" | "yaml">("typescript");
  const [text, setText] = useState(yaml);
  /** YAML snapshots the current form when entered; invalid edits remain visible until repaired. */
  function select(next: "typescript" | "yaml") {
    // Prevent leaving invalid text behind while showing a deployable-looking TypeScript view.
    if (error || disabled || next === view) return;
    // Start each YAML session from the latest form state, including auth changes.
    if (next === "yaml") setText(yaml);
    setView(next); onViewChange(next === "yaml");
  }
  /** Every keystroke invalidates the reviewed plan, including incomplete YAML. */
  function edit(value: string) { setText(value); onYAML(value); }
  return <section className="space-y-3" aria-label="App code and configuration">
    <div className="inline-flex rounded-lg bg-slate-100 p-1" aria-label="Editor view">
      <button type="button" aria-pressed={view === "typescript"} disabled={disabled || Boolean(error)} onClick={() => select("typescript")} className={`rounded-md px-3 py-1.5 text-sm ${view === "typescript" ? "bg-white font-semibold text-slate-900 shadow-sm" : "text-slate-600"}`}>TypeScript</button>
      <button type="button" aria-pressed={view === "yaml"} disabled={disabled} onClick={() => select("yaml")} className={`rounded-md px-3 py-1.5 text-sm ${view === "yaml" ? "bg-white font-semibold text-slate-900 shadow-sm" : "text-slate-600"}`}>YAML config</button>
    </div>
    {/* Each view edits its own file; the same draft supplies the compile request. */}
    {view === "typescript" ? <TypeScriptEditor id="unified-app-source" required value={source} disabled={disabled} onChange={onSource} /> : <>
      <p className="text-xs text-slate-500">App settings and service authentication. <code>source_path: app.ts</code> points to the TypeScript view. Use the service picker to change providers or versions.</p>
      <TypeScriptEditor id="unified-app-yaml" language="yaml" value={text} disabled={disabled} onChange={edit} />
    </>}
    {error ? <p role="alert" className="text-sm text-red-700">{error}</p> : null}
  </section>;
}
