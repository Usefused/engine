export interface WorkflowService {
  service_id: string;
  service_version_id: string;
  version: string;
  operations: string[];
}

export interface WorkflowTemplate {
  schema_version: number;
  slug: string;
  version: string;
  name: string;
  description: string;
  category: string;
  requirements: string[];
  services: Record<string, WorkflowService>;
  unified_operations: Record<string, Record<string, unknown>>;
}

export interface WorkflowRelease {
  id: string;
  publisher: string;
  hash: string;
  public: boolean;
  template: string;
}

export interface Workflow extends Omit<WorkflowRelease, "template"> {
  template: WorkflowTemplate;
}

export interface WorkflowSource {
  id: string;
  version: string;
  hash: string;
}
export interface WorkflowAppConfig extends Record<string, unknown> {
  apiVersion: string;
  kind: "sdk" | "mcp";
  name: string;
  version: string;
  bucket: string;
  language?: string;
  description?: string;
  services: Record<
    string,
    { version: string; operations?: string[]; [key: string]: unknown }
  >;
  unified_operations?: Record<string, Record<string, unknown>>;
  workflow_sources?: WorkflowSource[];
}

export const WORKFLOW_QUERY = `query WorkflowLibrary($search: String!, $ids: [ID!], $limit: Int!, $offset: Int!) {
  workflows(search: $search, ids: $ids, limit: $limit, offset: $offset) {
    total items { id publisher hash public template }
  }
}`;

/** Verifies the exact published authoring bytes before showing or composing executable templates. */
export async function decodeWorkflow(
  release: WorkflowRelease
): Promise<Workflow> {
  const bytes = new TextEncoder().encode(release.template);
  // Bound the encoded document rather than JavaScript's UTF-16 character count.
  if (bytes.length > 256 * 1024)
    throw new Error("Workflow template is too large.");
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  const hash = `sha256:${Array.from(new Uint8Array(digest), hexByte).join("")}`;
  // A changed manifest must never be installed under an older release identity.
  if (hash !== release.hash)
    throw new Error("Workflow content verification failed.");
  const template = JSON.parse(release.template) as WorkflowTemplate;
  if (
    template.schema_version !== 1 ||
    !template.services ||
    !template.unified_operations
  ) {
    // An incompatible Registry cannot silently degrade a workflow into physical operations only.
    throw new Error("Unsupported workflow template.");
  }
  return { ...release, template };
}

/** Formats digest bytes consistently with the Registry and CLI SHA-256 convention. */
function hexByte(byte: number): string {
  return byte.toString(16).padStart(2, "0");
}

/** Recursively sorts JSON object keys so equivalent mappings do not conflict because of property ordering. */
function canonicalJSON(value: unknown): string {
  // Arrays retain their semantic ordering; only object property order is incidental.
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (value !== null && typeof value === "object") {
    return `{${Object.entries(value)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, child]) => `${JSON.stringify(key)}:${canonicalJSON(child)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

/** Combines dependencies once and rejects incompatible version pins before any workspace mutation. */
export function workflowDependencies(
  workflows: Workflow[]
): Record<string, WorkflowService> {
  const combined: Record<string, WorkflowService> = Object.create(null);
  const identities = new Map<string, string>();
  for (const workflow of workflows) {
    for (const [key, service] of Object.entries(workflow.template.services)) {
      mergeWorkflowDependency(combined, identities, key, service);
    }
  }
  return combined;
}

/** Builds one ordinary immutable app config while preserving all existing app routing and authored definitions. */
export function composeWorkflowApp(
  base: WorkflowAppConfig,
  workflows: Workflow[]
): WorkflowAppConfig {
  // An empty selection is not a workflow installation; the app limit also bounds total composition work.
  validateWorkflowSelectionCount(workflows.length);
  // Only adapters with typed Unified support may install these definitions.
  if (
    base.kind === "sdk" &&
    base.language !== "typescript" &&
    base.language !== "python"
  )
    throw new Error("Workflow SDKs require TypeScript or Python.");
  const config = structuredClone(base);
  config.services = Object.assign(Object.create(null), config.services);
  config.unified_operations = Object.assign(
    Object.create(null),
    config.unified_operations ?? {}
  );
  const sources = new Map(
    (config.workflow_sources ?? []).map((source) => [source.id, source])
  );
  mergeWorkflowServices(config, workflowDependencies(workflows));
  for (const workflow of workflows)
    mergeWorkflowDefinition(config, sources, workflow);
  // Combined limits are checked before enabling dependencies, not only by the later Engine plan.
  if (Object.keys(config.unified_operations!).length > 64 || sources.size > 32)
    throw new Error(
      "The combined app exceeds the workflow or unified operation limit."
    );
  config.workflow_sources = [...sources.values()].sort((left, right) =>
    left.id.localeCompare(right.id)
  );
  return config;
}

/** Preserves the selected set across Engine library, detail, and installation routes. */
export function workflowSelectionURL(path: string, ids: string[]): string {
  const params = new URLSearchParams();
  for (const id of [...new Set(ids)]) params.append("workflow", id);
  return `${path}?${params.toString()}`;
}

/** Merges one provider pin without permitting aliases or version drift between workflows. */
function mergeWorkflowDependency(
  combined: Record<string, WorkflowService>,
  identities: Map<string, string>,
  key: string,
  service: WorkflowService
): void {
  const existing = combined[key];
  // Aliases would assign competing connection selectors to one provider identity.
  if (
    identities.has(service.service_id) &&
    identities.get(service.service_id) !== key
  )
    throw new Error(`Conflicting service aliases for ${key}.`);
  // Matching display versions cannot hide a different immutable provider identity.
  if (
    existing &&
    (existing.service_id !== service.service_id ||
      existing.service_version_id !== service.service_version_id ||
      existing.version !== service.version)
  )
    throw new Error(
      `Selected workflows require incompatible versions of ${key}.`
    );
  identities.set(service.service_id, key);
  combined[key] = {
    ...service,
    operations: [
      ...new Set([...(existing?.operations ?? []), ...service.operations]),
    ].sort(),
  };
}

/** Adds workflow dependencies while preserving the app's private routing and previously selected operations. */
function mergeWorkflowServices(
  config: WorkflowAppConfig,
  dependencies: Record<string, WorkflowService>
): void {
  for (const [key, dependency] of Object.entries(dependencies)) {
    const existing = config.services[key];
    // An additive extension must never retarget existing provider calls.
    if (existing && existing.version !== dependency.version)
      throw new Error(`The app uses a different version of ${key}.`);
    config.services[key] = {
      ...existing,
      version: dependency.version,
      operations: [
        ...new Set([...(existing?.operations ?? []), ...dependency.operations]),
      ].sort(),
    };
  }
}

/** Merges one release's methods and provenance without replacing an existing authored definition. */
function mergeWorkflowDefinition(
  config: WorkflowAppConfig,
  sources: Map<string, WorkflowSource>,
  workflow: Workflow
): void {
  for (const [name, operation] of Object.entries(
    workflow.template.unified_operations
  )) {
    const existing = config.unified_operations![name];
    // Exact repeats are idempotent, while changes to the same method require deliberate editing.
    if (existing && canonicalJSON(existing) !== canonicalJSON(operation))
      throw new Error(
        `Unified operation ${name} conflicts with an existing definition.`
      );
    config.unified_operations![name] = structuredClone(operation);
  }
  const source = {
    id: workflow.id,
    version: workflow.template.version,
    hash: workflow.hash,
  };
  const existing = sources.get(source.id);
  // A release ID cannot be rebound to different authoring content by a client.
  if (existing && canonicalJSON(existing) !== canonicalJSON(source))
    throw new Error("Workflow release provenance conflicts with this app.");
  sources.set(source.id, source);
}

/** Enforces the shared installer bound before any dependency aggregation or mutation. */
function validateWorkflowSelectionCount(count: number): void {
  // Empty selections are not installs and larger sets exceed the published composition contract.
  if (count === 0 || count > 32)
    throw new Error("Select between 1 and 32 workflows.");
}
