import {
  Fixture,
  FixtureOperation,
  FixtureParameter,
  FixtureRequestContent,
  FixtureResponseContract,
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
  operation_id: string;
  method: string;
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
  | { mode: "operationId"; operation: OperationDetail }
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

function resolveOperationDetail(fixture: Fixture, operationId: string): SearchDocsResult {
  const op = fixture.resolve(operationId);
  if (!op) {
    // Mirrors the Go-side Resolve()'s "not found" contract exactly: no
    // partial match, no fallback to a summary -- an unregistered
    // operationId simply isn't answerable, consistent with tier 1 of the
    // Trust and Governance Model.
    return { mode: "operationId", error: `no such operationId: ${operationId}` };
  }
  return { mode: "operationId", operation: toDetail(op) };
}

function listOperations(fixture: Fixture): OperationSummary[] {
  return fixture.operations.map(toSummary);
}

function queryOperations(fixture: Fixture, query: string, limit: number): OperationSummary[] {
  return fixture.operations
    .map((op) => ({ op, score: bestScore(query, [op.name, op.description, op.path]) }))
    .filter(({ score }) => score > 0)
    .sort((a, b) => b.score - a.score)
    .slice(0, limit)
    .map(({ op }) => toSummary(op));
}

function toSummary(op: FixtureOperation): OperationSummary {
  return {
    operation_id: op.operation_id,
    method: op.method,
    description: op.description ?? op.name,
  };
}

// Keep this a shallow projection so documentation cannot diverge from the
// exact nested request/response contracts authorized by Engine.
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
