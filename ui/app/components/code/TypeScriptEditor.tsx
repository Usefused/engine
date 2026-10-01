import { useLayoutEffect, useMemo, useRef, type CSSProperties, type UIEvent, type KeyboardEvent } from "react";
import { indentCode } from "~/lib/code-indentation";
import SyntaxHighlighter from "react-syntax-highlighter/dist/esm/prism-light.js";
import typescript from "react-syntax-highlighter/dist/esm/languages/prism/typescript.js";

// Register only TypeScript and its grammar dependencies to keep the authoring bundle small.
SyntaxHighlighter.registerLanguage("typescript", typescript);

const tokenStyles: Record<string, CSSProperties> = {
  comment: { color: "#64748b" },
  prolog: { color: "#64748b" },
  keyword: { color: "#7c3aed" },
  string: { color: "#047857" },
  number: { color: "#b45309" },
  boolean: { color: "#b45309" },
  function: { color: "#2563eb" },
  "class-name": { color: "#0e7490" },
  operator: { color: "#475569" },
  punctuation: { color: "#64748b" },
  property: { color: "#334155" },
};
const textMetrics: CSSProperties = {
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace',
  fontSize: 12,
  lineHeight: "22px",
  letterSpacing: "normal",
  tabSize: 2,
  whiteSpace: "pre",
  overflowWrap: "normal",
  wordBreak: "normal",
};

interface TypeScriptEditorProps {
  id: string;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  required?: boolean;
}

/** Retains native editing and required-field semantics while a synchronized layer supplies TypeScript colors. */
export function TypeScriptEditor({ id, value, onChange, disabled, required }: TypeScriptEditorProps) {
  const textarea = useRef<HTMLTextAreaElement>(null);
  const pendingSelection = useRef<{ start: number; end: number; direction: "forward" | "backward" | "none" } | null>(null);
  const escapeTab = useRef(false);
  const highlight = useRef<HTMLDivElement>(null);
  const gutter = useRef<HTMLDivElement>(null);
  const lineCount = value.split("\n").length;
  // A trailing space preserves the final empty line without changing the authored source.
  const highlighted = useMemo(() => <SyntaxHighlighter language="typescript" style={tokenStyles} customStyle={{ ...textMetrics, margin: 0, padding: "16px 16px 16px 56px", border: 0, background: "transparent", overflow: "visible", minHeight: "100%", minWidth: "100%", width: "max-content" }} codeTagProps={{ style: textMetrics }}>{`${value} `}</SyntaxHighlighter>, [value]);

  /** Both decorative layers follow the textarea's native scroll so caret and tokens stay aligned. */
  function syncScroll(event: UIEvent<HTMLTextAreaElement>) {
    const { scrollTop, scrollLeft } = event.currentTarget;
    // Refs can be absent while the editor is mounting or leaving the page.
    if (highlight.current) { highlight.current.scrollTop = scrollTop; highlight.current.scrollLeft = scrollLeft; }
    if (gutter.current) gutter.current.scrollTop = scrollTop;
  }

  /** Passes every edit through the existing source state so previous compilation plans are invalidated. */
  function updateSource(event: React.ChangeEvent<HTMLTextAreaElement>) { onChange(event.target.value); }

  // Restore selection after React commits the controlled value, including backward block selections.
  useLayoutEffect(() => {
    const selection = pendingSelection.current;
    // Ordinary edits retain the browser's native caret behavior.
    if (selection && textarea.current) {
      textarea.current.setSelectionRange(selection.start, selection.end, selection.direction);
      pendingSelection.current = null;
    }
  }, [value]);

  /** Tab edits indentation; Escape then Tab provides a keyboard route out of the editor. */
  function handleKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    // Release the next Tab so keyboard-only users are never trapped in source editing.
    if (event.key === "Escape") {
      escapeTab.current = true;
      return;
    }
    const releaseTab = escapeTab.current;
    escapeTab.current = false;
    // Preserve platform shortcuts, composition, disabled state, and focus navigation after Escape.
    if (event.key !== "Tab" || releaseTab || event.ctrlKey || event.metaKey || event.altKey || event.nativeEvent.isComposing || disabled) return;
    event.preventDefault();
    const element = event.currentTarget;
    const edit = indentCode(value, element.selectionStart, element.selectionEnd, event.shiftKey);
    // Outdenting an already flush-left line should not invalidate a compilation plan.
    if (edit.value === value) return;
    pendingSelection.current = { start: edit.start, end: edit.end, direction: element.selectionDirection };
    onChange(edit.value);
  }

  return <div className="overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm focus-within:border-violet-300 focus-within:ring-2 focus-within:ring-violet-100">
    <div className="flex items-center justify-between border-b border-slate-200 bg-slate-50 px-4 py-2.5">
      <div className="flex items-center gap-2.5"><span aria-hidden="true" className="rounded bg-blue-600 px-1 py-0.5 text-[10px] font-bold leading-none text-white">TS</span><span className="font-mono text-xs font-medium text-slate-700">app.ts</span></div>
      <span className="text-xs font-normal text-slate-400">TypeScript</span>
    </div>
    <div className="fused-typescript-editor relative h-96 text-slate-800" style={textMetrics}>
      <div ref={highlight} aria-hidden="true" className="fused-code-highlight pointer-events-none absolute inset-0 overflow-hidden">{highlighted}</div>
      <div ref={gutter} aria-hidden="true" className="fused-code-highlight pointer-events-none absolute inset-y-0 left-0 z-10 w-10 overflow-hidden border-r border-slate-100 bg-slate-50 text-right text-slate-400"><pre className="m-0 py-4 pr-2" style={textMetrics}>{Array.from({ length: lineCount }, (_, index) => index + 1).join("\n")}</pre></div>
      <textarea ref={textarea} id={id} aria-label="TypeScript source" aria-describedby={`${id}-help`} onKeyDown={handleKeyDown} value={value} onChange={updateSource} onScroll={syncScroll} required={required} disabled={disabled} spellCheck={false} autoCapitalize="off" autoComplete="off" autoCorrect="off" wrap="off" placeholder="Generated TypeScript will appear here…" className="absolute inset-0 m-0 h-full w-full resize-none overflow-auto border-0 bg-transparent text-transparent caret-slate-900 outline-none placeholder:text-slate-400 selection:bg-violet-200/60 disabled:cursor-wait" style={{ ...textMetrics, padding: "16px 16px 16px 56px" }} />
    </div>
    <div className="flex justify-between gap-4 border-t border-slate-100 px-4 py-1.5 text-[10px] font-normal text-slate-400"><span id={`${id}-help`}>Tab to indent · Shift+Tab to outdent · Esc then Tab to leave</span><span aria-hidden="true">{lineCount} lines</span></div>
  </div>;
}
