import { Link } from "@remix-run/react";
import { ArrowUpRight } from "lucide-react";
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
      <h1 className="text-xl font-semibold text-slate-900">
        {title}
      </h1>
      <p className="mt-1 max-w-3xl text-sm text-slate-500">{description}</p>
    </header>
  );
}

type SetupNote = { text: string; workflows: Map<string, string> };
type ServiceRequirement = {
  identity: string;
  key: string;
  serviceID: string;
};

/** Groups shared setup notes and lists each service once; version and operation details belong in Operations. */
function summarizeRequirements(workflows: Workflow[]) {
  const notes = new Map<string, SetupNote>();
  const services = new Map<string, ServiceRequirement>();
  for (const workflow of workflows) {
    for (const requirement of workflow.template.requirements) {
      const text = requirement.trim();
      // Blank authored notes provide no setup guidance and should not create empty rows.
      if (!text) continue;
      // Shared notes appear once while retaining attribution for workflow-specific guidance.
      const note = notes.get(text) ?? { text, workflows: new Map<string, string>() };
      note.workflows.set(workflow.id, workflow.template.name);
      notes.set(text, note);
    }
    for (const [key, service] of Object.entries(workflow.template.services)) {
      // This is a service directory, so multiple pins or workflows must not repeat the same service.
      const identity = service.service_id;
      services.set(identity, { identity, key, serviceID: service.service_id });
    }
  }
  return { notes: [...notes.values()], services: [...services.values()] };
}

/** Keeps attribution readable but secondary to the setup instruction, without repeating shared workflow names. */
function RequirementAttribution({ note, count }: { note: SetupNote; count: number }) {
  // Single-workflow details already establish the source in their page header.
  if (count < 2) return null;
  return <p className="mt-1 text-xs leading-5 text-slate-500">
    {/* Shared guidance needs one scope label rather than a repeated list of selected titles. */}
    {note.workflows.size === count ? "Applies to all selected workflows" : [...note.workflows.values()].join(" · ")}
  </p>;
}

/** Shows only service identity and its owner, leaving execution details to the Operations tab. */
function RequiredService({ service }: { service: ServiceRequirement }) {
  const separator = service.key.indexOf("/");
  // Qualified references identify the service owner; unqualified names must not imply workflow ownership.
  const name = separator < 0 ? service.key : service.key.slice(separator + 1);
  const publisher = separator < 0 ? "" : service.key.slice(0, separator);
  return <li className="min-w-0 px-3 py-3">
    <Link to={`/integrations/${service.serviceID}`} className="group inline-flex min-w-0 max-w-full items-center gap-1 text-sm font-medium text-gray-900 underline-offset-4 hover:underline">
      <span className="break-all">{name}</span><ArrowUpRight className="h-3 w-3 shrink-0 text-gray-400 group-hover:text-gray-700" aria-hidden="true" />
    </Link>
    {/* Show ownership only when the service reference provides it. */}
    {publisher && <p className="mt-0.5 break-all text-xs leading-4 text-gray-500">{publisher}</p>}
  </li>;
}

/** Separates setup guidance from the service-and-owner directory shared with App Builder. */
export function WorkflowRequirements({ workflows, embedded = false }: { workflows: Workflow[]; embedded?: boolean }) {
  const { notes, services } = summarizeRequirements(workflows);
  return (
    /* App Builder supplies its own surface; the drawer needs only a plain white canvas. */
    <section className={embedded ? "min-w-0 space-y-6" : "min-w-0 space-y-6 bg-white"}>
      <section className="rounded-lg bg-gray-50 p-4">
        <h3 className="text-sm font-semibold text-gray-900">Before you run</h3>
        {/* An empty manifest is not proof that its provider needs no credentials. */}
        {notes.length === 0 ? <p className="mt-2 text-sm leading-6 text-gray-600">No additional setup notes were provided.</p> : (
          <ul className="mt-2 space-y-3">
            {notes.map((note) => <li key={note.text} className="min-w-0">
              <p className="break-words text-sm leading-6 text-gray-600">{note.text}</p>
              <RequirementAttribution note={note} count={workflows.length} />
            </li>)}
          </ul>
        )}
      </section>
      <section className="border-t border-gray-200 pt-5">
        <h3 className="text-sm font-semibold text-gray-900">Services used <span className="ml-1 font-normal text-gray-500">({services.length})</span></h3>
        <ul className="mt-3 divide-y divide-gray-100 rounded-lg border border-gray-200">{services.map((service) => <RequiredService key={service.identity} service={service} />)}</ul>
      </section>
    </section>
  );
}
