import { readFileSync } from "node:fs";

/** Mirrors the canonical Engine parameter wire contract used by MCP sessions. */
export interface FixtureParameter {
  name: string;
  in: "path" | "query" | "header" | "cookie" | "querystring";
  required: boolean;
  type: string;
  description: string;
  path_encoding?: "preserve_slashes";
  serialization: FixtureParameterSerialization;
  schema?: FixtureSchemaContract;
  content?: Record<string, FixtureParameterContent>;
  deprecated?: boolean;
  example?: unknown;
  examples?: Record<string, unknown>;
}

export interface FixtureParameterSerialization {
  style: string;
  explode: boolean | null;
  allow_reserved: boolean | null;
  allow_empty_value: boolean | null;
}

/** The projection is intentionally subordinate to the raw, hashed schema. */
export interface FixtureSchemaProjection {
  $ref?: string;
  type?: string;
  format?: string;
  properties?: Record<string, FixtureSchemaProjection>;
  items?: FixtureSchemaProjection;
  additional_properties?: FixtureSchemaProjection;
  required?: string[];
  example?: unknown;
}

export interface FixtureSchemaContract {
  dialect: string;
  raw: unknown;
  content_hash: string;
  projection: FixtureSchemaProjection;
  projection_diagnostics?: Array<{
    code: string;
    keyword: string;
    pointer: string;
    message: string;
  }>;
}

interface FixtureEncodedContent {
  schema?: FixtureSchemaContract;
  item_schema?: FixtureSchemaContract;
  encoding?: Record<string, FixtureRequestEncoding>;
  prefix_encoding?: FixtureRequestEncoding[];
  item_encoding?: FixtureRequestEncoding;
  example?: unknown;
  examples?: Record<string, unknown>;
}

export interface FixtureParameterContent extends FixtureEncodedContent {}

export interface FixtureRequestEncoding {
  content_type?: string;
  headers?: Record<string, FixtureHeaderContract>;
  style?: string;
  explode?: boolean;
  allow_reserved?: boolean;
  encoding?: Record<string, FixtureRequestEncoding>;
  prefix_encoding?: FixtureRequestEncoding[];
  item_encoding?: FixtureRequestEncoding;
  binary_encoding?: "base64";
}

export interface FixtureHeaderContract {
  description?: string;
  required?: boolean;
  deprecated?: boolean;
  serialization: FixtureParameterSerialization;
  schema?: FixtureSchemaContract;
  content?: Record<string, FixtureParameterContent>;
  example?: unknown;
  examples?: Record<string, unknown>;
}

export interface FixtureRequestRepresentation extends FixtureEncodedContent {
  media_type: string;
  serialization: "json" | "form_urlencoded" | "multipart" | "raw";
}

/** Each executable body representation stays nested under the reviewed request. */
export interface FixtureRequestContent {
  required?: boolean;
  payload_parameter?: string;
  representations: FixtureRequestRepresentation[];
  default_media_type?: string;
  upload_workflow?: FixtureUploadWorkflow;
}

export interface FixtureUploadWorkflow {
  version: number;
  accepted_media_types: string[];
  max_size_bytes?: number;
  modes: Array<{
    kind: string;
    steps: Array<{
      kind: string;
      method: string;
      url: {
        kind: string;
        path?: string;
        header_name?: string;
        allowed_origins?: string[];
      };
      body: string;
      chunking?: {
        default_size_bytes: number;
        size_multiple_bytes: number;
        max_size_bytes: number;
      };
      success_statuses: Array<{ min: number; max: number }>;
      continue_statuses: Array<{ min: number; max: number }>;
    }>;
  }>;
}

export interface FixtureResponseRepresentation {
  media_type: string;
  schema?: FixtureSchemaContract;
  item_schema?: FixtureSchemaContract;
  sse?: { item_mode: string; done_sentinel?: string };
  prefix_encoding?: FixtureRequestEncoding[];
  item_encoding?: FixtureRequestEncoding;
  example?: unknown;
  examples?: Record<string, unknown>;
}

export interface FixtureLinkContract {
  operation_ref?: string;
  operation_id?: string;
  description?: string;
  parameters?: Record<string, unknown>;
  request_body?: unknown;
  server?: {
    url: string;
    name?: string;
    description?: string;
    environment?: string;
    is_default?: boolean;
    variables?: Array<{
      name: string;
      default?: string;
      enum?: string[];
      required: boolean;
    }>;
  };
  extensions?: Record<string, { value: unknown; provenance: string }>;
}

export interface FixtureResponseContract {
  summary?: string;
  description: string;
  headers?: Record<string, FixtureHeaderContract>;
  representations: FixtureResponseRepresentation[];
  // Link execution is not enabled; the typed wire object still keeps docs lossless.
  links?: Record<string, FixtureLinkContract>;
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
  responses: Record<string, FixtureResponseContract>;
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

/**
 * loadFixture deliberately indexes without reshaping nested contracts, because
 * search_docs must expose the same reviewed bytes that Go authorized.
 */
export function loadFixture(path: string): Fixture {
  const raw = readFileSync(path, "utf-8");
  const parsed = JSON.parse(raw) as FixtureFile;
  if (!Array.isArray(parsed.operations)) {
    throw new Error("fixture.json missing an \"operations\" array");
  }
  return new Fixture(parsed.operations);
}
