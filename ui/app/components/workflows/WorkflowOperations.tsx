import { useEffect, useRef, useState } from "react";
import { ArrowLeft, ArrowRight, Check, Copy, X } from "lucide-react";
import { OperationDisclosure, OperationParameterTable, type OperationParameter } from "~/components/OperationDetailsParts";
import { SchemaViewer, type JsonSchemaNode } from "~/components/SchemaViewer";
import type { Workflow } from "~/lib/workflow-library";

type Operation = Record<string, unknown>;

/** Narrows published JSON without treating missing mappings as executable defaults. */
function record(value: unknown): Record<string, unknown> {
  // Schema and binding containers must be objects; arrays and absent values have no named fields.
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

/** Splits the private routing selector into a readable service key and its owner. */
function serviceIdentity(value: unknown) {
  const service = String(value ?? "");
  const separator = service.lastIndexOf("/");
  // Unqualified selectors remain intact and do not imply an owner.
  return { name: separator < 0 ? service : service.slice(separator + 1), owner: separator < 0 ? "" : service.slice(0, separator) };
}

/** Shows operations as data paths so a workflow reads as an orchestration, not a service directory. */
export function WorkflowOperations({ workflow, inline = false }: { workflow: Workflow; inline?: boolean }) {
  const operations = Object.entries(workflow.template.unified_operations);
  const [selected, setSelected] = useState<string | null>(operations[0]?.[0] ?? null);
  const [inspected, setInspected] = useState<string | null>(null);
  // A removed selection falls back to the first remaining published action.
  const activeName = selected && workflow.template.unified_operations[selected] ? selected : operations[0]?.[0] ?? null;
  const active = activeName ? workflow.template.unified_operations[activeName] : null;
  // The drawer shows a flow in place, while standalone catalogue details keep a separate inspect action.
  if (!inline) return <>
    <ol className="divide-y divide-slate-200 border-y border-slate-200">
      {operations.map(([name, operation]) => {
        const stepCount = Object.keys(record(operation.bindings)).length;
        return <li key={name}>
          <button type="button" onClick={() => setInspected(name)} aria-haspopup="dialog" className="flex w-full items-center justify-between gap-4 py-4 text-left hover:bg-slate-50">
            <span className="min-w-0"><code className="break-all text-sm font-semibold text-slate-900">{name}</code><span className="ml-2 text-xs text-slate-500">{stepCount} {stepCount === 1 ? "service call" : "service calls"}</span></span>
            <ArrowRight className="h-4 w-4 shrink-0 text-slate-400" aria-hidden="true" />
          </button>
        </li>;
      })}
      {/* No-action workflows remain inspectable without implying an empty catalogue result. */}
      {operations.length === 0 && <li className="py-8 text-center text-sm text-slate-500">This workflow does not define any operations.</li>}
    </ol>
    {/* Closing an inspection clears only the temporary modal target. */}
    {inspected && workflow.template.unified_operations[inspected] && <WorkflowOperationSidebar key={inspected} name={inspected} operation={workflow.template.unified_operations[inspected]} workflow={workflow} onClose={() => setInspected(null)} />}
  </>;
  return <section aria-label="Workflow data flow" className="min-w-0 space-y-5">
    {/* A single action needs no selector to explain which flow is being shown. */}
    {operations.length > 1 && <OperationPicker operations={operations} selected={activeName} onSelect={setSelected} />}
    {/* Empty or invalid releases must not render a fabricated operation. */}
    {activeName && active ? <WorkflowOperationFlow name={activeName} operation={active} workflow={workflow} /> : (
      <p className="py-8 text-sm text-slate-500">This workflow does not define any operations.</p>
    )}
  </section>;
}

/** Lets users switch callable workflow actions without repeating a catalogue-style row for each one. */
function OperationPicker({ operations, selected, onSelect }: { operations: [string, Operation][]; selected: string | null; onSelect: (name: string) => void }) {
  return <nav aria-label="Workflow actions" className="mb-5 flex gap-2 overflow-x-auto border-b border-slate-200 pb-3">
    {operations.map(([name]) => (
      <button key={name} type="button" aria-pressed={selected === name} onClick={() => onSelect(name)}
        className={`shrink-0 border-b-2 px-2 pb-2 text-sm font-medium ${selected === name ? "border-slate-900 text-slate-950" : "border-transparent text-slate-500 hover:text-slate-800"}`}>
        {name}
      </button>
    ))}
  </nav>;
}

/** Renders named input fields as the entry point of the workflow data path. */
function InputNode({ schema }: { schema: unknown }) {
  const input = record(schema);
  const properties = Object.entries(record(input.properties));
  const required = new Set(Array.isArray(input.required) ? input.required.map(String) : []);
  return <section className="min-w-0" aria-label="Workflow inputs">
    <h3 className="text-[10px] font-semibold uppercase tracking-[0.14em] text-slate-500">Inputs</h3>
    {/* Only declared properties become fields in the compact input contract. */}
    {properties.length ? <ul className="mt-2 divide-y divide-slate-200 border-y border-slate-200">
      {properties.map(([name, value]) => {
        const property = record(value);
        // Union field types remain authored facts and are kept readable in the compact flow node.
        const type = Array.isArray(property.type) ? property.type.join(" | ") : String(property.type ?? "schema");
        return <li key={name} className="flex min-w-0 items-baseline justify-between gap-2 py-2 text-xs">
          <code className="break-all font-medium text-slate-900">{name}</code>
          <span className="shrink-0 text-slate-500">{type}{required.has(name) ? " · required" : ""}</span>
        </li>;
      })}
    </ul> : <p className="mt-2 border-y border-slate-200 py-3 text-xs text-slate-500">No named inputs</p>}
  </section>;
}

/** Shows each provider call and its authored dependencies without inventing sequence between independent steps. */
function StepNode({ name, value }: { name: string; value: unknown }) {
  const step = record(value);
  const output = record(step.output);
  const dependencies = Array.isArray(step.depends_on) ? step.depends_on.map(String) : [];
  const service = serviceIdentity(step.service);
  return <li className="min-w-0 border-l-2 border-slate-300 py-1 pl-3">
    <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1">
      <code className="break-all text-xs font-semibold text-slate-950">{name}</code>
      <span className="text-xs text-slate-500">calls</span>
      <code className="break-all text-xs text-slate-700">{String(step.operation ?? "operation")}</code>
    </div>
    {/* No service label is shown when the binding omitted its target. */}
    {service.name && <p className="mt-1 break-words text-[11px] text-slate-500">{service.name}{service.owner && <span> · {service.owner}</span>}</p>}
    {dependencies.length > 0 && <p className="mt-2 text-[11px] text-slate-600">Uses results from {dependencies.join(", ")}</p>}
    {/* Mappings use the full step width so expressions do not split midway through a field name. */}
    <div className="mt-2 space-y-2">
      {step.input !== undefined && <MappingLine label="Input mapping" value={step.input} />}
      {output.mapping !== undefined && <MappingLine label="Output mapping" value={output.mapping} />}
    </div>
  </li>;
}

/** Makes authored mappings visible in the overview while preserving their literal expressions. */
function MappingLine({ label, value }: { label: string; value: unknown }) {
  return <div className="min-w-0 text-[11px] leading-4 text-slate-600">
    <span className="font-medium text-slate-500">{label}</span>
    {/* Keep authored JSON intact; a narrow view scrolls instead of breaking expressions into misleading fragments. */}
    <div className="mt-0.5 max-w-full overflow-x-auto"><code className="block w-max whitespace-pre">{typeof value === "string" ? value : JSON.stringify(value)}</code></div>
  </div>;
}

/** Places inputs, provider calls and returned data in separate lanes, preserving graph semantics. */
function WorkflowOperationFlow({ name, operation, workflow }: { name: string; operation: Operation; workflow: Workflow }) {
  const output = record(operation.output);
  const bindings = Object.entries(record(operation.bindings));
  // Only authored dependency edges justify suggesting a relationship among calls.
  const dependenciesDeclared = bindings.some(([, binding]) => Array.isArray(record(binding).depends_on) && record(binding).depends_on.length > 0);
  return <div className="min-w-0">
    <header className="mb-5 border-b border-slate-200 pb-4">
      <p className="text-[10px] font-semibold uppercase tracking-[0.14em] text-slate-500">Workflow action</p>
      <h2 className="mt-1 break-all text-lg font-semibold text-slate-950">{name}</h2>
      {operation.description && <p className="mt-1 text-sm leading-5 text-slate-600">{String(operation.description)}</p>}
    </header>
    <div className="min-w-0 space-y-6">
      <InputNode schema={operation.input} />
      <section className="min-w-0" aria-label="Provider calls">
        <h3 className="text-[10px] font-semibold uppercase tracking-[0.14em] text-slate-500">Calls these services</h3>
        {/* An empty binding map means the action has no provider work to summarize. */}
        {bindings.length ? <>
          <ul className="mt-2 space-y-3 border-y border-slate-200 py-3">{bindings.map(([step, value]) => <StepNode key={step} name={step} value={value} />)}</ul>
          <p className="mt-2 text-[10px] leading-4 text-slate-500">{dependenciesDeclared ? "Connections reflect declared dependencies." : "Steps have no declared dependencies; no execution order is implied."}</p>
        </> : <p className="mt-2 border-y border-slate-200 py-3 text-xs text-slate-500">No provider calls</p>}
      </section>
      <section className="min-w-0" aria-label="Workflow output">
        <h3 className="text-[10px] font-semibold uppercase tracking-[0.14em] text-slate-500">Returns</h3>
        {/* Only authored schemas promise named return fields. */}
        {output.schema ? <SchemaFields schema={output.schema} /> : <p className="mt-2 border-y border-slate-200 py-3 text-xs text-slate-600">Selected step results</p>}
        {/* A mapping is shown only when the release explicitly projects one. */}
        {output.mapping !== undefined && <div className="mt-2"><MappingLine label="Output mapping" value={output.mapping} /></div>}
      </section>
    </div>
    <WorkflowSDKExample name={name} operation={operation} />
    <WorkflowTechnicalDetails name={name} operation={operation} workflow={workflow} />
  </div>;
}

/** Summarizes declared output fields rather than showing the raw JSON schema in the main flow. */
function SchemaFields({ schema }: { schema: unknown }) {
  const output = record(schema);
  const properties = Object.entries(record(output.properties));
  const required = new Set(Array.isArray(output.required) ? output.required.map(String) : []);
  return properties.length ? <ul className="mt-2 divide-y divide-slate-200 border-y border-slate-200">
    {properties.map(([name, value]) => {
      const field = record(value);
      // Union types remain explicit instead of being collapsed to a guessed scalar type.
      const type = Array.isArray(field.type) ? field.type.join(" | ") : String(field.type ?? "schema");
      return <li key={name} className="flex min-w-0 items-baseline justify-between gap-2 py-2 text-xs"><code className="break-all font-medium">{name}</code><span className="shrink-0 text-slate-500">{type}{required.has(name) ? " · required" : ""}</span></li>;
    })}
  </ul> : <p className="mt-2 border-y border-slate-200 py-3 text-xs text-slate-600">Defined output schema</p>;
}

/** Creates harmless placeholder values for required fields from the published input schema. */
function workflowInputExample(schema: unknown): string {
  const input = record(schema);
  const properties = Object.entries(record(input.properties));
  const required = new Set(Array.isArray(input.required) ? input.required.map(String) : []);
  // Optional fields are omitted so the example stays focused on the minimum required call input.
  const fields = properties.filter(([name]) => required.has(name));
  const values = fields.map(([name, value]) => {
    const field = record(value);
    const types = Array.isArray(field.type) ? field.type : [field.type];
    // Placeholder literals follow the declared schema type and never copy authored example data.
    const example = types.includes("integer") || types.includes("number") ? "123"
      : types.includes("boolean") ? "true"
      : types.includes("array") ? "[]"
      : types.includes("object") ? "{}"
      : '"your-value"';
    return `    ${JSON.stringify(name)}: ${example},`;
  });
  // An operation with no visible required fields can be demonstrated with an empty object.
  return values.length ? `{
${values.join("\n")}
  }` : "{}";
}

/** Builds the generated TypeScript SDK call with this operation's explicit workflow step targets. */
function workflowSDKCallExample(name: string, operation: Operation): string {
  const targets = Object.keys(record(operation.bindings));
  const input = workflowInputExample(operation.input);
  // The generated method takes an explicit dependency-closed target list as its second argument.
  return `// sdk is your initialized generated Fused SDK client.\nconst result = await sdk.unified.${name}(\n  ${input},\n  { targets: ${JSON.stringify(targets)} }\n);`;
}

/** Displays and copies a runnable invocation for the selected workflow operation. */
function WorkflowSDKExample({ name, operation }: { name: string; operation: Operation }) {
  const [copied, setCopied] = useState(false);
  const code = workflowSDKCallExample(name, operation);
  // Copy feedback is local and temporary; it does not transmit the generated snippet.
  function copyExample() {
    navigator.clipboard.writeText(code);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }
  return <details className="mt-7 overflow-hidden rounded-lg border border-slate-200 bg-white">
    <summary className="flex cursor-pointer list-none items-center justify-between px-3 py-2.5 text-xs font-semibold text-slate-700 hover:bg-slate-50">
      <span>Run this workflow</span><span className="rounded bg-slate-100 px-1.5 py-0.5 text-[10px] font-semibold text-slate-600">TypeScript SDK</span>
    </summary>
    <div className="relative border-t border-slate-200 bg-slate-950">
      <button type="button" onClick={copyExample} className="absolute right-2 top-2 rounded p-1.5 text-slate-400 hover:bg-slate-800 hover:text-white" title="Copy workflow example" aria-label="Copy workflow SDK example">
        {/* The icon confirms a local copy without changing the displayed example. */}
        {copied ? <Check className="h-3.5 w-3.5 text-emerald-400" /> : <Copy className="h-3.5 w-3.5" />}
      </button>
      <pre className="overflow-x-auto p-4 pr-10 text-[11px] leading-5 text-slate-200"><code>{code}</code></pre>
    </div>
  </details>;
}

/** Keeps complete contracts and rollback settings available after the readable data-flow overview. */
function WorkflowTechnicalDetails({ name, operation, workflow }: { name: string; operation: Operation; workflow: Workflow }) {
  const output = record(operation.output);
  return <section className="mt-7 border-t border-slate-200 pt-4">
    <h3 className="mb-2 text-xs font-semibold text-slate-700">Full contract</h3>
    <div className="space-y-2">
      <MappingDetails title="Operation definition" value={operation} />
      <WorkflowSchema title="Input schema" schema={operation.input} workflowID={workflow.id} />
      {Object.entries(record(operation.bindings)).map(([step, value]) => <WorkflowStepDetails key={step} name={step} value={value} workflowID={workflow.id} />)}
      <WorkflowSchema title="Output schema" schema={output.schema} workflowID={workflow.id} />
    </div>
    {!output.schema && <p className="mt-2 text-[11px] text-slate-500">No output schema is declared for {name}.</p>}
  </section>;
}

/** Keeps the complete authored input and output configuration available on demand. */
function MappingDetails({ title, value }: { title: string; value: unknown }) {
  // Missing optional mappings need no empty disclosure control.
  if (value === undefined || value === null) return null;
  return <OperationDisclosure label={title}><pre className="max-h-80 overflow-auto bg-slate-950 p-4 text-[11px] leading-5 text-slate-200">{JSON.stringify(value, null, 2)}</pre></OperationDisclosure>;
}

/** Keeps the complete step contract beside its mapping and compensation details. */
function WorkflowStepDetails({ name, value, workflowID }: { name: string; value: unknown; workflowID: string }) {
  const step = record(value);
  const output = record(step.output);
  const rollback = record(step.rollback);
  const dependencies = Array.isArray(step.depends_on) ? step.depends_on.map(String) : [];
  return <OperationDisclosure label={`${name} · ${String(step.operation ?? "operation")}`}>
    <div className="space-y-3 p-4">
      {step.service && <p className="break-all text-xs text-slate-500"><span className="font-medium text-slate-600">Service</span> {String(step.service)}</p>}
      {dependencies.length > 0 && <p className="text-xs text-slate-600">Depends on: {dependencies.join(", ")}</p>}
      <MappingDetails title="Input mapping" value={step.input} />
      <WorkflowSchema title="Step output schema" schema={output.schema} workflowID={workflowID} />
      <MappingDetails title="Output mapping" value={output.mapping} />
      {step.rollback != null && <div className="space-y-3 border-t border-slate-200 pt-3 text-xs text-slate-700"><span className="font-semibold">Rollback</span> · <code>{String(rollback.operation ?? "")}</code><MappingDetails title="Rollback input mapping" value={rollback.input} /></div>}
    </div>
  </OperationDisclosure>;
}

/** Keeps a complete schema available beneath the concise field summary. */
function WorkflowSchema({ title, schema, workflowID }: { title: string; schema: unknown; workflowID: string }) {
  // An absent schema is unknown and must never become an invented response contract.
  if (!schema || typeof schema !== "object") return null;
  return <OperationDisclosure label={title}><div className="bg-[#161c27] p-4"><SchemaViewer schema={schema as JsonSchemaNode} serviceId={workflowID} componentScope={workflowID} allowRemoteRefs={false} /></div></OperationDisclosure>;
}

/** Projects named inputs into the shared parameter table while preserving requiredness from the schema. */
function workflowParameters(input: unknown): OperationParameter[] {
  const schema = record(input);
  // Only the authored required set determines whether a field is mandatory.
  const required = new Set(Array.isArray(schema.required) ? schema.required : []);
  return Object.entries(record(schema.properties)).map(([name, value]) => {
    const property = record(value);
    // Union types are displayed together; references and inferred shapes stay explicitly schema-defined.
    const type = Array.isArray(property.type) ? property.type.join(" | ") : String(property.type ?? "schema");
    return { name, in: "input", type, required: required.has(name) };
  });
}

/** Opens a standalone workflow action in a native modal with browser-managed focus. */
function WorkflowOperationSidebar({ name, operation, workflow, onClose }: { name: string; operation: Operation; workflow: Workflow; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  // A newly selected operation starts with browser-managed focus inside its inspector.
  useEffect(() => {
    const node = dialog.current;
    node?.showModal();
    return () => node?.close();
  }, []);
  // Restore focus before React removes the modal from the document.
  function close() {
    dialog.current?.close();
    onClose();
  }
  return <dialog ref={dialog} aria-labelledby="workflow-operation-name" onCancel={(event) => { event.preventDefault(); close(); }}
    onClick={(event) => { /* Only the backdrop dismisses the inspector; content clicks stay inside it. */ if (event.target === event.currentTarget) close(); }}
    className="fixed inset-y-0 left-auto right-0 m-0 h-dvh max-h-none w-full max-w-none overflow-y-auto overflow-x-hidden border-l border-slate-200 bg-white p-0 text-slate-900 shadow-2xl backdrop:bg-slate-900/20 md:w-[600px]">
    <div className="flex min-h-full flex-col">
      <header className="sticky top-0 z-10 border-b border-slate-200 bg-white/95 p-4 backdrop-blur sm:p-6">
        <div className="flex items-start justify-between gap-4"><div className="min-w-0"><p className="text-[10px] font-semibold uppercase tracking-wide text-slate-500">Workflow data path</p><h2 id="workflow-operation-name" className="mt-2 break-all text-lg font-semibold text-slate-950">{name}</h2><p className="mt-1 text-xs text-slate-500">Part of {workflow.template.name}</p></div>
          <button type="button" onClick={close} aria-label="Close workflow operation details" className="rounded-full p-2 text-slate-500 hover:bg-slate-100 hover:text-slate-900"><X className="h-4 w-4" /></button></div>
      </header>
      <div className="p-4 sm:p-6"><WorkflowOperationFlow name={name} operation={operation} workflow={workflow} /></div>
    </div>
  </dialog>;
}
