import { useId, useState } from "react";
import { Plus, X } from "lucide-react";
import { Select } from "~/components/forms/Select";
import { unifiedInputFields, type InputValueType } from "~/lib/unified-app-input-fields";

type ValueType = InputValueType;
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

/** Edits schema-derived values directly in chips, retaining invalid drafts until corrected or cleared. */
export function UnifiedAppInput({ value, onChange, disabled, onEditingChange, schema }: { schema?: unknown; value: string; onChange: (value: string) => void; disabled: boolean; onEditingChange: (editing: boolean) => void }) {
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
      const next = absent ? undefined : type === "string" ? text : parseJSON(text);
      if (!absent && type !== "json" && valueType(next) !== type) throw new Error("Wrong JSON type");
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
      if (!draft.name) throw new Error("Enter a field name.");
      if (draft.name !== draft.original && Object.hasOwn(object, draft.name)) throw new Error("That field already exists.");
      // Text is literal; all other types use JSON syntax to preserve their exact shape.
      const next = draft.type === "string" ? draft.text : parseJSON(draft.text);
      if (draft.type !== "json" && valueType(next) !== draft.type) throw new Error(`Enter a JSON ${draft.type} value.`);
      const entries = Object.entries(object).filter(([name]) => name !== draft.original);
      // Object.fromEntries treats special keys as data instead of invoking prototype setters.
      onChange(JSON.stringify(Object.fromEntries([...entries, [draft.name, next]]), null, 2));
      edit(null);
    } catch {
      // Validation never echoes potentially sensitive field contents into an error message.
      setError(!draft.name ? "Enter a field name." : draft.name !== draft.original && Object.hasOwn(object, draft.name) ? "That field already exists." : `Enter a valid ${draft.type} value. Use JSON syntax for numbers, booleans, lists, and objects.`);
    }
  }

  /** Clears a property and its local draft without altering other values or their validation state. */
  function remove(name: string) {
    // The JSON editor owns invalid documents; chip actions cannot discard those drafts.
    if (!object) return;
    resetInline(name);
    onChange(JSON.stringify(Object.fromEntries(Object.entries(object).filter(([key]) => key !== name)), null, 2));
  }

  return <section aria-label="App input" className="min-w-0">
    <div className="mb-2 flex items-center justify-between gap-3"><h3 className="text-sm font-medium text-slate-800">Input</h3><div className="flex rounded-lg bg-slate-100 p-0.5" aria-label="Input editor">
      {/* Switching views never regenerates or coerces the payload; incomplete values must be corrected or cleared first. */}
      {(["fields", "json"] as const).map((view) => <button key={view} type="button" disabled={disabled || Boolean(draft) || invalidFields.size > 0 || (view === "fields" && !canUseFields)} aria-pressed={mode === view} onClick={() => { setInlineText({}); setMode(view); }} className={`rounded-md px-2.5 py-1 text-xs font-medium transition disabled:opacity-40 ${mode === view ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-800"}`}>{view === "fields" ? "Fields" : "JSON"}</button>)}
    </div></div>
    {/* JSON remains available for nested documents, non-object inputs, and syntax corrections. */}
    {mode === "json" ? <textarea aria-label="Input JSON" spellCheck={false} className={`${control} min-h-28 font-mono text-xs leading-6`} disabled={disabled} value={value} onChange={(event) => onChange(event.target.value)} /> : <div className="rounded-xl border border-slate-200 bg-white p-3 focus-within:border-violet-300 focus-within:ring-2 focus-within:ring-violet-50">
      <div className="flex min-h-9 flex-wrap items-center gap-2">
        {/* Declared fields remain visible when absent, so users only fill values and never type schema keys. */}
        {fields ? fields.map((item) => {
          const present = Boolean(object && Object.hasOwn(object, item.name));
          const current = object?.[item.name];
          // Local text preserves unfinished syntax and numeric formatting while valid values update the request immediately.
          const text = Object.hasOwn(inlineText, item.name) ? inlineText[item.name] : present ? typeof current === "string" && item.type === "string" ? current : JSON.stringify(current) : "";
          const invalid = invalidFields.has(item.name);
          const choices = item.choices ?? (item.type === "boolean" ? [true, false] : item.type === "null" ? [null] : undefined);
          // Option indices distinguish an unset value from legitimate empty strings, false, zero, and null.
          const choiceIndex = present ? choices?.findIndex((choice) => JSON.stringify(choice) === JSON.stringify(current)) : -1;
          return <span key={item.name} title={item.description} className={`inline-flex max-w-full items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs focus-within:ring-2 ${invalid ? "border-red-300 bg-red-50 text-red-900 focus-within:ring-red-100" : "border-violet-100 bg-violet-50/70 text-violet-950 focus-within:border-violet-300 focus-within:ring-violet-100"}`}>
            <label htmlFor={`${id}-${item.name}`} className="max-w-40 shrink-0 truncate font-semibold">{item.name}{item.required && <span aria-hidden="true" className="ml-0.5 text-violet-400">*</span>}</label><span aria-hidden="true" className="text-violet-300">:</span>
            {/* Values are native inline controls; typed choices and free text both update without a save action. */}
            {choices ? <Select density="compact" tone="subtle" id={`${id}-${item.name}`} aria-label={item.name} disabled={disabled} className="min-w-0 max-w-56 bg-transparent py-0.5 outline-none" value={String(choiceIndex ?? -1)} onChange={(event) => {
              // Returning to the placeholder removes the property rather than sending a fabricated default.
              if (event.target.value === "-1") { remove(item.name); return; }
              const choice = choices[Number(event.target.value)];
              changeInline(item.name, item.type, typeof choice === "string" && item.type === "string" ? choice : JSON.stringify(choice));
            }}><option value="-1">{item.required ? "Set value" : "Optional"}</option>{choices.map((choice, index) => <option key={index} value={index}>{JSON.stringify(choice)}</option>)}</Select> : <textarea id={`${id}-${item.name}`} aria-label={item.name} aria-required={item.required} aria-invalid={invalid} aria-describedby={invalid ? `${id}-invalid` : undefined} title={item.description} rows={1} disabled={disabled} spellCheck={false} value={text} placeholder={item.required ? "Set value" : "Optional"} className="min-w-0 max-w-64 resize-none bg-transparent py-0.5 font-mono leading-5 outline-none placeholder:font-sans placeholder:text-violet-400" style={{ width: `${Math.min(30, Math.max(9, text.length + 1))}ch` }} onChange={(event) => changeInline(item.name, item.type, event.target.value)} onKeyDown={(event) => {
              // Escape restores only this chip; Enter leaves the value in place without submitting a surrounding form.
              if (event.key === "Escape") { event.preventDefault(); resetInline(item.name); }
              if (event.key === "Enter" && !event.shiftKey) { event.preventDefault(); event.currentTarget.blur(); }
            }} />}
            {/* A clear affordance also removes malformed local text, allowing optional values to remain omitted. */}
            {(present || Object.hasOwn(inlineText, item.name)) && <button type="button" disabled={disabled} aria-label={`Clear ${item.name}`} onClick={() => remove(item.name)} className="rounded p-1 text-violet-400 hover:bg-violet-100 hover:text-violet-800 disabled:opacity-50"><X className="h-3 w-3" aria-hidden="true" /></button>}
          </span>;
        }) : Object.entries(object ?? {}).map(([name, item]) => <span key={name} className="inline-flex max-w-full items-center rounded-lg border border-violet-100 bg-violet-50/70 text-xs text-violet-950">
          <button type="button" disabled={disabled || Boolean(draft)} aria-label={`Edit ${name}`} title={`${name}: ${JSON.stringify(item)}`} onClick={() => edit({ original: name, name, type: valueType(item), text: typeof item === "string" ? item : JSON.stringify(item, null, 2) })} className="flex min-w-0 items-center gap-1.5 rounded-l-lg px-2.5 py-2 text-left hover:bg-violet-100 disabled:opacity-50"><span className="max-w-40 truncate font-semibold">{name}</span><span className="text-violet-300">:</span><span className="max-w-56 truncate font-mono">{JSON.stringify(item)}</span></button>
          <button type="button" disabled={disabled || Boolean(draft)} aria-label={`Remove ${name}`} onClick={() => remove(name)} className="mr-1 rounded p-1 text-violet-400 hover:bg-violet-100 hover:text-violet-800 disabled:opacity-50"><X className="h-3 w-3" aria-hidden="true" /></button>
        </span>)}
        {/* Free-form keys remain available only when no authored contract was provided. */}
        {!hasSchema && !draft && <button type="button" disabled={disabled} onClick={() => edit({ original: null, name: "", type: "string", text: "" })} className="inline-flex items-center gap-1 rounded-lg px-2 py-2 text-xs font-medium text-slate-500 hover:bg-slate-50 hover:text-violet-700 disabled:opacity-50"><Plus className="h-3.5 w-3.5" aria-hidden="true" />Add field</button>}
        {fields?.length === 0 && <p className="px-1 text-xs text-slate-400">This app has no declared input fields.</p>}
      </div>
      {invalidFields.size > 0 && <p id={`${id}-invalid`} role="alert" className="mt-2 text-xs text-red-700">Finish or clear the highlighted values before running. Use valid numbers or JSON for structured values.</p>}
      {/* Extra JSON keys are retained and disclosed instead of becoming invisible in the chip view. */}
      {fields && object && Object.keys(object).some((name) => !fields.some((item) => item.name === name)) && <p className="mt-2 text-xs text-amber-700">Additional fields are present. Use JSON to review them.</p>}
      {/* Editing stays inside the input boundary, with explicit commit/cancel and accessible labels. */}
      {draft && <div className="mt-3 space-y-3 border-t border-slate-100 pt-3" onKeyDown={(event) => { /* Escape discards only the unfinished chip, preserving the last committed payload. */ if (event.key === "Escape") { event.preventDefault(); edit(null); } }}>
        {/* Schema-less inputs still need a name and type when authoring a new field. */}
        <div className="grid grid-cols-[minmax(0,1fr)_8rem] gap-2"><label className="space-y-1 text-xs text-slate-500" htmlFor={`${id}-name`}>Field<input id={`${id}-name`} autoFocus disabled={disabled} className={control} value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} /></label><label className="space-y-1 text-xs text-slate-500" htmlFor={`${id}-type`}>Type<Select id={`${id}-type`} disabled={disabled} className={control} value={draft.type} onChange={(event) => setDraft({ ...draft, type: event.target.value as ValueType })}>{types.map((type) => <option key={type} value={type}>{type}</option>)}</Select></label></div>
        <label className="block space-y-1 text-xs text-slate-500" htmlFor={`${id}-value`}>Value<textarea id={`${id}-value`} disabled={disabled} className={`${control} min-h-16 font-mono`} value={draft.text} onChange={(event) => setDraft({ ...draft, text: event.target.value })} /></label>
        {error && <p role="alert" className="text-xs text-red-700">{error}</p>}
        <div className="flex gap-2"><button type="button" disabled={disabled} onClick={save} className="rounded-lg bg-violet-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-violet-700 disabled:opacity-50">Save field</button><button type="button" disabled={disabled} onClick={() => edit(null)} className="rounded-lg px-3 py-1.5 text-xs text-slate-500 hover:bg-slate-50">Cancel</button></div>
      </div>}
    </div>}
    <p className="mt-2 text-xs text-slate-400">{hasSchema ? "Edit values directly in the chips. Changes apply as you type." : "Click a field to edit its value. Use JSON for the full input."}</p>
  </section>;
}
