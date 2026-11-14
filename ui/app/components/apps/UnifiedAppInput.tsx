import { useId, useState } from "react";
import { Plus, X } from "lucide-react";
import { Select } from "~/components/forms/Select";
import { unifiedInputFields, type InputField, type InputValueType } from "~/lib/unified-app-input-fields";

type ValueType = InputValueType;
type InputProps = { schema?: unknown; value: string; onChange: (value: string) => void; disabled: boolean; onEditingChange: (editing: boolean) => void };
type Draft = { original: string | null; name: string; type: ValueType; text: string };
const types: ValueType[] = ["string", "number", "boolean", "null", "object", "array"];
const control = "w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm outline-none focus:border-violet-400 focus:ring-2 focus:ring-violet-100 disabled:opacity-50";

/** Identifies JSON types explicitly so strings such as "false" never become booleans while editing. */
function valueType(value: unknown): ValueType {
  // Null and arrays require distinct editors even though JavaScript calls them objects.
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  return typeof value as ValueType;
}

/** Rejects numeric overflow before chip edits could silently serialize it as null. */
function parseJSON(text: string): unknown {
  return JSON.parse(text, (_key, value) => {
    // JSON numbers must survive serialization without losing their value.
    if (typeof value === "number" && !Number.isFinite(value)) throw new Error("Use a finite number.");
    return value;
  });
}

/** Treats chips as a lossless object editor and retains raw JSON for every other document shape. */
function inputObject(text: string): Record<string, unknown> | null {
  try {
    const value = parseJSON(text);
    // Arrays, scalars, and malformed drafts remain editable in the JSON view.
    return value !== null && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : null;
  } catch { return null; /* Parsing must never overwrite an unfinished JSON draft. */ }
}

/** Owns lossless input drafts independently of their scalar, structured, or raw JSON presentation. */
function useInputEditor({ value, onChange, onEditingChange, schema }: InputProps) {
  const id = useId();
  const object = inputObject(value);
  const fields = unifiedInputFields(schema);
  const [mode, setMode] = useState<"fields" | "json">(object && (schema === undefined || fields !== null) ? "fields" : "json");
  const [draft, setDraft] = useState<Draft | null>(null);
  const [error, setError] = useState("");
  const [inlineText, setInlineText] = useState<Record<string, string>>({});
  const [invalidFields, setInvalidFields] = useState<Set<string>>(new Set());
  // Schema-less callers retain manual chips; unsupported schemas use JSON without guessing names or types.
  const hasSchema = schema !== undefined;
  const canUseFields = Boolean(object) && (!hasSchema || fields !== null);

  /** Keeps unsaved chip edits visible and prevents running an older committed payload by mistake. */
  function edit(next: Draft | null) {
    setDraft(next); setError(""); onEditingChange(next !== null || invalidFields.size > 0);
  }

  /** Applies valid keystrokes immediately while keeping incomplete numbers and JSON visible and non-executable. */
  function changeInline(name: string, type: ValueType, text: string) {
    // Only object documents can be edited by named chips; JSON mode owns all other shapes.
    if (!object) return;
    setInlineText({ ...inlineText, [name]: text });
    const invalid = new Set(invalidFields);
    try {
      // Clearing non-text input omits the value; required-field validation still blocks the request.
      const absent = text === "" && type !== "string";
      const next = absent ? undefined : parseFieldValue(type, text);
      const entries = Object.entries(object).filter(([key]) => key !== name);
      // Special property names remain ordinary JSON data rather than prototype mutations.
      onChange(JSON.stringify(Object.fromEntries(absent ? entries : [...entries, [name, next]]), null, 2));
      invalid.delete(name);
    } catch { invalid.add(name); /* Preserve the last valid payload until the incomplete value can be represented in JSON. */ }
    setInvalidFields(invalid); onEditingChange(invalid.size > 0 || draft !== null);
  }

  /** Escape restores the last valid value instead of silently abandoning other unfinished fields. */
  function resetInline(name: string) {
    setInlineText(Object.fromEntries(Object.entries(inlineText).filter(([key]) => key !== name)));
    const invalid = new Set(invalidFields); invalid.delete(name);
    setInvalidFields(invalid); onEditingChange(invalid.size > 0 || draft !== null);
  }

  /** Commits one typed value without flattening nested values or replacing another property. */
  function save() {
    // A concurrent invalid document cannot be repaired implicitly by committing a chip.
    if (!draft || !object) return;
    try {
      // Whitespace within an existing key is meaningful; only an empty key is rejected here.
      const nameError = draftNameError(draft, object);
      if (nameError) throw new Error(nameError);
      const next = parseFieldValue(draft.type, draft.text);
      const entries = Object.entries(object).filter(([name]) => name !== draft.original);
      // Object.fromEntries treats special keys as data instead of invoking prototype setters.
      onChange(JSON.stringify(Object.fromEntries([...entries, [draft.name, next]]), null, 2));
      edit(null);
    } catch {
      // Validation never echoes potentially sensitive field contents into an error message.
      setError(draftNameError(draft, object) ?? `Enter a valid ${draft.type} value. Use JSON syntax for numbers, booleans, lists, and objects.`);
    }
  }

  /** Clears a property and its local draft without altering other values or their validation state. */
  function remove(name: string) {
    // The JSON editor owns invalid documents; chip actions cannot discard those drafts.
    if (!object) return;
    resetInline(name);
    onChange(JSON.stringify(Object.fromEntries(Object.entries(object).filter(([key]) => key !== name)), null, 2));
  }

  return { id, object, fields, mode, setMode, draft, setDraft, error, inlineText, setInlineText, invalidFields, hasSchema, canUseFields, edit, changeInline, resetInline, save, remove };
}

type InputEditor = ReturnType<typeof useInputEditor>;

/** Rejects mismatched JSON shapes while keeping literal strings untouched. */
function parseFieldValue(type: ValueType, text: string): unknown {
  // A text field containing "false" must remain a string.
  if (type === "string") return text;
  const value = parseJSON(text);
  // Ambiguous contracts accept any JSON; concrete fields require their declared type.
  if (type !== "json" && valueType(value) !== type) throw new Error("Wrong JSON type");
  return value;
}

/** Validates field identity without including private values in user-facing errors. */
function draftNameError(draft: Draft, object: Record<string, unknown>): string | null {
  // Empty keys are authoring errors; existing keys may contain meaningful whitespace.
  if (!draft.name) return "Enter a field name.";
  // Renaming must not overwrite another field.
  if (draft.name !== draft.original && Object.hasOwn(object, draft.name)) return "That field already exists.";
  return null;
}

/** Shows declared types and gives structured values room to edit while retaining incomplete drafts. */
export function UnifiedAppInput(props: InputProps) {
  const editor = useInputEditor(props);
  return <section aria-label="App input" className="min-w-0">
    <div className="mb-2 flex items-center justify-between gap-3"><h3 className="text-sm font-medium text-slate-800">Input</h3><EditorMode editor={editor} disabled={props.disabled} /></div>
    {/* Raw JSON remains available for unsupported schemas and non-object documents. */}
    {editor.mode === "json" ? <textarea aria-label="Input JSON" spellCheck={false} className={`${control} min-h-28 font-mono text-xs leading-6`} disabled={props.disabled} value={props.value} onChange={/** Preserves raw syntax until the user corrects it. */ (event) => props.onChange(event.target.value)} /> : <InputFields editor={editor} disabled={props.disabled} />}
    <p className="mt-2 text-xs text-slate-400">{editor.hasSchema ? "Changes apply as you type. Objects and arrays use JSON." : "Click a field to edit its value. Use JSON for the full input."}</p>
  </section>;
}

/** Switches views without discarding incomplete input or coercing its JSON type. */
function EditorMode({ editor, disabled }: { editor: InputEditor; disabled: boolean }) {
  const blocked = disabled || Boolean(editor.draft) || editor.invalidFields.size > 0;
  return <div className="flex rounded-lg bg-slate-100 p-0.5" aria-label="Input editor">{(["fields", "json"] as const).map(/** JSON stays accessible even when a schema cannot be represented as fields. */ (view) => <button key={view} type="button" disabled={blocked || (view === "fields" && !editor.canUseFields)} aria-pressed={editor.mode === view} onClick={/** Clears only formatting caches after all drafts have been resolved. */ () => { editor.setInlineText({}); editor.setMode(view); }} className={`rounded-md px-2.5 py-1 text-xs font-medium transition disabled:opacity-40 ${editor.mode === view ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-800"}`}>{view === "fields" ? "Fields" : "JSON"}</button>)}</div>;
}

/** Keeps the schema form, manual editor, and validation messages on the same lossless payload. */
function InputFields({ editor, disabled }: { editor: InputEditor; disabled: boolean }) {
  const { fields, object, id, invalidFields } = editor;
  // Extra properties remain in the request and are disclosed rather than silently hidden or deleted.
  const extras = fields && Object.keys(object ?? {}).some(/** Tests authored names without reading private values. */ name => !fields.some(/** Matches the exact schema property name. */ field => field.name === name));
  return <div className="space-y-3">
    {/* Only declared contracts render fixed fields; schema-less input keeps manual key editing. */}
    {fields ? fields.map(/** Retains stable field identity while typing. */ item => <SchemaField key={item.name} item={item} editor={editor} disabled={disabled} />) : <ManualFields editor={editor} disabled={disabled} />}
    {fields?.length === 0 && <p className="text-xs text-slate-400">This app has no declared input fields.</p>}
    {invalidFields.size > 0 && <p id={`${id}-invalid`} role="alert" className="text-xs text-red-700">Finish or clear the highlighted values before running. Use valid numbers or JSON for structured values.</p>}
    {extras && <p className="text-xs text-amber-700">Additional fields are present. Use JSON to review them.</p>}
  </div>;
}

/** Uses multiline JSON only where the authored type needs structural syntax. */
function isStructured(type: ValueType): boolean { return type === "object" || type === "array" || type === "json"; }

/** Prefers unfinished text over committed JSON so invalid edits remain visible. */
function schemaFieldText(editor: InputEditor, item: InputField): string {
  // Local drafts preserve whitespace and incomplete tokens between keystrokes.
  if (Object.hasOwn(editor.inlineText, item.name)) return editor.inlineText[item.name];
  // Absent fields have no fabricated default, even when their type is an object.
  if (!editor.object || !Object.hasOwn(editor.object, item.name)) return "";
  const value = editor.object[item.name];
  if (typeof value === "string" && item.type === "string") return value;
  return JSON.stringify(value, null, isStructured(item.type) ? 2 : undefined);
}

/** Presents native choices only for declared enums or types with a finite set of values. */
function fieldChoices(item: InputField): unknown[] | undefined {
  // Authored enum values take precedence over generic boolean and null options.
  if (item.choices) return item.choices;
  if (item.type === "boolean") return [true, false];
  if (item.type === "null") return [null];
  return undefined;
}

/** Labels type and requiredness independently, with a clear action that never creates replacement data. */
function SchemaField({ item, editor, disabled }: { item: InputField; editor: InputEditor; disabled: boolean }) {
  const invalid = editor.invalidFields.has(item.name);
  const present = Object.hasOwn(editor.object ?? {}, item.name) || Object.hasOwn(editor.inlineText, item.name);
  const choices = fieldChoices(item);
  const fieldID = `${editor.id}-${item.name}`;
  return <div className={`min-w-0 rounded-lg border bg-white p-3 transition-colors focus-within:ring-2 ${invalid ? "border-red-300 focus-within:ring-red-100" : "border-slate-200 focus-within:border-violet-300 focus-within:ring-violet-50"}`}>
    <div className="flex min-h-6 items-center gap-2">
      <label htmlFor={fieldID} className="min-w-0 break-all text-xs font-semibold text-slate-800">{item.name}</label>
      <span id={`${fieldID}-type`} className="shrink-0 rounded bg-slate-100 px-1.5 py-0.5 font-mono text-[10px] text-slate-500">{item.type}</span>
      <span className="ml-auto text-[10px] text-slate-400">{item.required ? "Required" : "Optional"}</span>
      {/* Clearing also discards invalid local syntax without inventing an empty object or array. */}
      {present && <button type="button" disabled={disabled} aria-label={`Clear ${item.name}`} onClick={/** Removes only the selected field. */ () => editor.remove(item.name)} className="rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-700 disabled:opacity-50"><X className="h-3.5 w-3.5" aria-hidden="true" /></button>}
    </div>
    {/* Authored descriptions remain readable without requiring a hover gesture. */}
    {item.description && <p id={`${fieldID}-description`} className="mt-1 text-xs leading-5 text-slate-500">{item.description}</p>}
    {choices ? <FieldChoice item={item} editor={editor} disabled={disabled} choices={choices} /> : <FieldText item={item} editor={editor} disabled={disabled} />}
  </div>;
}

/** Uses option indices to distinguish omission from false, null, zero, and empty strings. */
function FieldChoice({ item, editor, disabled, choices }: { item: InputField; editor: InputEditor; disabled: boolean; choices: unknown[] }) {
  const present = Object.hasOwn(editor.object ?? {}, item.name);
  const selected = present ? choices.findIndex(/** Compares complete JSON values rather than display labels. */ choice => JSON.stringify(choice) === JSON.stringify(editor.object?.[item.name])) : -1;
  return <Select density="compact" id={`${editor.id}-${item.name}`} aria-label={item.name} aria-required={item.required} aria-describedby={`${editor.id}-${item.name}-type`} disabled={disabled} className="mt-2 w-full min-w-0 text-xs" value={String(selected)} onChange={/** Applies exactly the selected value without fabricating a default. */ (event) => {
    // The placeholder means omission, including for required fields that the full validator will reject.
    if (event.target.value === "-1") { editor.remove(item.name); return; }
    const choice = choices[Number(event.target.value)];
    editor.changeInline(item.name, item.type, typeof choice === "string" && item.type === "string" ? choice : JSON.stringify(choice));
  }}><option value="-1">Choose a value…</option>{choices.map(/** Index keys preserve options whose JSON types have similar labels. */ (choice, index) => <option key={index} value={index}>{JSON.stringify(choice)}</option>)}</Select>;
}

/** Supplies type-specific guidance without turning the placeholder into a request value. */
function fieldPlaceholder(type: ValueType): string {
  // Structured values require explicit JSON rather than automatic string coercion.
  if (type === "object" || type === "array") return `Enter a JSON ${type}…`;
  if (type === "json") return "Enter JSON…";
  if (type === "number") return "Enter a number…";
  return "Enter text…";
}

/** Gives structured values a full-width JSON editor while preserving scalar keyboard behavior. */
function FieldText({ item, editor, disabled }: { item: InputField; editor: InputEditor; disabled: boolean }) {
  const structured = isStructured(item.type);
  const invalid = editor.invalidFields.has(item.name);
  const fieldID = `${editor.id}-${item.name}`;
  const describedBy = [`${fieldID}-type`, item.description && `${fieldID}-description`, invalid && `${editor.id}-invalid`].filter(Boolean).join(" ");
  return <textarea id={fieldID} aria-label={item.name} aria-required={item.required} aria-invalid={invalid} aria-describedby={describedBy} rows={structured ? 4 : 1} disabled={disabled} spellCheck={false} value={schemaFieldText(editor, item)} placeholder={fieldPlaceholder(item.type)} className={`mt-2 block w-full min-w-0 rounded-md bg-slate-50 px-2.5 py-2 font-mono text-xs leading-6 text-slate-800 outline-none placeholder:font-sans placeholder:text-slate-400 disabled:opacity-50 ${structured ? "min-h-28 resize-y" : "resize-none"}`} onChange={/** Keeps incomplete JSON local until it can safely update the request. */ event => editor.changeInline(item.name, item.type, event.target.value)} onKeyDown={/** Allows JSON newlines while retaining Escape recovery. */ event => {
    // Escape restores the last valid value for this field only.
    if (event.key === "Escape") { event.preventDefault(); editor.resetInline(item.name); }
    // Plain Enter finishes scalar editing; structured values need newlines for formatting.
    if (event.key === "Enter" && !event.shiftKey && !structured) { event.preventDefault(); event.currentTarget.blur(); }
  }} />;
}

/** Retains explicit key authoring for callers without a schema. */
function ManualFields({ editor, disabled }: { editor: InputEditor; disabled: boolean }) {
  return <div className="rounded-xl border border-slate-200 bg-white p-3">
    <div className="flex min-h-9 flex-wrap items-center gap-2">{Object.entries(editor.object ?? {}).map(/** Keeps manual keys editable without losing nested JSON. */ ([name, item]) => <span key={name} className="inline-flex max-w-full items-center rounded-lg border border-slate-200 bg-slate-50 text-xs text-slate-800">
      <button type="button" disabled={disabled || Boolean(editor.draft)} aria-label={`Edit ${name}`} title={`${name}: ${JSON.stringify(item)}`} onClick={/** Starts an explicit typed draft rather than changing the committed payload. */ () => editor.edit({ original: name, name, type: valueType(item), text: typeof item === "string" ? item : JSON.stringify(item, null, 2) })} className="flex min-w-0 items-center gap-1.5 rounded-l-lg px-2.5 py-2 text-left hover:bg-slate-100 disabled:opacity-50"><span className="max-w-40 truncate font-semibold">{name}</span><span className="font-mono text-[10px] text-slate-400">{valueType(item)}</span><span className="max-w-56 truncate font-mono">{JSON.stringify(item)}</span></button>
      <button type="button" disabled={disabled || Boolean(editor.draft)} aria-label={`Remove ${name}`} onClick={/** Removes only the named property. */ () => editor.remove(name)} className="mr-1 rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-700 disabled:opacity-50"><X className="h-3 w-3" aria-hidden="true" /></button>
    </span>)}
      {/* New keys belong only to schema-less documents and cannot interrupt a pending draft. */}
      {!editor.hasSchema && !editor.draft && <button type="button" disabled={disabled} onClick={/** Starts an empty draft without adding a request property. */ () => editor.edit({ original: null, name: "", type: "string", text: "" })} className="inline-flex items-center gap-1 rounded-lg px-2 py-2 text-xs font-medium text-slate-500 hover:bg-slate-50 hover:text-violet-700 disabled:opacity-50"><Plus className="h-3.5 w-3.5" aria-hidden="true" />Add field</button>}
    </div>
    {editor.draft && <DraftFields editor={editor} draft={editor.draft} disabled={disabled} />}
  </div>;
}

/** Commits manual fields explicitly and leaves committed input untouched when a draft is cancelled. */
function DraftFields({ editor, draft, disabled }: { editor: InputEditor; draft: Draft; disabled: boolean }) {
  return <div className="mt-3 space-y-3 border-t border-slate-100 pt-3" onKeyDown={/** Escape cancels only the pending manual field. */ event => { if (event.key === "Escape") { event.preventDefault(); editor.edit(null); } }}>
    <div className="grid grid-cols-[minmax(0,1fr)_8rem] gap-2"><label className="space-y-1 text-xs text-slate-500" htmlFor={`${editor.id}-name`}>Field<input id={`${editor.id}-name`} autoFocus disabled={disabled} className={control} value={draft.name} onChange={/** Edits identity only within the draft. */ event => editor.setDraft({ ...draft, name: event.target.value })} /></label><label className="space-y-1 text-xs text-slate-500" htmlFor={`${editor.id}-type`}>Type<Select id={`${editor.id}-type`} disabled={disabled} className={control} value={draft.type} onChange={/** Defers type validation until explicit save. */ event => editor.setDraft({ ...draft, type: event.target.value as ValueType })}>{types.map(/** Offers only supported JSON editor types. */ type => <option key={type} value={type}>{type}</option>)}</Select></label></div>
    <label className="block space-y-1 text-xs text-slate-500" htmlFor={`${editor.id}-value`}>Value<textarea id={`${editor.id}-value`} disabled={disabled} className={`${control} min-h-16 font-mono`} value={draft.text} onChange={/** Keeps raw draft text until validation succeeds. */ event => editor.setDraft({ ...draft, text: event.target.value })} /></label>
    {editor.error && <p role="alert" className="text-xs text-red-700">{editor.error}</p>}
    <div className="flex gap-2"><button type="button" disabled={disabled} onClick={editor.save} className="rounded-lg bg-violet-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-violet-700 disabled:opacity-50">Save field</button><button type="button" disabled={disabled} onClick={/** Discards the draft while preserving other incomplete fields. */ () => editor.edit(null)} className="rounded-lg px-3 py-1.5 text-xs text-slate-500 hover:bg-slate-50">Cancel</button></div>
  </div>;
}
