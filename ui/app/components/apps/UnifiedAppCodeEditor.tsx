import { useState } from "react";
import { TypeScriptEditor } from "~/components/code/TypeScriptEditor";

/** Keeps Unified App code in one visible TypeScript editor while the setup controls supply deployment metadata. */
export function UnifiedAppCodeEditor({ source, disabled, onSource }: {
  source: string; disabled?: boolean; onSource: (source: string) => void;
}) {
  // The initial source remains the comparison baseline for later edits in this editor session.
  const [baseline] = useState(source);
  return <section className="space-y-3" aria-label="App code">
    <TypeScriptEditor id="unified-app-source" baseline={baseline} required value={source} disabled={disabled} onChange={onSource} />
  </section>;
}
