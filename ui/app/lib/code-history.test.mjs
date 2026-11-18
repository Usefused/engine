import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import ts from "typescript";
import { act, createElement, useState } from "react";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { JSDOM } from "jsdom";
import * as codeHistory from "./code-history.ts";
import * as indentation from "./code-indentation.ts";
import * as differences from "./code-diff.ts";

// Rapid single-character typing merges into one undo step while pauses, newlines, and pastes stay separate.
test("code history coalesces rapid typing, separates discrete edits, and preserves bounded stacks", () => {
  let state = codeHistory.createCodeHistory("const a = 1;");
  state = codeHistory.recordCodeEdit(state, "const a = 12", { kind: "typing", now: 1000, beforeSelection: { start: 12, end: 12, direction: "none" }, afterSelection: { start: 12, end: 12, direction: "none" } });
  state = codeHistory.recordCodeEdit(state, "const a = 123", { kind: "typing", now: 1200, afterSelection: { start: 13, end: 13, direction: "none" } });
  assert.equal(state.past.length, 1);
  assert.equal(state.present.value, "const a = 123");
  // Pausing past the coalesce window starts a fresh undo snapshot.
  state = codeHistory.recordCodeEdit(state, "const a = 123;", { kind: "typing", now: 2500 });
  assert.equal(state.past.length, 2);
  // Undo steps backward and populates future; a new edit discards invalidated redo steps.
  const firstUndo = codeHistory.undoCodeHistory(state);
  assert.equal(firstUndo.changed, true);
  assert.equal(firstUndo.history.present.value, "const a = 123");
  assert.equal(firstUndo.history.future.length, 1);
  const secondUndo = codeHistory.undoCodeHistory(firstUndo.history);
  assert.equal(secondUndo.history.present.value, "const a = 1;");
  assert.deepEqual(secondUndo.history.present.selection, { start: 12, end: 12, direction: "none" });
  assert.equal(codeHistory.undoCodeHistory(secondUndo.history).changed, false);
  const firstRedo = codeHistory.redoCodeHistory(secondUndo.history);
  assert.equal(firstRedo.changed, true);
  assert.equal(firstRedo.history.present.value, "const a = 123");
  const branched = codeHistory.recordCodeEdit(firstRedo.history, "const a = 999;", { kind: "discrete", now: 3000 });
  assert.equal(branched.future.length, 0);
  // External updates record as discrete undoable steps and respect the history size cap.
  let capped = codeHistory.createCodeHistory("v0");
  for (let index = 1; index <= 5; index += 1) {
    capped = codeHistory.syncExternalCodeValue(capped, `v${index}`, 3);
  }
  assert.equal(capped.past.length, 3);
  assert.equal(capped.past[0].value, "v2");
  assert.equal(codeHistory.classifyCodeEditKind("ab", "abc"), "typing");
  assert.equal(codeHistory.classifyCodeEditKind("ab", "ab\n"), "discrete");
  assert.equal(codeHistory.classifyCodeEditKind("ab", "abcdef"), "discrete");
});

// Shortcut detection supports macOS and Windows/Linux conventions without stealing Alt chords.
test("codeHistoryShortcut resolves Cmd/Ctrl+Z, Shift+Z, and Y across platforms", () => {
  assert.equal(codeHistory.codeHistoryShortcut({ key: "z", metaKey: true, ctrlKey: false, shiftKey: false, altKey: false }), "undo");
  assert.equal(codeHistory.codeHistoryShortcut({ key: "Z", ctrlKey: true, metaKey: false, shiftKey: false, altKey: false }), "undo");
  assert.equal(codeHistory.codeHistoryShortcut({ key: "z", metaKey: true, ctrlKey: false, shiftKey: true, altKey: false }), "redo");
  assert.equal(codeHistory.codeHistoryShortcut({ key: "Z", ctrlKey: true, metaKey: false, shiftKey: true, altKey: false }), "redo");
  assert.equal(codeHistory.codeHistoryShortcut({ key: "y", ctrlKey: true, metaKey: false, shiftKey: false, altKey: false }), "redo");
  assert.equal(codeHistory.codeHistoryShortcut({ key: "y", ctrlKey: true, metaKey: false, shiftKey: true, altKey: false }), null);
  assert.equal(codeHistory.codeHistoryShortcut({ key: "z", ctrlKey: true, metaKey: false, shiftKey: false, altKey: true }), null);
  assert.equal(codeHistory.codeHistoryShortcut({ key: "z", ctrlKey: true, metaKey: true, shiftKey: false, altKey: false }), null);
});

// Real UnifiedAppCodeEditor + TypeScriptEditor verify toolbar buttons, shortcuts, Tab undo, and external sync undo.
test("UnifiedAppCodeEditor supports undo and redo via toolbar buttons and keyboard shortcuts", async () => {
  const dom = new JSDOM('<div id="root"></div>');
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const require = createRequire(import.meta.url);
  /** Stubs Prism rendering so the real textarea, toolbar, and change review run in JSDOM. */
  function Highlighter() { return null; }
  Highlighter.registerLanguage = () => {};
  const dependencies = {
    "~/lib/code-diff": differences,
    "~/lib/code-indentation": indentation,
    "~/lib/code-history": codeHistory,
    "react-syntax-highlighter/dist/esm/prism-light.js": { default: Highlighter },
  };
  /** Compiles the real TypeScriptEditor and UnifiedAppCodeEditor components for browser-like execution. */
  function loadComponent(path) {
    const source = readFileSync(new URL(path, import.meta.url), "utf8");
    const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
    const exports = {};
    /** Maps local aliases while stubbing only syntax language grammars. */
    function resolve(name) {
      if (name.includes("/languages/prism/")) return { default: {} };
      return dependencies[name] ?? require(name);
    }
    new Function("require", "exports", compiled)(resolve, exports);
    return exports;
  }
  dependencies["~/components/code/TypeScriptEditor"] = loadComponent("../components/code/TypeScriptEditor.tsx");
  const { UnifiedAppCodeEditor } = loadComponent("../components/apps/UnifiedAppCodeEditor.tsx");
  let latestSource = "export default buildUnifiedApp({});";
  let setExternalSource = null;
  let setEditorDisabled = null;

  /** Mirrors CreateUnifiedApp's controlled source state and external Config-tab updates. */
  function Harness() {
    const [source, setSource] = useState(latestSource);
    const [disabled, setDisabled] = useState(false);
    setExternalSource = (next) => { latestSource = next; setSource(next); };
    setEditorDisabled = setDisabled;
    return createElement(UnifiedAppCodeEditor, {
      source,
      disabled,
      onSource: (next) => { latestSource = next; setSource(next); },
    });
  }

  const root = createRoot(document.getElementById("root"));
  /** Reads the live textarea rendered inside UnifiedAppCodeEditor. */
  const textarea = () => document.getElementById("unified-app-source");
  /** Reads a toolbar history button by its accessible label. */
  const button = (label) => document.querySelector(`button[aria-label="${label}"]`);

  try {
    await act(async () => root.render(createElement(Harness)));
    assert.equal(button("Undo").disabled, true);
    assert.equal(button("Redo").disabled, true);

    // Tab indentation records a discrete undo step and restores caret position on Undo/Redo.
    textarea().setSelectionRange(0, 0);
    await act(async () => Simulate.keyDown(textarea(), { key: "Tab" }));
    assert.equal(latestSource, "  export default buildUnifiedApp({});");
    assert.equal(textarea().selectionStart, 2);
    assert.equal(button("Undo").disabled, false);

    // Multi-character source edit pushes a second discrete entry.
    await act(async () => Simulate.change(textarea(), { target: { value: "  export default buildUnifiedApp({ name: 'demo' });", selectionStart: 51, selectionEnd: 51, selectionDirection: "none" } }));
    assert.equal(latestSource, "  export default buildUnifiedApp({ name: 'demo' });");

    // Cmd+Z reverts the edit; clicking Undo reverts Tab indentation and restores the caret to 0.
    await act(async () => Simulate.keyDown(textarea(), { key: "z", metaKey: true }));
    assert.equal(latestSource, "  export default buildUnifiedApp({});");
    assert.equal(button("Redo").disabled, false);

    await act(async () => Simulate.click(button("Undo")));
    assert.equal(latestSource, "export default buildUnifiedApp({});");
    assert.equal(textarea().selectionStart, 0);
    assert.equal(button("Undo").disabled, true);

    // Cmd+Shift+Z and Ctrl+Y step forward through the redo stack.
    await act(async () => Simulate.keyDown(textarea(), { key: "z", metaKey: true, shiftKey: true }));
    assert.equal(latestSource, "  export default buildUnifiedApp({});");
    assert.equal(textarea().selectionStart, 2);

    await act(async () => Simulate.keyDown(textarea(), { key: "y", ctrlKey: true }));
    assert.equal(latestSource, "  export default buildUnifiedApp({ name: 'demo' });");
    assert.equal(button("Redo").disabled, true);

    // External updates (like Config tab projection) remain undoable and redoable from the Code editor.
    await act(async () => setExternalSource("export default buildUnifiedApp({ config: { version: '1.0.2' } });"));
    await act(async () => Simulate.click(button("Undo")));
    assert.equal(latestSource, "  export default buildUnifiedApp({ name: 'demo' });");
    await act(async () => Simulate.click(button("Redo")));
    assert.equal(latestSource, "export default buildUnifiedApp({ config: { version: '1.0.2' } });");

    // Disabled state blocks both toolbar buttons and keyboard shortcuts.
    await act(async () => setEditorDisabled(true));
    assert.equal(button("Undo").disabled, true);
    await act(async () => Simulate.keyDown(textarea(), { key: "z", ctrlKey: true }));
    assert.equal(latestSource, "export default buildUnifiedApp({ config: { version: '1.0.2' } });");
  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});
