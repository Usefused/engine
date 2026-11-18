export interface CodeSelection {
  start: number;
  end: number;
  direction: "forward" | "backward" | "none";
}

export interface CodeSnapshot {
  value: string;
  selection: CodeSelection | null;
}

export type CodeEditKind = "typing" | "discrete";

export interface CodeHistoryState {
  past: CodeSnapshot[];
  present: CodeSnapshot;
  future: CodeSnapshot[];
  lastEditAt: number;
  lastEditKind: CodeEditKind | null;
}

export interface RecordCodeEditOptions {
  kind: CodeEditKind;
  now: number;
  beforeSelection?: CodeSelection | null;
  afterSelection?: CodeSelection | null;
  coalesceMs?: number;
  maxEntries?: number;
}

const DEFAULT_COALESCE_MS = 600;
const DEFAULT_MAX_ENTRIES = 100;

/** Initializes bounded undo/redo history anchored on the editor's current source text. */
export function createCodeHistory(value: string): CodeHistoryState {
  return {
    past: [],
    present: { value, selection: null },
    future: [],
    lastEditAt: 0,
    lastEditKind: null,
  };
}

/** Classifies single-character same-line edits as typing and multi-character or newline edits as discrete steps. */
export function classifyCodeEditKind(previous: string, next: string): CodeEditKind {
  // Multi-character replacements, pastes, and block deletions should always stand as their own undo step.
  if (Math.abs(next.length - previous.length) !== 1) return "discrete";
  // Pressing Enter or joining lines starts a new undo boundary even when only one character changed.
  return previous.split("\n").length === next.split("\n").length ? "typing" : "discrete";
}

/** Determines whether rapid sequential typing should merge into the current undo step. */
function shouldCoalesceCodeEdit(
  history: CodeHistoryState,
  kind: CodeEditKind,
  now: number,
  coalesceMs: number,
): boolean {
  // Discrete edits and the first edit after an undo/redo must never merge into an earlier snapshot.
  if (kind !== "typing" || history.lastEditKind !== "typing" || history.past.length === 0) return false;
  return now - history.lastEditAt <= coalesceMs;
}

/** Records a source mutation onto the undo stack, coalescing rapid typing and clearing invalidated redo steps. */
export function recordCodeEdit(
  history: CodeHistoryState,
  nextValue: string,
  options: RecordCodeEditOptions,
): CodeHistoryState {
  // Identical text produces no history entry and must preserve any existing redo stack.
  if (nextValue === history.present.value) return history;
  const coalesceMs = options.coalesceMs ?? DEFAULT_COALESCE_MS;
  const maxEntries = options.maxEntries ?? DEFAULT_MAX_ENTRIES;
  const nextPresent: CodeSnapshot = { value: nextValue, selection: options.afterSelection ?? null };
  // Rapid sequential typing updates the active snapshot in place so one Undo reverts the burst.
  if (shouldCoalesceCodeEdit(history, options.kind, options.now, coalesceMs)) {
    return { ...history, present: nextPresent, future: [], lastEditAt: options.now, lastEditKind: options.kind };
  }
  const previousSnapshot: CodeSnapshot = {
    value: history.present.value,
    selection: options.beforeSelection ?? history.present.selection,
  };
  const past = [...history.past, previousSnapshot].slice(-maxEntries);
  return { past, present: nextPresent, future: [], lastEditAt: options.now, lastEditKind: options.kind };
}

/** Reverts to the previous snapshot while preserving the current state on the redo stack. */
export function undoCodeHistory(
  history: CodeHistoryState,
  maxEntries = DEFAULT_MAX_ENTRIES,
): { history: CodeHistoryState; changed: boolean } {
  // An empty past stack cannot undo further.
  if (history.past.length === 0) return { history, changed: false };
  const previous = history.past[history.past.length - 1];
  const past = history.past.slice(0, -1);
  const future = [history.present, ...history.future].slice(0, maxEntries);
  return {
    history: { past, present: previous, future, lastEditAt: 0, lastEditKind: null },
    changed: true,
  };
}

/** Reapplies the next undone snapshot while moving the current state back onto the undo stack. */
export function redoCodeHistory(
  history: CodeHistoryState,
  maxEntries = DEFAULT_MAX_ENTRIES,
): { history: CodeHistoryState; changed: boolean } {
  // An empty future stack has no undone edit to reapply.
  if (history.future.length === 0) return { history, changed: false };
  const [next, ...rest] = history.future;
  const past = [...history.past, history.present].slice(-maxEntries);
  return {
    history: { past, present: next, future: rest, lastEditAt: 0, lastEditKind: null },
    changed: true,
  };
}

/** Captures external source updates (such as Config tab sync or assistant revisions) as undoable discrete steps. */
export function syncExternalCodeValue(
  history: CodeHistoryState,
  value: string,
  maxEntries = DEFAULT_MAX_ENTRIES,
): CodeHistoryState {
  // Ignore controlled re-renders whose text already matches the active history snapshot.
  if (value === history.present.value) return history;
  return recordCodeEdit(history, value, { kind: "discrete", now: 0, afterSelection: null, maxEntries });
}

/** Maps platform undo/redo keystrokes (Cmd/Ctrl+Z, Cmd/Ctrl+Shift+Z, Cmd/Ctrl+Y) without hijacking Alt chords. */
export function codeHistoryShortcut(event: {
  key: string;
  metaKey: boolean;
  ctrlKey: boolean;
  shiftKey: boolean;
  altKey: boolean;
}): "undo" | "redo" | null {
  const hasPrimaryModifier = event.metaKey !== event.ctrlKey;
  // Require exactly one primary modifier and no Alt key so OS and AltGr chords remain untouched.
  if (!hasPrimaryModifier || event.altKey) return null;
  const key = event.key.toLowerCase();
  // Shift reverses Z into redo across both macOS and Windows/Linux.
  if (key === "z") return event.shiftKey ? "redo" : "undo";
  // Ctrl+Y / Cmd+Y triggers redo when Shift is not held.
  if (key === "y" && !event.shiftKey) return "redo";
  return null;
}
