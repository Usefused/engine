import { Link, useLoaderData, useSearchParams } from "@remix-run/react";
import { listWorkflows } from "~/lib/workflow-api";
import { workflowSelectionURL } from "~/lib/workflow-library";
import {
  WorkflowDefinition,
  WorkflowPageHeader,
  WorkflowRequirements,
} from "~/components/workflows/WorkflowDetails";

/** Resolves one immutable release; unavailable dependencies must not render an installable stale page. */
export async function clientLoader({ params }: { params: { id?: string } }) {
  // An absent route identity cannot fall back to an arbitrary catalogue item.
  if (!params.id) throw new Response("Workflow not found", { status: 404 });
  return (await listWorkflows("", [params.id])).items[0];
}

/** Presents the workflow's details inside Engine and adds it to the current multi-workflow selection. */
export default function WorkflowDetailsPage() {
  const workflow = useLoaderData<typeof clientLoader>();
  const [params] = useSearchParams();
  const selected = [...new Set([...params.getAll("workflow"), workflow.id])];
  return (
    <main className="mx-auto max-w-5xl p-4 sm:p-8 space-y-6">
      <Link
        className="text-sm text-violet-700"
        to={workflowSelectionURL(
          "/integrations/workflows",
          params.getAll("workflow")
        )}
      >
        ← Workflows
      </Link>
      <WorkflowPageHeader
        title={workflow.template.name}
        description={workflow.template.description}
      />
      <p className="text-sm text-slate-500">
        Published by {workflow.publisher} · Version {workflow.template.version}
      </p>
      <div className="flex flex-wrap gap-3">
        <Link
          className="rounded-lg bg-violet-600 px-4 py-2 text-white"
          to={workflowSelectionURL("/integrations/workflows/install", selected)}
        >
          Add to app
        </Link>
        <Link
          className="rounded-lg border px-4 py-2"
          to={workflowSelectionURL("/integrations/workflows", selected)}
        >
          Select and keep browsing
        </Link>
      </div>
      <WorkflowRequirements workflows={[workflow]} />
      <WorkflowDefinition workflow={workflow} />
    </main>
  );
}
