import {
  Fixture,
  FixtureOperation,
  FixtureParameter,
  FixtureRequestContent,
  FixtureResponseContract,
  FixtureUnifiedOperation,
  FixtureUnifiedTarget,
} from "./fixture.js";
import { weightedIntentScore } from "./fuzzyMatch.js";
import { DOCUMENTATION_OUTPUT_POLICY } from "./outputLimits.js";
import { appendPointer, resolveJsonPointer } from "./jsonPointer.js";

export const SEARCH_DOCS_MAX_BYTES = DOCUMENTATION_OUTPUT_POLICY.maxBytes;
export const SEARCH_DOCS_DEFAULT_LIMIT = 3;
export const SEARCH_DOCS_MAX_LIMIT = 5;
export const SEARCH_DOCS_LIST_LIMIT = 20;

export type DocumentationSection = "parameters" | "request" | `response:${string}` | "input" | "targets" | "output" | "definitions";

/** Admits only bounded public section names, including exact physical response statuses. */
export function isDocumentationSection(value: unknown): value is DocumentationSection {
  // Tool input must be a string before any bounded name checks are attempted.
  if (typeof value !== "string") {
    return false;
  }
  // Fixed sections cover request and Unified contracts without accepting private namespaces.
  if (["parameters", "request", "input", "targets", "output", "definitions"].includes(value)) {
    return true;
  }
  // A bounded status suffix permits exact response retrieval without accepting arbitrary private namespaces.
  return /^response:[^:/\s]{1,32}$/.test(value);
}

/** Bare callable identity used by bounded schema-free list mode. */
export interface OperationSummary {
  kind?: "unified";
  operation_id: string;
  method?: string;
  path?: string;
  description: string;
  description_truncated?: true;
}

/** Explains exactly which complete schemas are present and retrievable. */
export interface SchemaStatus {
  complete: boolean;
  included_sections: DocumentationSection[];
  available_sections: DocumentationSection[];
}

/** Physical callable detail preserves the reviewed request and response contracts. */
export interface OperationDetail extends OperationSummary {
  pagination: PaginationGuidance;
  parameters?: FixtureParameter[];
  request_content?: FixtureRequestContent | null;
  responses?: Record<string, FixtureResponseContract>;
  schema_status: SchemaStatus;
}

/** Exact pagination guidance exposes caller policy only after identity is resolved. */
type ExactPaginationGuidance =
  | { supported: false; caller_bound_supported: false; usage: string }
  | { supported: true; caller_bound_supported: false; engine_max_pages?: number; usage: string }
  | { supported: true; caller_bound_supported: true; engine_max_pages: number; usage: string };

/** Ranked pagination guidance withholds every caller-bound field until exact lookup. */
type QueryPaginationGuidance =
  | { supported: false; usage: string }
  | { supported: true; exact_lookup_required: true; usage: string };

/** Explains whether exact discovery is required before changing a physical call shape. */
export type PaginationGuidance = ExactPaginationGuidance | QueryPaginationGuidance;

/** Unified detail exposes only the public compiler descriptor. */
export interface UnifiedOperationDetail extends OperationSummary {
  kind: "unified";
  input_schema?: unknown;
  output_schema?: unknown;
  targets?: UnifiedTargetDetail[];
  schema_status: SchemaStatus;
}

/** Unified targets deliberately exclude Engine identities and private mappings. */
export type UnifiedTargetDetail = Pick<FixtureUnifiedTarget, "public_target" | "service_target" | "operation_id" | "depends_on" | "rollback" | "output_schema">;

export interface SearchDocsArgs {
  query?: string;
  operationId?: string;
  limit?: number;
  section?: DocumentationSection;
  schemaPath?: string;
}

export type SearchDocsResult =
  | { mode: "list"; operations: OperationSummary[]; total: number; truncated: boolean }
  | { mode: "query"; operations: Array<OperationDetail | UnifiedOperationDetail>; total: number; truncated: boolean }
  | { mode: "operationId"; operation: OperationDetail | UnifiedOperationDetail }
  | { mode: "operationId" | "section"; error: string }
  | {
      mode: "section";
      operation_id: string;
      section: DocumentationSection;
      schema_path: string;
      schema_status: SchemaStatus;
      value?: unknown;
      available_schema_paths?: string[];
      paths_truncated?: boolean;
    };

interface DocumentationSectionValue {
  name: DocumentationSection;
  priority: number;
  fragment: Record<string, unknown>;
  value: unknown;
}

interface SearchCandidate {
  summary: OperationSummary;
  detail_metadata?: { path: string; pagination: ExactPaginationGuidance; query_pagination: QueryPaginationGuidance };
  search_aliases: string[];
  score: number;
  sections: DocumentationSectionValue[];
}

const MAX_DESCRIPTION_BYTES = 512;
const MAX_CHILD_PATHS = 32;
const MANUAL_GET_PAGINATION_USAGE = "Only when this operation's documentation explicitly identifies a page input, continuation output, and stop condition, pass the page input in params, await the two-argument call(), read the continuation from its result, and repeat until the documented stop condition. Never infer page, cursor, or offset semantics from names. Each page counts toward execute's call and time limits.";

/** Routes exact, lazy, intent, and list requests without changing call authorization. */
export function searchDocs(fixture: Fixture, args: SearchDocsArgs): SearchDocsResult {
  // Lazy requests require both an exact callable and a named public section.
  if (args.section !== undefined || args.schemaPath !== undefined) {
    return resolveSection(fixture, args);
  }
  // Exact identity remains more specific than any simultaneously supplied query.
  if (args.operationId) {
    return resolveOperationDetail(fixture, args.operationId);
  }
  // Whitespace carries no lexical intent and therefore selects schema-free list mode.
  if (args.query?.trim()) {
    return queryOperations(fixture, args.query, normalizeLimit(args.limit));
  }
  return listOperations(fixture, normalizeListLimit(args.limit));
}

/** Clamps direct and tool callers to the same small ranking window. */
function normalizeLimit(limit: number | undefined): number {
  // Non-finite and absent values use the documented default instead of leaking odd slice behavior.
  if (limit === undefined || !Number.isFinite(limit)) {
    return SEARCH_DOCS_DEFAULT_LIMIT;
  }
  return Math.max(1, Math.min(SEARCH_DOCS_MAX_LIMIT, Math.floor(limit)));
}

/** Keeps list mode useful for catalogue orientation without sharing query's tighter top-K. */
function normalizeListLimit(limit: number | undefined): number {
  // An omitted list limit uses a larger schema-free window while remaining bounded.
  if (limit === undefined || !Number.isFinite(limit)) {
    return SEARCH_DOCS_LIST_LIMIT;
  }
  return Math.max(1, Math.min(SEARCH_DOCS_LIST_LIMIT, Math.floor(limit)));
}

/** Returns a bounded schema-free catalogue window with exact truncation metadata. */
function listOperations(fixture: Fixture, limit: number): SearchDocsResult {
  const summaries = [...fixture.operations.map(physicalSummary), ...fixture.unifiedOperations.map(unifiedSummary)];
  const operations: OperationSummary[] = [];
  for (const summary of summaries.slice(0, limit)) {
    const trial = { mode: "list" as const, operations: [...operations, summary], total: summaries.length, truncated: true };
    // A summary is admitted whole so even non-schema metadata is never silently clipped.
    if (!fitsBudget(trial)) {
      break;
    }
    operations.push(summary);
  }
  return {
    mode: "list",
    operations,
    total: summaries.length,
    truncated: operations.length < summaries.length,
  };
}

/** Ranks physical and Unified callables together, then packs complete sections by priority. */
function queryOperations(fixture: Fixture, query: string, limit: number): SearchDocsResult {
  const matched = allCandidates(fixture)
    .map((candidate) => ({ ...candidate, score: scoreCandidate(query, candidate) }))
    .filter(({ score }) => score > 0)
    .sort(compareCandidates);
  const selected = matched.slice(0, limit);
  let operations = selected.map((candidate) => toDetailShell(candidate, "query"));
  // Metadata and all ranked summaries are reserved before any schema consumes the shared budget.
  while (!fitsBudget({ mode: "query", operations, total: matched.length, truncated: true })) {
    operations = operations.slice(0, -1);
  }
  operations = packSections(operations, selected.slice(0, operations.length), "query", matched.length);
  return {
    mode: "query",
    operations,
    total: matched.length,
    truncated: operations.length < matched.length,
  };
}

/** Resolves an exact callable and packs as many whole sections as the semantic budget admits. */
function resolveOperationDetail(fixture: Fixture, operationId: string): SearchDocsResult {
  const candidate = exactCandidate(fixture, operationId);
  // Exact lookup never falls back to fuzzy matching because call() uses the same identity invariant.
  if (!candidate) {
    return { mode: "operationId", error: "no such operationId" };
  }
  let operation = toDetailShell(candidate, "operationId");
  // The fixture admission bounds public identity metadata, but fail closed if that invariant changes.
  if (!fitsBudget({ mode: "operationId", operation })) {
    return { mode: "operationId", error: "operation metadata exceeds the search_docs result budget" };
  }
  [operation] = packSections([operation], [candidate], "operationId", 1);
  return { mode: "operationId", operation };
}

/** Packs request-side sections globally before response-side documentation. */
function packSections(
  operations: Array<OperationDetail | UnifiedOperationDetail>,
  candidates: SearchCandidate[],
  mode: "query" | "operationId",
  total: number,
): Array<OperationDetail | UnifiedOperationDetail> {
  let packed = operations;
  const attempts = sectionPackingOrder(candidates);
  for (const { index, section } of attempts) {
    const trial = replaceAt(packed, index, includeSection(packed[index], section));
    // Budget checks include the actual mode wrapper because its metadata also consumes UTF-8 bytes.
    const result = mode === "query"
      ? { mode, operations: trial, total, truncated: trial.length < total }
      : { mode, operation: trial[0] };
    // Sections are indivisible so omitted schemas remain available through lazy retrieval.
    if (fitsBudget(result)) {
      packed = trial;
    }
  }
  return packed.map(finalizeStatus);
}

/** Orders every section globally so all request-side detail precedes any response detail. */
function sectionPackingOrder(candidates: SearchCandidate[]): Array<{ index: number; section: DocumentationSectionValue }> {
  const attempts: Array<{ index: number; section: DocumentationSectionValue }> = [];
  for (const priority of [0, 1, 2]) {
    for (let index = 0; index < candidates.length; index++) {
      for (const section of candidates[index].sections) {
        // Only the active phase is appended, preserving authored order within equal priorities.
        if (section.priority === priority) {
          attempts.push({ index, section });
        }
      }
    }
  }
  return attempts;
}

/** Replaces one detail immutably so failed budget trials cannot mutate admitted output. */
function replaceAt(
  operations: Array<OperationDetail | UnifiedOperationDetail>,
  index: number,
  operation: OperationDetail | UnifiedOperationDetail,
): Array<OperationDetail | UnifiedOperationDetail> {
  // Only the selected ranked slot changes; every other reserved summary stays intact.
  return operations.map((current, currentIndex) => currentIndex === index ? operation : current);
}

/** Adds one entire public section and records its exact inclusion. */
function includeSection(
  operation: OperationDetail | UnifiedOperationDetail,
  section: DocumentationSectionValue,
): OperationDetail | UnifiedOperationDetail {
  const mergedFragment = mergeSectionFragment(operation, section.fragment);
  return {
    ...operation,
    ...mergedFragment,
    schema_status: {
      ...operation.schema_status,
      included_sections: [...operation.schema_status.included_sections, section.name],
    },
  };
}

/** Merges independently retrievable response statuses without overwriting earlier statuses. */
function mergeSectionFragment(
  operation: OperationDetail | UnifiedOperationDetail,
  fragment: Record<string, unknown>,
): Record<string, unknown> {
  // Response-status fragments share one public responses object in callable detail.
  if ("responses" in fragment) {
    const existing = "responses" in operation ? operation.responses ?? {} : {};
    return { responses: { ...existing, ...(fragment.responses as Record<string, FixtureResponseContract>) } };
  }
  return fragment;
}

/** Marks a detail complete only when every advertised section is present whole. */
function finalizeStatus(operation: OperationDetail | UnifiedOperationDetail): OperationDetail | UnifiedOperationDetail {
  return {
    ...operation,
    schema_status: {
      ...operation.schema_status,
      complete: operation.schema_status.included_sections.length === operation.schema_status.available_sections.length,
    },
  };
}

/** Retrieves one safe public section or JSON Pointer subtree without returning partial values. */
function resolveSection(fixture: Fixture, args: SearchDocsArgs): SearchDocsResult {
  // Both fields are required because a section name is meaningful only within one exact callable.
  if (!args.operationId || !args.section) {
    return { mode: "section", error: "operationId and section are required for section mode" };
  }
  const candidate = exactCandidate(fixture, args.operationId);
  // Section retrieval retains the same exact, collision-free identity rule as detail mode.
  if (!candidate) {
    return { mode: "section", error: "no such operationId" };
  }
  const section = candidate.sections.find(({ name }) => name === args.section);
  // Cross-kind section names are rejected instead of being guessed or remapped.
  if (!section) {
    return { mode: "section", error: "section is not available for this operation" };
  }
  const schemaPath = args.schemaPath ?? "";
  const resolved = resolveJsonPointer(section.value, schemaPath);
  // Invalid or missing paths return a stable error without echoing fixture data.
  if (resolved.error) {
    return { mode: "section", error: resolved.error };
  }
  return boundedSectionResult(candidate.summary.operation_id, section.name, schemaPath, resolved.value);
}

/** Returns a whole selected value when possible, otherwise deterministic child pointers. */
function boundedSectionResult(
  operationId: string,
  section: DocumentationSection,
  schemaPath: string,
  value: unknown,
): SearchDocsResult {
  const included: SchemaStatus = { complete: true, included_sections: [section], available_sections: [section] };
  const complete = { mode: "section" as const, operation_id: operationId, section, schema_path: schemaPath, schema_status: included, value };
  // Exact values are returned only when their complete JSON representation fits.
  if (fitsBudget(complete)) {
    return complete;
  }
  const omitted: SchemaStatus = { complete: false, included_sections: [], available_sections: [section] };
  const paths = childPointers(value, schemaPath);
  let available: string[] = [];
  for (const path of paths.slice(0, MAX_CHILD_PATHS)) {
    const trial = { mode: "section" as const, operation_id: operationId, section, schema_path: schemaPath, schema_status: omitted, available_schema_paths: [...available, path], paths_truncated: true };
    // Child pointers remain exact and are omitted whole if even their metadata would overflow.
    if (!fitsBudget(trial)) {
      break;
    }
    available.push(path);
  }
  return {
    mode: "section",
    operation_id: operationId,
    section,
    schema_path: schemaPath,
    schema_status: omitted,
    available_schema_paths: available,
    paths_truncated: available.length < paths.length,
  };
}

/** Lists deterministic immediate child pointers for iterative retrieval of oversized values. */
function childPointers(value: unknown, base: string): string[] {
  // Arrays expose their stable numeric positions in document order.
  if (Array.isArray(value)) {
    return value.map((_, index) => appendPointer(base, String(index)));
  }
  // Object keys are sorted so identical fixtures always return identical navigation hints.
  if (value !== null && typeof value === "object") {
    return Object.keys(value as Record<string, unknown>).sort().map((key) => appendPointer(base, key));
  }
  return [];
}

/** Builds all local candidates without database, network, or private mapping access. */
function allCandidates(fixture: Fixture): SearchCandidate[] {
  return [
    ...fixture.operations.map((operation) => toPhysicalCandidate(operation, fixture)),
    ...fixture.unifiedOperations.map(toUnifiedCandidate),
  ];
}

/** Resolves one exact public candidate across collision-free fixture indexes. */
function exactCandidate(fixture: Fixture, operationId: string): SearchCandidate | undefined {
  const physical = fixture.resolve(operationId);
  // Physical lookup remains first because fixture admission prevents cross-kind collisions.
  if (physical) {
    return toPhysicalCandidate(physical, fixture);
  }
  const unified = fixture.resolveUnified(operationId);
  // A missing logical name remains absent rather than becoming a fuzzy fallback.
  return unified ? toUnifiedCandidate(unified) : undefined;
}

/** Projects one physical operation into independently packable request and response sections. */
function toPhysicalCandidate(operation: FixtureOperation, fixture: Fixture): SearchCandidate {
  const summary = physicalSummary(operation);
  const pagination = paginationGuidance(operation);
  return {
    summary,
    detail_metadata: { path: operation.path, pagination, query_pagination: queryPaginationGuidance(operation, pagination) },
    search_aliases: [operation.name],
    score: 0,
    sections: [
      {
        name: "parameters",
        priority: 0,
        fragment: { parameters: operation.parameters ?? [] },
        value: operation.parameters ?? [],
      },
      {
        name: "request",
        priority: 0,
        fragment: { request_content: operation.request_content ?? null },
        value: operation.request_content ?? null,
      },
      ...Object.keys(operation.responses).sort().map((status) => ({
        name: `response:${status}` as const,
        priority: 2,
        fragment: { responses: { [status]: operation.responses[status] } },
        value: operation.responses[status],
      })),
      ...definitionSections(operation, fixture),
    ],
  };
}

/** Converts the effective fixture policy into one stable, provider-neutral call instruction. */
function paginationGuidance(operation: FixtureOperation): ExactPaginationGuidance {
  const pagination = operation.pagination;
  const operationId = JSON.stringify(operation.operation_id);
  const unboundedCall = `call(${operationId}, params)`;
  // Unsupported operations need manual-provider guidance without exposing a decorative Engine bound.
  if (!pagination.supported) {
    // GET is the only generic manual-paging case; every method still receives the exact safe call correction.
    const methodUsage = operation.method.toUpperCase() === "GET"
      ? `This GET operation has no Engine pagination contract, so each ${unboundedCall} makes one provider request and does not traverse provider pages. ${MANUAL_GET_PAGINATION_USAGE}`
      : "This operation has no Engine pagination contract.";
    const usage = `${methodUsage} Use ${unboundedCall} without a pagination option. A pagination bound documented for another operation must not be reused here.`;
    return { supported: false, caller_bound_supported: false, usage };
  }
  // A one-page Engine policy has no legal positive strict reduction for the caller.
  if (!pagination.caller_bound_supported) {
    return {
      supported: true,
      caller_bound_supported: false,
      engine_max_pages: pagination.engine_max_pages,
      usage: `Engine follows operation ${operationId}'s reviewed pagination contract up to engine_max_pages, but no positive maxPages value is lower than that limit. Use ${unboundedCall} without a pagination option. A pagination bound documented for another operation must not be reused here.`,
    };
  }
  return {
    supported: true,
    caller_bound_supported: true,
    // Fixture admission proves every boundable policy carries its positive Engine limit.
    engine_max_pages: pagination.engine_max_pages!,
    usage: `Engine follows provider pagination automatically for operation ${operationId}. To accept a partial result from this exact operation, pass { pagination: { maxPages: N } } as call()'s third argument, where N is a positive integer lower than engine_max_pages. This bound applies only to operation ${operationId} and must not be reused for another call. Provider page-size parameters do not bound total Engine traversal.`,
  };
}

/** Removes reusable numeric bounds from ranked results while retaining an exact next discovery action. */
function queryPaginationGuidance(operation: FixtureOperation, exact: ExactPaginationGuidance): QueryPaginationGuidance {
  // Unsupported operations have no bound to infer and retain only their exact safe two-argument denial.
  if (!exact.supported) {
    return { supported: false, usage: exact.usage };
  }
  const operationId = JSON.stringify(operation.operation_id);
  // Every supported ranked result uses one uniform exact-lookup boundary, including policies that ultimately allow no caller bound.
  return {
    supported: true,
    exact_lookup_required: true,
    usage: `Operation ${operationId} supports Engine pagination, but ranked query results do not expose its caller policy. Until exact detail is retrieved, use only call(${operationId}, params). Call search_docs with operationId ${operationId} to retrieve the exact pagination guidance. A policy from another operation must not be reused.`,
  };
}

/** Makes shared definitions lazily navigable through the existing bounded section API, never per-operation copies. */
function definitionSections(operation: FixtureOperation, fixture: Fixture): DocumentationSectionValue[] {
  const definitions = fixture.schemaDefinitions[operation.service_version_id ?? ""];
  // Standalone legacy operations do not advertise a dictionary that cannot be retrieved.
  if (!definitions) {
    return [];
  }
  // Priority 3 is intentionally outside automatic packing: callers retrieve only needed definitions by JSON Pointer.
  return [{ name: "definitions", priority: 3, fragment: {}, value: definitions }];
}

/** Projects one Unified descriptor without compiler mappings or Engine identities. */
function toUnifiedCandidate(operation: FixtureUnifiedOperation): SearchCandidate {
  const targets = operation.targets.map(toUnifiedTargetDetail);
  const sections: DocumentationSectionValue[] = [
    { name: "input", priority: 0, fragment: { input_schema: operation.input_schema }, value: operation.input_schema },
    { name: "targets", priority: 1, fragment: { targets }, value: targets },
  ];
  // Absent output schemas are not advertised as retrievable documentation.
  if (operation.output_schema !== undefined) {
    sections.push({ name: "output", priority: 2, fragment: { output_schema: operation.output_schema }, value: operation.output_schema });
  }
  return {
    summary: unifiedSummary(operation),
    search_aliases: operation.targets.flatMap((target) => [target.public_target, target.service_target, target.operation_id].filter((value): value is string => value !== undefined)),
    score: 0,
    sections,
  };
}

/** Converts a candidate to a schema-free callable shell with mode-appropriate execution metadata. */
function toDetailShell(candidate: SearchCandidate, mode: "query" | "operationId"): OperationDetail | UnifiedOperationDetail {
  const schemaStatus: SchemaStatus = {
    complete: candidate.sections.length === 0,
    included_sections: [],
    available_sections: candidate.sections.map(({ name }) => name),
  };
  // Unified candidates have no physical path or pagination contract to project.
  if (candidate.summary.kind === "unified") {
    return { ...candidate.summary, kind: "unified", schema_status: schemaStatus };
  }
  // Candidate construction guarantees physical metadata before detail can be returned.
  if (!candidate.detail_metadata) {
    throw new Error("physical search candidate missing execution metadata");
  }
  const { path, pagination, query_pagination: queryPagination } = candidate.detail_metadata;
  // Ranked results withhold reusable caller bounds; exact lookup remains the authoritative numeric policy.
  return { ...candidate.summary, path, pagination: mode === "query" ? queryPagination : pagination, schema_status: schemaStatus };
}

/** Scores public physical and Unified metadata with identity weighted most strongly. */
function scoreCandidate(query: string, candidate: SearchCandidate): number {
  return weightedIntentScore(query, [
    { value: candidate.summary.operation_id, weight: 8 },
    { value: candidate.summary.description, weight: 4 },
    { value: candidate.detail_metadata?.path, weight: 3 },
    { value: candidate.summary.method, weight: 1 },
    ...candidate.search_aliases.map((value) => ({ value, weight: 3 })),
  ]);
}

/** Orders by lexical evidence and then exact public identity for stable ties. */
function compareCandidates(left: SearchCandidate, right: SearchCandidate): number {
  // Higher evidence ranks first; ties must not depend on fixture or sort implementation order.
  if (left.score !== right.score) {
    return right.score - left.score;
  }
  // Exact public IDs provide a locale-independent tie breaker across both operation kinds.
  return left.summary.operation_id < right.summary.operation_id ? -1 : left.summary.operation_id > right.summary.operation_id ? 1 : 0;
}

/** Builds a bounded physical summary while retaining the exact callable ID. */
function physicalSummary(operation: FixtureOperation): OperationSummary {
  return {
    operation_id: operation.operation_id,
    method: operation.method,
    ...boundedDescription(operation.description ?? operation.name),
  };
}

/** Builds a bounded Unified summary under its exact authored operation name. */
function unifiedSummary(operation: FixtureUnifiedOperation): OperationSummary {
  return { kind: "unified", operation_id: operation.name, ...boundedDescription(operation.description ?? operation.name) };
}

/** Bounds prose by UTF-8 bytes and marks the only intentionally shortened field. */
function boundedDescription(description: string): Pick<OperationSummary, "description" | "description_truncated"> {
  // Most descriptions fit unchanged, preserving authored wording and avoiding allocation.
  if (Buffer.byteLength(description, "utf8") <= MAX_DESCRIPTION_BYTES) {
    return { description };
  }
  let result = "";
  for (const character of description) {
    // The ellipsis is reserved before admission so the final marker also fits the bound.
    if (Buffer.byteLength(`${result}${character}…`, "utf8") > MAX_DESCRIPTION_BYTES) {
      break;
    }
    result += character;
  }
  return { description: `${result}…`, description_truncated: true };
}

/** Projects only caller-relevant target names, dependencies, rollback, and schemas. */
function toUnifiedTargetDetail(target: FixtureUnifiedTarget): UnifiedTargetDetail {
  // Rollback metadata is projected explicitly so private identities cannot ride along with its public ID.
  return {
    public_target: target.public_target,
    service_target: target.service_target,
    operation_id: target.operation_id,
    depends_on: target.depends_on,
    rollback: target.rollback ? { operation_id: target.rollback.operation_id } : undefined,
    output_schema: target.output_schema,
  };
}

/** Measures the exact serialized UTF-8 result rather than JavaScript code units. */
function fitsBudget(value: unknown): boolean {
  return Buffer.byteLength(JSON.stringify(value), "utf8") <= SEARCH_DOCS_MAX_BYTES;
}
