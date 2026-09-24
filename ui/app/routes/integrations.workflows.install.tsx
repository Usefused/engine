import { redirect } from "@remix-run/react";
import { workflowSelectionURL } from "~/lib/workflow-library";

/** Preserves saved installation links while delegating all setup to the shared App Builder. */
export function clientLoader({ request }: { request: Request }) {
  const ids = new URL(request.url).searchParams.getAll("workflow");
  return redirect(workflowSelectionURL("/integrations/builder", ids));
}

/** Navigation finishes in App Builder before any configuration or mutation is presented. */
export default function WorkflowInstallRedirect() {
  return null;
}
