import { api } from "./api";
import { AppDescriptionClarificationError, decodeUnifiedSource } from "./unified-app-contract";
import { createDescribeScheduler, settleDescribe, matchDescribeEvents, mergeDescribePin, type DescribeProgress, type DescribeSelectionReady, type DescribeSchedule, type AppServicePin, type AppDescription, type DescribeKind, type DescribeServiceCandidate, type ChooseDescribeService } from "./app-describe-contract";

interface IntentService { name: string; endpoint_query: string; endpoint_queries: string[]; select_all_operations: boolean; event_queries: string[] }
interface Intent { clarification: string; action: string; name: string; description: string; language?: string; sequential?: boolean; webhook_requested: boolean; services: IntentService[] }
type Candidate = DescribeServiceCandidate;

/** Resolves every described provider through the same bounded discovery query used by CLI. */
async function resolveServices(services: IntentService[], chooseService?: ChooseDescribeService): Promise<Map<string, Candidate>> {
  const result = await api.graphql<{ serviceCandidatesByRefs: Array<{ ref: string; candidates: Candidate[] }> }>(`query UnifiedAppCandidates($refs: [String!]!, $limitPerRef: Int!) {
    serviceCandidatesByRefs(refs: $refs, limitPerRef: $limitPerRef) { ref candidates { id name slug provider { handle } } }
  }`, { refs: services.map((service) => service.name), limitPerRef: 20 });
  const selected = new Map<string, Candidate>();
  for (const group of result.serviceCandidatesByRefs) {
    const exact = group.candidates.filter((candidate) => [candidate.slug, candidate.name, `@${candidate.provider?.handle}/${candidate.slug}`].some((name) => name.toLowerCase() === group.ref.toLowerCase()));
    // Prefer a unique exact match; fuzzy discovery must never silently choose between providers.
    const matches = exact.length ? exact : group.candidates;
    // A discovery miss needs a different service name, not a provider selection.
    if (!matches.length) throw new Error(`No service found for “${group.ref}”. Check the name or import the service into Registry.`);
    let candidate = matches[0];
    // Pause the same parsed request until the user resolves the publisher ambiguity.
    if (matches.length > 1) {
      // Non-interactive callers still receive concrete canonical references to disambiguate their goal.
      if (!chooseService) throw new Error(`Choose a service for “${group.ref}”: ${matches.map((match) => `@${match.provider?.handle}/${match.slug}`).join(", ")}.`);
      const selectedID = await chooseService(group.ref, matches);
      const chosen = matches.find((match) => match.id === selectedID);
      // A stale or fabricated selection must never expand the discovered candidate set.
      if (!chosen) throw new Error(`The selected service is no longer available for “${group.ref}”.`);
      candidate = chosen;
    }
    selected.set(group.ref, candidate);
  }
  return selected;
}

/** Grounds operations and inbound events in one immutable provider version. */
async function resolveOperations(intent: IntentService, candidate: Candidate, kind: DescribeKind, includeEvents: boolean, schedule: DescribeSchedule, operationDone: (name: string) => void): Promise<[string, AppServicePin]> {
  const result = await schedule(() => api.graphql<{ service: { service_versions: Array<{ id: string; name: string; status: string }> } | null }>(`query UnifiedAppServiceVersion($id: String!) {
    service(id: $id) { service_versions(limit: 1) { id name status } }
  }`, { id: candidate.id }));
  const version = result.service?.service_versions[0];
  // A missing or retired version cannot be guessed from the model's memory.
  if (!version || version.status === "deprecated") throw new Error(`No active version is available for ${intent.name}.`);
  const queries = operationQueries(intent, kind, includeEvents);
  const operations = await settleDescribe(queries.map(async (query) => {
    // Validate within the shared slot so a classifier miss also stops queued paid requests.
    const name = await schedule(async () => {
      const classified = await api.mcpGraphql<{ classifyPromptOperation: string }>(`query ClassifyPromptOperation($service_id: String!, $version: String!, $query: String!) {
      classifyPromptOperation(service_id: $service_id, version: $version, query: $query)
    }`, { service_id: candidate.id, version: version.name, query });
      // A classifier miss must stop before source generation or any mutation.
      if (!classified.classifyPromptOperation) throw new Error(`No matching operation found for ${intent.name}: ${query}`);
      return classified.classifyPromptOperation;
    });
    operationDone(name);
    return name;
  }));
  // Qualified keys retain the publisher identity used to select the contract.
  if (!candidate.provider?.handle) throw new Error(`The service ${intent.name} has no canonical provider identity.`);
  const pin: AppServicePin = { service_id: candidate.id, service_version_id: version.id, version: version.name, operations: [...new Set(operations)] };
  await schedule(() => resolveAdditionalCapabilities(pin, intent, includeEvents));
  return [`@${candidate.provider.handle}/${candidate.slug}`, pin];
}

/** Resolves the same read-only proposal as CLI describe for the explicitly chosen output. */
export async function describeApp(goal: string, kind: DescribeKind, progress: DescribeProgress, chooseService?: ChooseDescribeService, selectionReady?: DescribeSelectionReady): Promise<AppDescription> {
  // Bounds protect paid parsing without discarding the user's editable text.
  if (!goal.trim() || new TextEncoder().encode(goal).length > 16384) throw new Error("Describe your app in 1 to 16,384 bytes.");
  progress("Understanding your app…", "intent");
  const { parseSDKIntent: intent } = await api.graphql<{ parseSDKIntent: Intent }>(`query ParsePromptIntent($q: String!, $appContext: String!) {
    parseSDKIntent(q: $q, appContext: $appContext) { clarification action name description language sequential webhook_requested services { name endpoint_query endpoint_queries select_all_operations event_queries } }
  }`, { q: goal.trim(), appContext: "" });
  validateIntent(intent, kind);
  progress("Finding matching services…", "services");
  const candidates = await resolveServices(intent.services, chooseService);
  const services: Record<string, AppServicePin> = {};
  const explicitEvents = intent.services.some((service) => service.event_queries?.length);
  const schedule = createDescribeScheduler();
  let completed = 0;
  const total = intent.services.reduce((count, service) => count + intentOperationQueries(service).length, 0);
  progress(`Selecting operations for ${[...candidates.values()].map((candidate) => candidate.name).join(", ")}…`, "operations");
  /** Counts only grounded operation matches, never elapsed-time estimates. */
  function operationDone(name: string) { completed++; progress(`Selected ${name} · ${completed} of ${total} operations`, "operations"); }
  const resolved = await settleDescribe(intent.services.map(async (service) => {
    const candidate = candidates.get(service.name);
    // Missing discovery results cannot fall through to an ungrounded proposal.
    if (!candidate) throw new Error(`Service ${service.name} was not found.`);
    // General inbound goals cover all named services only when none has narrower event intent, matching CLI.
    const includeEvents = Boolean(service.event_queries?.length || (intent.webhook_requested && !explicitEvents));
    return resolveOperations(service, candidate, kind, includeEvents, schedule, operationDone);
  }));
  for (const [key, pin] of resolved) services[key] = mergeDescribePin(services[key], pin);
  const proposal: AppDescription = { name: intent.name, description: intent.description || goal.trim(), services, language: intent.language };
  selectionReady?.(proposal);
  // Only hosted orchestration needs source generation; SDK and MCP expose the reviewed capabilities directly.
  if (kind === "unified_app") proposal.source = await draftAppSource(goal, services, progress);
  progress("Preparing your app for review…", "review");
  return proposal;
}

/** Drafts or revises source using only the reviewed immutable operation contracts. */
export async function draftAppSource(goal: string, services: Record<string, AppServicePin>, progress: DescribeProgress, source?: string): Promise<string> {
  // Bound change requests and source independently before sending either to the drafting service.
  if (!goal.trim() || new TextEncoder().encode(goal).length > 16384) throw new Error("Describe your change in 1 to 16,384 bytes.");
  if (source !== undefined && (!source.trim() || new TextEncoder().encode(source).length > 128 * 1024)) throw new Error("App source must contain 1 to 128 KiB of TypeScript.");
  const selections = Object.entries(services).flatMap(([service, pin]) => [
    ...pin.operations.map((operation) => ({ service, service_id: pin.service_id, version: pin.version, operation })),
    ...(pin.webhooks ?? []).map((event) => ({ service, service_id: pin.service_id, version: pin.version, event })),
  ]);
  // The Registry drafter sees only exact selected contracts, including inbound payload evidence.
  if (!selections.length || selections.length > 16 || Object.values(services).some((pin) => pin.select_all)) throw new Error("Unified Apps require 1 to 16 specific operations or webhook events.");
  // Distinguish revising reviewed code from creating the first source draft.
  progress(source === undefined ? "Generating TypeScript from your selected operations and events…" : "Revising your app against the selected provider contracts…", "source");
  // Existing creation requests remain compatible with Registry versions that predate source revision.
  const result = source === undefined
    ? await api.graphql<{ draftPromptUnifiedApp: string }>(`query DraftPromptUnifiedApp($q: String!, $selections: String!) {
      draftPromptUnifiedApp(q: $q, selections: $selections)
    }`, { q: goal.trim(), selections: JSON.stringify(selections) })
    : await api.graphql<{ draftPromptUnifiedApp: string }>(`query RevisePromptUnifiedApp($q: String!, $selections: String!, $source: String!) {
      draftPromptUnifiedApp(q: $q, selections: $selections, source: $source)
    }`, { q: goal.trim(), selections: JSON.stringify(selections), source });
  return decodeUnifiedSource(result.draftPromptUnifiedApp);
}

/** Resolves inbound event names against an immutable catalogue using the CLI's deterministic matching policy. */
async function resolveEvents(id: string, version: string, queries: string[]): Promise<string[]> {
  const result = await api.graphql<{ service: { webhooks: Array<{ name: string; description: string }> } | null }>(`query DescribeAppEvents($id: String!, $version: String!) {
    service(id: $id, version: $version) { webhooks { name description } }
  }`, { id, version });
  return matchDescribeEvents(queries, result.service?.webhooks ?? []);
}

/** Preserves explicit all-operation and event-only requests without widening missing intent. */
function operationQueries(intent: IntentService, kind: DescribeKind, includeEvents: boolean): string[] {
  // Hosted source rejects changing operation catalogues while accepting exact inbound events.
  validateHostedSelection(kind, Boolean(intent.select_all_operations));
  // Plural operation intent is authoritative; the singular field is retained for older Registry responses.
  const queries = intentOperationQueries(intent);
  // Contradictory model output must not silently broaden a specific request.
  if (intent.select_all_operations && queries.length) throw new Error(`Choose specific operations or all operations for ${intent.name}.`);
  // Event-only and explicit all-operation requests legitimately have no classifier queries.
  if (!queries.length && (includeEvents || intent.select_all_operations)) return [];
  // Missing capability intent is a clarification, never an implicit select-all grant.
  if (!queries.length || queries.length > 16 || queries.some((query) => !query?.trim())) throw new Error(`Describe 1 to 16 specific operations for ${intent.name}, or explicitly request all operations.`);
  return queries;
}

/** Rejects unsupported authoring intent before discovery or model classification. */
function validateIntent(intent: Intent, kind: DescribeKind): void {
  // Clarifications take precedence over a partially filled proposal.
  if (intent.clarification?.trim()) throw new AppDescriptionClarificationError(intent.clarification);
  // The creation form cannot reinterpret updates as new apps.
  if (intent.action === "update") throw new Error("Describe a new app. Use the existing app's editor to create a new version.");
  validateAdapterIntent(intent, kind);
  // Bound discovery before issuing any classification requests.
  if (!intent.services?.length || intent.services.length > 16) throw new Error("Name 1 to 16 services in your description.");

}

/** Adds only explicitly requested all-operation or inbound capabilities to an already pinned contract. */
async function resolveAdditionalCapabilities(pin: AppServicePin, intent: IntentService, includeEvents: boolean): Promise<void> {
  // Select-all remains explicit rather than inferred from a classifier miss.
  if (intent.select_all_operations) pin.select_all = true;
  // Event-only requests resolve the immutable inbound catalogue without granting operations.
  if (includeEvents) pin.webhooks = await resolveEvents(pin.service_id, pin.version, intent.event_queries ?? []);
}

/** Reads plural intent first while retaining the older singular Registry response. */
function intentOperationQueries(intent: IntentService): string[] {
  // Plural intent separates capabilities; singular text must not duplicate those requests.
  if (intent.endpoint_queries?.length) return intent.endpoint_queries;
  return intent.endpoint_query ? [intent.endpoint_query] : [];
}

/** Applies adapter-specific constraints without changing the explicitly selected output kind. */
function validateAdapterIntent(intent: Intent, kind: DescribeKind): void {
  // Ordered execution needs authored orchestration instead of independent callable capabilities.
  if (intent.sequential && kind !== "unified_app") throw new Error("Sequential workflows require a Unified App.");
  const inbound = intent.webhook_requested || intent.services?.some((service) => service.event_queries?.length);
  // Direct REST execution has no receiver, while Unified Apps use the hosted trigger worker.
  if (kind === "api" && inbound) throw new Error("Use a Unified App, SDK, or MCP app to receive webhook events.");
  // The UI must not silently translate a requested emitter it cannot build.
  if (["sdk", "app"].includes(kind) && intent.language && !["typescript", "python"].includes(intent.language)) throw new Error("Choose TypeScript or Python for the SDK.");
}

/** Prevents wildcard operation grants from entering hosted source drafting. */
function validateHostedSelection(kind: DescribeKind, unsupported: boolean): void {
  // SDK and MCP can follow an evolving operation catalogue; hosted source needs exact bindings.
  if (kind === "unified_app" && unsupported) throw new Error("Unified Apps require specific operations.");
}
