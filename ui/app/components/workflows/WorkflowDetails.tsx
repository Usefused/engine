import type { Workflow } from "~/lib/workflow-library";

/** Keeps Engine workflow screens consistent with the existing catalogue's typography and spacing. */
export function WorkflowPageHeader({
  title,
  description,
}: {
  title: string;
  description: string;
}) {
  return (
    <header>
      <h1 className="text-3xl font-bold tracking-tight text-slate-900">
        {title}
      </h1>
      <p className="mt-2 max-w-3xl text-slate-600">{description}</p>
    </header>
  );
}

/** Shows authored setup guidance alongside dependencies derived from the published executable graph. */
export function WorkflowRequirements({ workflows }: { workflows: Workflow[] }) {
  return (
    <section className="rounded-xl border bg-white p-5 space-y-4">
      <h2 className="text-lg font-semibold">Requirements</h2>
      <p className="text-sm text-slate-600">
        Required service versions will be enabled in this workspace. Choose
        credentials for the app and connect provider accounts before running it.
      </p>
      {workflows.map((workflow) => (
        <div key={workflow.id} className="space-y-2">
          <h3 className="font-medium">{workflow.template.name}</h3>
          <ul className="list-disc pl-5 text-sm text-slate-600 space-y-1">
            {workflow.template.requirements.map((requirement, index) => (
              <li key={index}>{requirement}</li>
            ))}
            {Object.entries(workflow.template.services).map(
              ([key, service]) => (
                <li key={key} className="break-words">
                  {key} · {service.version}
                  <span className="block text-xs">
                    Operations: {service.operations.join(", ")}
                  </span>
                </li>
              )
            )}
          </ul>
        </div>
      ))}
    </section>
  );
}

/** Exposes real input/output schemas and bindings, including provider writes and rollback behavior, without running them. */
export function WorkflowDefinition({ workflow }: { workflow: Workflow }) {
  return (
    <section className="space-y-4">
      <h2 className="text-lg font-semibold">Operations</h2>
      {Object.entries(workflow.template.unified_operations).map(
        ([name, operation]) => (
          <article className="rounded-xl border bg-white p-5" key={name}>
            <h3 className="font-mono font-semibold break-all">{name}</h3>
            <p className="mt-2 text-sm text-slate-600">
              {String(operation.description ?? "")}
            </p>
            <details className="mt-4">
              <summary className="cursor-pointer text-sm font-medium text-violet-700">
                Inputs, steps, and output configuration
              </summary>
              <pre className="mt-3 max-h-[32rem] overflow-auto rounded-lg bg-slate-50 p-4 text-xs">
                {JSON.stringify(operation, null, 2)}
              </pre>
            </details>
          </article>
        )
      )}
    </section>
  );
}
