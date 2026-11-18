import { useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type UIEvent, type KeyboardEvent, type RefObject } from "react";
import { Redo2, Undo2 } from "lucide-react";
import { diffCode, codeDiffContext } from "~/lib/code-diff";
import { classifyCodeEditKind, codeHistoryShortcut, createCodeHistory, recordCodeEdit, redoCodeHistory, syncExternalCodeValue, undoCodeHistory, type CodeEditKind, type CodeSelection } from "~/lib/code-history";
import { indentCode } from "~/lib/code-indentation";
import SyntaxHighlighter from "react-syntax-highlighter/dist/esm/prism-light.js";
import yaml from "react-syntax-highlighter/dist/esm/languages/prism/yaml.js";
import json from "react-syntax-highlighter/dist/esm/languages/prism/json.js";
import typescript from "react-syntax-highlighter/dist/esm/languages/prism/typescript.js";

// Register only the authoring languages used by Fused instead of the complete syntax bundle.
SyntaxHighlighter.registerLanguage("yaml", yaml);
SyntaxHighlighter.registerLanguage("typescript", typescript);
SyntaxHighlighter.registerLanguage("json", json);

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
  baseline?: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  required?: boolean;
  language?: "typescript" | "yaml" | "json";
  validationMessage?: string;
  describedBy?: string;
}

/** Detects an unmodified Tab/Shift+Tab press intended to adjust code indentation rather than move focus. */
function isTabIndentKey(event: KeyboardEvent<HTMLTextAreaElement>, releaseTab: boolean): boolean {
  // Preserve platform shortcuts and focus navigation immediately after Escape.
  return event.key === "Tab" && !releaseTab && !event.ctrlKey && !event.metaKey && !event.altKey;
}

/** Captures the active caret or selection range when the target exposes DOM selection indices. */
function readTextareaSelection(element: HTMLTextAreaElement): CodeSelection | null {
  // Synthetic change targets in tests may omit selection fields; fall back gracefully without dropping the edit.
  if (typeof element.selectionStart !== "number" || typeof element.selectionEnd !== "number") return null;
  return { start: element.selectionStart, end: element.selectionEnd, direction: element.selectionDirection ?? "none" };
}

/** Manages controlled undo/redo history so Tab indentation, typing, and external sync edits remain reversible. */
function useCodeHistory(
  value: string,
  disabled: boolean | undefined,
  onChange: (value: string) => void,
  textarea: RefObject<HTMLTextAreaElement | null>,
  pendingSelection: { current: CodeSelection | null },
) {
  const [history, setHistory] = useState(() => createCodeHistory(value));
  const historyRef = useRef(history);
  const lastSelection = useRef<CodeSelection | null>(null);

  // Capture external updates (such as Config tab sync or assistant revisions) as discrete undoable snapshots.
  useEffect(() => {
    if (value === historyRef.current.present.value) return;
    const next = syncExternalCodeValue(historyRef.current, value);
    historyRef.current = next;
    setHistory(next);
  }, [value]);

  /** Records a direct editor mutation onto the undo stack while tracking its before/after selection. */
  function record(nextValue: string, kind: CodeEditKind, beforeSelection: CodeSelection | null, afterSelection: CodeSelection | null) {
    const next = recordCodeEdit(historyRef.current, nextValue, { kind, now: Date.now(), beforeSelection, afterSelection });
    historyRef.current = next;
    lastSelection.current = afterSelection;
    setHistory(next);
  }

  /** Applies one undo or redo transition, restoring caret selection and optionally returning focus to the editor. */
  function apply(direction: "undo" | "redo", restoreFocus = false) {
    // Locked editors cannot mutate code through toolbar clicks or keyboard shortcuts.
    if (disabled) return;
    const result = direction === "undo" ? undoCodeHistory(historyRef.current) : redoCodeHistory(historyRef.current);
    // Avoid emitting redundant onChange calls when already at the end of the stack.
    if (!result.changed) return;
    historyRef.current = result.history;
    setHistory(result.history);
    // Restore the saved caret/selection range once React commits the controlled text.
    if (result.history.present.selection) {
      pendingSelection.current = result.history.present.selection;
      lastSelection.current = result.history.present.selection;
    }
    onChange(result.history.present.value);
    // Return focus to the textarea after toolbar clicks so typing can continue immediately.
    if (restoreFocus) textarea.current?.focus();
  }

  /** Tracks the latest textarea selection so typing edits capture the pre-edit caret for accurate undo. */
  function trackSelection(element: HTMLTextAreaElement) {
    lastSelection.current = readTextareaSelection(element);
  }

  return {
    canUndo: history.past.length > 0,
    canRedo: history.future.length > 0,
    currentValue: () => historyRef.current.present.value,
    lastSelection: () => lastSelection.current,
    record,
    apply,
    trackSelection,
  };
}

/** Renders compact Undo/Redo controls beside the change-review action in the editor header. */
function HistoryButtons({ canUndo, canRedo, disabled, onUndo, onRedo }: {
  canUndo: boolean; canRedo: boolean; disabled?: boolean; onUndo: () => void; onRedo: () => void;
}) {
  const buttonStyle = "inline-flex items-center justify-center rounded-md p-1 text-slate-600 hover:bg-slate-200 focus-visible:outline-slate-950 disabled:cursor-not-allowed disabled:opacity-35 disabled:hover:bg-transparent";
  return <div className="flex items-center gap-0.5" role="group" aria-label="Edit history">
    <button type="button" aria-label="Undo" title="Undo (⌘Z / Ctrl+Z)" disabled={Boolean(disabled) || !canUndo} onClick={onUndo} className={buttonStyle}><Undo2 className="h-3.5 w-3.5" aria-hidden="true" /></button>
    <button type="button" aria-label="Redo" title="Redo (⇧⌘Z / Ctrl+Y)" disabled={Boolean(disabled) || !canRedo} onClick={onRedo} className={buttonStyle}><Redo2 className="h-3.5 w-3.5" aria-hidden="true" /></button>
  </div>;
}

/** Shares native editing and line-level change review without mixing removed text into the authored value. */
export function TypeScriptEditor({ id, value, baseline, onChange, disabled, required, language = "typescript", validationMessage = "", describedBy }: TypeScriptEditorProps) {
  // Language changes presentation only; the parent owns the corresponding source or configuration text.
  const yamlMode = language === "yaml";
  // JSON shares editing ergonomics while keeping payload labels distinct from app source.
  const jsonMode = language === "json";
  const label = jsonMode ? "Input JSON" : yamlMode ? "YAML configuration" : "TypeScript source";
  const textarea = useRef<HTMLTextAreaElement>(null);
  const pendingSelection = useRef<CodeSelection | null>(null);
  const escapeTab = useRef(false);
  const highlight = useRef<HTMLDivElement>(null);
  const gutter = useRef<HTMLDivElement>(null);
  const [review, setReview] = useState(false);
  const history = useCodeHistory(value, disabled, onChange, textarea, pendingSelection);
  // Editors without a baseline retain their existing presentation and avoid comparison work.
  const changes = useMemo(() => baseline === undefined ? [] : diffCode(baseline, value), [baseline, value]);
  const addedLines = useMemo(() => new Set(changes.filter(line => line.kind === "added").map(line => line.newLine)), [changes]);
  const removedCount = changes.filter(line => line.kind === "removed").length;
  const reviewRows = useMemo(() => codeDiffContext(changes), [changes]);
  const hasChanges = addedLines.size > 0 || removedCount > 0;
  // A completed undo restores the normal editable surface automatically.
  const reviewing = review && hasChanges;
  const lineCount = value.split("\n").length;
  // The highlighter needs line numbering enabled to supply line indices; our synchronized gutter renders the numbers.
  // A trailing space preserves the final empty line without changing the authored source.
  const highlighted = useMemo(() => <SyntaxHighlighter showLineNumbers lineNumberStyle={{ display: "none" }} wrapLines lineProps={(lineNumber) => ({ style: { display: "block", backgroundColor: addedLines.has(lineNumber) ? "#dcfce7" : undefined } })} language={language} style={tokenStyles} customStyle={{ ...textMetrics, margin: 0, padding: "16px 16px 16px 56px", border: 0, background: "transparent", overflow: "visible", minHeight: "100%", minWidth: "100%", width: "max-content" }} codeTagProps={{ style: textMetrics }}>{`${value} `}</SyntaxHighlighter>, [value, language, addedLines]);

  // Native validity prevents the outer form from submitting malformed or schema-invalid input.
  useLayoutEffect(() => { textarea.current?.setCustomValidity(validationMessage); }, [validationMessage]);

  /** Both decorative layers follow the textarea's native scroll so caret and tokens stay aligned. */
  function syncScroll(event: UIEvent<HTMLTextAreaElement>) {
    const { scrollTop, scrollLeft } = event.currentTarget;
    // Refs can be absent while the editor is mounting or leaving the page.
    if (highlight.current) { highlight.current.scrollTop = scrollTop; highlight.current.scrollLeft = scrollLeft; }
    if (gutter.current) gutter.current.scrollTop = scrollTop;
  }

  /** Passes every edit through undo history and the parent source state so previous compilation plans are invalidated. */
  function updateSource(event: React.ChangeEvent<HTMLTextAreaElement>) {
    const element = event.target;
    const nextValue = element.value;
    const afterSelection = readTextareaSelection(element);
    const kind = classifyCodeEditKind(history.currentValue(), nextValue);
    history.record(nextValue, kind, history.lastSelection(), afterSelection);
    onChange(nextValue);
  }

  // Restore selection after React commits the controlled value, including backward block selections.
  useLayoutEffect(() => {
    const selection = pendingSelection.current;
    // Ordinary edits retain the browser's native caret behavior.
    if (selection && textarea.current) {
      textarea.current.setSelectionRange(selection.start, selection.end, selection.direction);
      pendingSelection.current = null;
    }
  }, [value]);

  /** Handles undo/redo and Tab indentation while Escape then Tab provides a keyboard route out of the editor. */
  function handleKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    // Release the next Tab so keyboard-only users are never trapped in source editing.
    if (event.key === "Escape") {
      escapeTab.current = true;
      return;
    }
    const releaseTab = escapeTab.current;
    escapeTab.current = false;
    // IME composition and disabled editors must never run custom keyboard mutations.
    if (event.nativeEvent.isComposing || disabled) return;
    const shortcut = codeHistoryShortcut(event);
    // Always intercept undo/redo shortcuts so stale browser-native textarea history cannot conflict with our stack.
    if (shortcut) {
      event.preventDefault();
      history.apply(shortcut);
      return;
    }
    // Preserve platform shortcuts and focus navigation after Escape.
    if (!isTabIndentKey(event, releaseTab)) return;
    event.preventDefault();
    const element = event.currentTarget;
    const beforeSelection = readTextareaSelection(element);
    const edit = indentCode(value, element.selectionStart, element.selectionEnd, event.shiftKey);
    // Outdenting an already flush-left line should not invalidate a compilation plan.
    if (edit.value === value) return;
    const afterSelection: CodeSelection = { start: edit.start, end: edit.end, direction: element.selectionDirection };
    pendingSelection.current = afterSelection;
    history.record(edit.value, "discrete", beforeSelection, afterSelection);
    onChange(edit.value);
  }

  return <div className="overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm focus-within:border-violet-300 focus-within:ring-2 focus-within:ring-violet-100">
    <div className="flex items-center justify-between border-b border-slate-200 bg-slate-50 px-4 py-2.5">
      <div className="flex items-center gap-2.5"><span aria-hidden="true" className="rounded bg-blue-600 px-1 py-0.5 text-[10px] font-bold leading-none text-white">{jsonMode ? "{}" : yamlMode ? "YML" : "TS"}</span><span className="font-mono text-xs font-medium text-slate-700">{jsonMode ? "input.json" : yamlMode ? "app.yaml" : "app.ts"}</span></div>
      <div className="flex items-center gap-2">
        <HistoryButtons canUndo={history.canUndo} canRedo={history.canRedo} disabled={disabled} onUndo={/** Reverts the last source edit and returns focus when editing. */ () => history.apply("undo", !reviewing)} onRedo={/** Reapplies the next undone source edit and returns focus when editing. */ () => history.apply("redo", !reviewing)} />
        {/* Counts and signs communicate changes without relying on red/green alone. */}
        {hasChanges ? <button type="button" aria-pressed={reviewing} onClick={() => setReview(!reviewing)} className="flex items-center gap-2 rounded-md px-2 py-1 text-xs font-medium text-slate-600 hover:bg-slate-200 focus-visible:outline-slate-950"><span>{reviewing ? "Back to editing" : "Review changes"}</span><span className="text-green-700">+{addedLines.size}</span><span className="text-red-700">−{removedCount}</span></button> : <span className="text-xs font-normal text-slate-400">{jsonMode ? "JSON" : yamlMode ? "YAML" : "TypeScript"}</span>}
      </div>
    </div>
    {/* Removed lines are read-only review evidence, never textarea content. */}
    {reviewing && <div role="region" aria-label={`${label} changes`} tabIndex={0} className="h-96 overflow-auto text-slate-800" style={textMetrics}>
      <div className="min-w-full w-max py-3">
        {reviewRows.map((line, index) => line.kind === "gap" ? <div key={index} className="bg-slate-50 px-4 py-1 text-xs text-slate-400">{line.count} unchanged lines</div> : <div key={index} className={`flex min-w-full ${line.kind === "added" ? "bg-green-100" : line.kind === "removed" ? "bg-red-100" : ""}`}>
          <span aria-hidden="true" className="sticky left-0 flex shrink-0 bg-inherit text-right text-slate-500"><span className="w-10 pr-2">{line.oldLine}</span><span className="w-10 pr-2">{line.newLine}</span><span className={`w-6 text-center font-semibold ${line.kind === "added" ? "text-green-700" : "text-red-700"}`}>{line.kind === "added" ? "+" : line.kind === "removed" ? "−" : " "}</span></span>
          <span className="sr-only">{line.kind}: </span><code className="pr-4">{line.text || " "}</code>
        </div>)}
      </div>
    </div>}
    <div hidden={reviewing} className={`fused-typescript-editor relative ${jsonMode ? "h-64" : "h-96"} text-slate-800`} style={textMetrics}>
      <div ref={highlight} aria-hidden="true" className="fused-code-highlight pointer-events-none absolute inset-0 overflow-hidden">{highlighted}</div>
      <div ref={gutter} aria-hidden="true" className="fused-code-highlight pointer-events-none absolute inset-y-0 left-0 z-10 w-10 overflow-hidden border-r border-slate-100 bg-slate-50 text-right text-slate-400"><pre className="m-0 py-4 pr-2" style={textMetrics}>{Array.from({ length: lineCount }, (_, index) => index + 1).join("\n")}</pre></div>
      <textarea ref={textarea} id={id} aria-label={label} aria-describedby={`${id}-help ${describedBy ?? ""}`} aria-invalid={Boolean(validationMessage)} onKeyDown={handleKeyDown} onSelect={/** Tracks caret movement so subsequent edits save accurate pre-edit undo selection. */ (event) => history.trackSelection(event.currentTarget)} value={value} onChange={updateSource} onScroll={syncScroll} required={required} disabled={disabled} spellCheck={false} autoCapitalize="off" autoComplete="off" autoCorrect="off" wrap="off" placeholder={jsonMode ? "Enter the app input as JSON…" : yamlMode ? "App configuration…" : "Generated TypeScript will appear here…"} className="absolute inset-0 m-0 h-full w-full resize-none overflow-auto border-0 bg-transparent text-transparent caret-slate-900 outline-none placeholder:text-slate-400 selection:bg-violet-200/60 disabled:cursor-wait" style={{ ...textMetrics, padding: "16px 16px 16px 56px" }} />
    </div>
    <div className="flex justify-between gap-4 border-t border-slate-100 px-4 py-1.5 text-[10px] font-normal text-slate-400"><span id={`${id}-help`}>{reviewing ? "Changes since this editor was opened · removed lines are not included in your app" : "Tab to indent · Shift+Tab to outdent · ⌘Z / Ctrl+Z to undo · Esc then Tab to leave"}</span><span aria-hidden="true">{lineCount} lines</span></div>
  </div>;
}
