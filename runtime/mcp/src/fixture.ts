import { readFileSync } from "node:fs";

/**
 * Mirrors internal/shared/models.Parameter's JSON shape exactly, so
 * fixture.json (and, later, a real export) can be read identically by both
 * the Go side (call()'s resolution/validation) and this Node side
 * (search_docs) -- one source of truth for what an operationId means, per
 * sprint/lighter_mcp_runtime_design.md.
 */
export interface FixtureParameter {
  name: string;
  in: "path" | "query" | "header";
  required: boolean;
  type: string;
  description?: string;
  path_encoding?: "preserve_slashes";
}

/** Mirrors models.Schema's JSON shape. */
export interface FixtureSchema {
  $ref?: string;
  type?: string;
  format?: string;
  properties?: Record<string, FixtureSchema>;
  items?: FixtureSchema;
  additional_properties?: FixtureSchema;
  required?: string[];
  example?: unknown;
}

/** Mirrors models.RequestContent's reviewed wire representation. */
export interface FixtureRequestContent {
  media_type: string;
  serialization: "json" | "form_urlencoded" | "multipart" | "raw";
  required: boolean;
  schema?: FixtureSchema | null;
  payload_parameter?: string;
  binary_encoding?: "base64";
  parts?: Record<string, FixtureRequestPart>;
}

export interface FixtureRequestPart {
  content_type?: string;
  binary_encoding?: "base64";
}

export interface FixtureOperation {
  operation_id: string;
  service_id: string;
  name: string;
  description?: string;
  method: string;
  path: string;
  parameters?: FixtureParameter[];
  request_content?: FixtureRequestContent | null;
  responses: Record<string, FixtureSchema>;
}

interface FixtureFile {
  operations: FixtureOperation[];
}

/**
 * Fixture is the Node-side twin of the Go Fixture type
 * (internal/engine/sandbox/mcp_fixture.go) -- same file, same shape, loaded
 * independently by each process rather than one process handing data to the
 * other, since search_docs (this side) and call()'s resolution (Go side)
 * both need a fast local lookup and neither should depend on the other being
 * reachable just to answer "what operations exist."
 */
export class Fixture {
  private readonly byOperationId = new Map<string, FixtureOperation>();

  constructor(public readonly operations: FixtureOperation[]) {
    for (const op of operations) {
      if (!op.operation_id) {
        throw new Error("fixture operation missing operation_id");
      }
      if (this.byOperationId.has(op.operation_id)) {
        throw new Error(`duplicate operation_id "${op.operation_id}" in fixture`);
      }
      this.byOperationId.set(op.operation_id, op);
    }
  }

  /**
   * Resolve looks up an operation by operationId. Returns undefined if the ID
   * isn't in this MCP server's registered set -- the mechanical enforcement
   * point for Trust and Governance Model tier 1: an operationId outside the
   * set simply isn't found, there's no downstream path that could act on an
   * ID that was never registered.
   */
  resolve(operationId: string): FixtureOperation | undefined {
    return this.byOperationId.get(operationId);
  }
}

/** loadFixture reads and parses fixture.json from path. */
export function loadFixture(path: string): Fixture {
  const raw = readFileSync(path, "utf-8");
  const parsed = JSON.parse(raw) as FixtureFile;
  if (!Array.isArray(parsed.operations)) {
    throw new Error("fixture.json missing an \"operations\" array");
  }
  return new Fixture(parsed.operations);
}
