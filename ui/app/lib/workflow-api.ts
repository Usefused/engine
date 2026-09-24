import { api } from "./api";
import {
  decodeWorkflow,
  workflowDependencies,
  WORKFLOW_QUERY,
  type Workflow,
  type WorkflowRelease,
} from "./workflow-library";

/** Reads published templates through Engine's authenticated and permission-checked Registry proxy. */
export async function listWorkflows(
  search = "",
  ids: string[] = [],
  offset = 0
): Promise<{ items: Workflow[]; total: number }> {
  const response = await api.graphql<{
    workflows: { items: WorkflowRelease[]; total: number };
  }>(WORKFLOW_QUERY, { search, ids, limit: 32, offset }, { cache: "no-store" });
  const items = await Promise.all(response.workflows.items.map(decodeWorkflow));
  // Exact selection cannot silently become a partial install when a release becomes unavailable.
  if (
    ids.length &&
    (items.length !== new Set(ids).size ||
      items.some((item) => !ids.includes(item.id)))
  )
    throw new Error("One or more selected workflows are unavailable.");
  return { items, total: response.workflows.total };
}

/** Enables exact workflow dependencies, then runs the existing App Builder lifecycle with partial-failure reporting. */
export async function withWorkflowDependencies<T>(
  workflows: Workflow[],
  progress: (message: string) => void,
  createApp: () => Promise<T>
): Promise<T> {
  // Ordinary service-only builds retain their existing permissions and network calls.
  if (!workflows.length) return createApp();
  const dependencies = workflowDependencies(workflows);
  let stage = "Checking workspace services";
  const enabled: string[] = [];
  try {
    const workspace = await api.workspace.getServices();
    for (const [key, dependency] of Object.entries(dependencies)) {
      const service = workspace.find(
        (item) => item.service_id === dependency.service_id
      );
      const ready = service?.enabled_versions?.some(
        (version) =>
          version.service_version_id === dependency.service_version_id &&
          version.status !== "deprecated"
      );
      // Already enabled versions remain untouched; activation is additive and never alters visibility.
      if (ready) continue;
      stage = `Enabling ${key} ${dependency.version}`;
      progress(stage);
      await api.workspace.addService(
        dependency.service_id,
        key,
        dependency.version,
        dependency.service_version_id
      );
      enabled.push(key);
    }
    stage = "Creating the app";
    progress(stage);
    return await createApp();
  } catch (error) {
    // Successful workspace changes survive an app failure and must be visible before retrying.
    const partial = enabled.length
      ? ` Services enabled: ${enabled.join(", ")}.`
      : "";
    throw new Error(
      `${stage} failed.${partial} ${
        error instanceof Error ? error.message : String(error)
      }`
    );
  }
}

/** Changes release visibility through the owner-checked Registry mutation and verifies the unchanged authoring digest. */
export async function setWorkflowVisibility(id: string, visible: boolean): Promise<Workflow> {
  const result = await api.graphql<{ setWorkflowVisibility: WorkflowRelease }>(`mutation SetWorkflowVisibility($id: ID!, $public: Boolean!) {
    setWorkflowVisibility(id: $id, public: $public) { id publisher hash public is_owner template }
  }`, { id, public: visible });
  return decodeWorkflow(result.setWorkflowVisibility);
}
