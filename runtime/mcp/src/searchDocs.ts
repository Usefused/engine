import {
  Fixture,
  FixtureOperation,
  FixtureParameter,
  FixtureRequestContent,
  FixtureResponseContract,
  FixtureUnifiedOperation,
  FixtureUnifiedTarget,
} from "./fixture.js";
import { bestScore } from "./fuzzyMatch.js";

/**
 * Bare identifier returned by list/query mode. Deliberately schema-free --
 * per sprint/lighter_mcp_runtime_design.md's Discovery section, a summary
 * with partial schema costs tokens without being safe to write a call()
 * against, so these two modes stay minimal on purpose rather than
 * "helpfully" including a schema fragment.
 */
export interface OperationSummary {
  kind?: "unified";
  operation_id: string;
  method?: string;
  description: string;
}

/** Full detail returned by operationId mode -- the only mode that pays the
 * token cost of a schema, and only for the operation actually asked for. */
export interface OperationDetail {
  operation_id: string;
  method: string;
  path: string;
  description?: string;
  parameters: FixtureParameter[];
  request_content?: FixtureRequestContent | null;
  responses: Record<string, FixtureResponseContract>;
}

/** Unified detail exposes public graph structure without Engine identities or mappings. */
export interface UnifiedOperationDetail {
  kind: "unified";
  operation_id: string;
  description?: string;
  input_schema: unknown;
  output_schema?: unknown;
  targets: UnifiedTargetDetail[];
}

/** Unified target detail is the explicitly safe subset projected to callers. */
export type UnifiedTargetDetail = Pick<FixtureUnifiedTarget, "public_target" | "service_target" | "operation_id" | "depends_on" | "rollback" | "output_schema">;

export interface SearchDocsArgs {
  /** Fuzzy/keyword query. Present together with no operationId => query mode. */
  query?: string;
  /** Exact operation ID. Present => operationId mode, ignores query. */
  operationId?: string;
  /** Top-K cap for query mode. Defaults to 10. */
  limit?: number;
}

export type SearchDocsResult =
  | { mode: "list" | "query"; operations: OperationSummary[] }
  | { mode: "operationId"; operation: OperationDetail | UnifiedOperationDetail }
  | { mode: "operationId"; error: string };

/**
 * searchDocs implements the three-mode contract from the design doc's
 * Discovery section: no args => list mode (everything, bare identifiers), a
 * query => query mode (fuzzy top-K, same bare shape), an operationId =>
 * full schema for exactly that operation. operationId takes precedence over
 * query if a caller somehow sends both, since "give me detail on this known
 * ID" is the more specific request.
 */
export function searchDocs(fixture: Fixture, args: SearchDocsArgs): SearchDocsResult {
  if (args.operationId) {
    return resolveOperationDetail(fixture, args.operationId);
  }
  if (args.query && args.query.trim() !== "") {
    return { mode: "query", operations: queryOperations(fixture, args.query, args.limit ?? 10) };
  }
  return { mode: "list", operations: listOperations(fixture) };
}

/** Resolves one exact name across the collision-free physical and logical indexes. */
function resolveOperationDetail(fixture: Fixture, operationId: string): SearchDocsResult {
  const op = fixture.resolve(operationId);
  // Physical lookup remains first and collision admission guarantees an exact
  // name can never classify as both operation kinds.
  if (op) {
    return { mode: "operationId", operation: toDetail(op) };
  }
  const unified = fixture.resolveUnified(operationId);
  // Logical lookup uses only the public descriptor indexed at session start.
  if (unified) {
    return { mode: "operationId", operation: toUnifiedDetail(unified) };
  }
  // Mirrors the Go-side Resolve()'s "not found" contract exactly: no partial
  // match or broader fallback can make an unregistered name discoverable.
  return { mode: "operationId", error: `no such operationId: ${operationId}` };
}

/** Lists both immutable catalogue kinds without adding a third MCP tool. */
function listOperations(fixture: Fixture): OperationSummary[] {
  return [...fixture.operations.map(toSummary), ...fixture.unifiedOperations.map(toUnifiedSummary)];
}

/** Fuzzy query scores both kinds together so the caller receives one bounded ranking. */
function queryOperations(fixture: Fixture, query: string, limit: number): OperationSummary[] {
  const candidates = [
    ...fixture.operations.map((operation) => ({ summary: toSummary(operation), score: bestScore(query, [operation.name, operation.description, operation.path]) })),
    ...fixture.unifiedOperations.map((operation) => ({ summary: toUnifiedSummary(operation), score: bestScore(query, [operation.name, operation.description]) })),
  ];
  // Only positive matches consume the caller's bounded result budget.
  return candidates
    .filter(({ score }) => score > 0)
    .sort((a, b) => b.score - a.score)
    .slice(0, limit)
    .map(({ summary }) => summary);
}

/** Projects a physical summary without changing its existing method metadata. */
function toSummary(op: FixtureOperation): OperationSummary {
  return {
    operation_id: op.operation_id,
    method: op.method,
    description: op.description ?? op.name,
  };
}

/** Projects a logical summary under its exact authored operation name. */
function toUnifiedSummary(operation: FixtureUnifiedOperation): OperationSummary {
  return {
    kind: "unified",
    operation_id: operation.name,
    description: operation.description ?? operation.name,
  };
}

/** Keeps physical documentation shallow so nested contracts remain authoritative. */
function toDetail(op: FixtureOperation): OperationDetail {
  return {
    operation_id: op.operation_id,
    method: op.method,
    path: op.path,
    description: op.description,
    parameters: op.parameters ?? [],
    request_content: op.request_content ?? null,
    responses: op.responses,
  };
}

/** Projects a Unified descriptor while intentionally dropping internal UUID identities. */
function toUnifiedDetail(operation: FixtureUnifiedOperation): UnifiedOperationDetail {
  return {
    kind: "unified",
    operation_id: operation.name,
    description: operation.description,
    input_schema: operation.input_schema,
    output_schema: operation.output_schema,
    targets: operation.targets.map(toUnifiedTargetDetail),
  };
}

/** Projects only caller-relevant target names, edges, rollback names, and schemas. */
function toUnifiedTargetDetail(target: FixtureUnifiedTarget): UnifiedTargetDetail {
  return {
    public_target: target.public_target,
    service_target: target.service_target,
    operation_id: target.operation_id,
    depends_on: target.depends_on,
    rollback: target.rollback ? { operation_id: target.rollback.operation_id } : undefined,
    output_schema: target.output_schema,
  };
}
