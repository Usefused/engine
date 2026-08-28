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
  shared_definitions?: boolean;
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
  service_version_id?: string;
  operation_id: string;
  service_id: string;
  name: string;
  description?: string;
  method: string;
  path: string;
  parameters?: FixtureParameter[];
  request_content?: FixtureRequestContent | null;
  responses: Record<string, FixtureResponseContract>;
  pagination: FixturePagination;
}

/** Carries the bounded effective Engine policy needed for safe agent invocation. */
export interface FixturePagination {
  supported: boolean;
  caller_bound_supported: boolean;
  engine_max_pages?: number;
}

/** Mirrors the existing credential-free Unified descriptor stored in the applied plan. */
export interface FixtureUnifiedOperation {
  name: string;
  description?: string;
  input_schema: unknown;
  output_schema?: unknown;
  targets: FixtureUnifiedTarget[];
}

/** Carries the public graph metadata search_docs may safely project. */
export interface FixtureUnifiedTarget {
  public_target: string;
  service_target?: string;
  operation_id: string;
  depends_on?: string[];
  rollback?: { operation_id: string };
  output_schema?: unknown;
}

/** Carries app-version identity that MCP hosts can inspect before listing tools. */
export interface FixtureServerMetadata {
  name: string;
  title: string;
  version: string;
  description: string;
}

interface FixtureFile {
  server?: Partial<FixtureServerMetadata>;
  schema_definitions?: Record<string, Record<string, FixtureSchemaContract>>;
  operations: FixtureOperation[];
  unified_operations?: {
    schema_version: number;
    operations: FixtureUnifiedOperation[];
  };
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
  private readonly byUnifiedOperationId = new Map<string, FixtureUnifiedOperation>();

  /** Admits immutable server identity before indexing any callable catalogue entries. */
  constructor(
    public readonly operations: FixtureOperation[],
    public readonly unifiedOperations: FixtureUnifiedOperation[] = [],
    public readonly schemaDefinitions: Record<string, Record<string, FixtureSchemaContract>> = {},
    server?: Partial<FixtureServerMetadata>,
  ) {
    this.server = validateServerMetadata(server);
    for (const op of operations) {
      // Exact physical IDs remain mandatory because call() has no fallback key.
      if (!op.operation_id) {
        throw new Error("fixture operation missing operation_id");
      }
      validateFixturePagination(op.operation_id, op.pagination);
      // Serialized fixture files retain their existing strict duplicate check.
      if (this.byOperationId.has(op.operation_id)) {
        throw new Error(`duplicate operation_id "${op.operation_id}" in fixture`);
      }
      this.byOperationId.set(op.operation_id, op);
    }
    for (const operation of unifiedOperations) {
      // Empty logical names cannot become exact call identifiers.
      if (!operation.name) {
        throw new Error("Unified fixture operation missing name");
      }
      // Cross-kind or repeated logical names are ambiguous to call(operationId)
      // and therefore fail before the MCP server exposes either operation.
      if (this.byOperationId.has(operation.name) || this.byUnifiedOperationId.has(operation.name)) {
        throw new Error(`physical and Unified operation name collision "${operation.name}"`);
      }
      this.byUnifiedOperationId.set(operation.name, operation);
    }
  }

  public readonly server: FixtureServerMetadata;

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

  /** Resolves one exact authored Unified name without inspecting private graph state. */
  resolveUnified(operationId: string): FixtureUnifiedOperation | undefined {
    return this.byUnifiedOperationId.get(operationId);
  }
}

/** Validates complete app-version identity before the runtime advertises an MCP server. */
function validateServerMetadata(server?: Partial<FixtureServerMetadata>): FixtureServerMetadata {
  const name = server?.name?.trim();
  const title = server?.title?.trim();
  const version = server?.version?.trim();
  const description = server?.description?.trim();
  // Missing authored metadata means the immutable MCP version is not runnable.
  if (!name || !title || !version || !description) {
    throw new Error("fixture.json missing required server metadata");
  }
  // Authored descriptions are admitted by Engine, while standalone fixtures still need a defensive context bound.
  if (Buffer.byteLength(description, "utf8") > 1024) {
    throw new Error("fixture server description exceeds 1024 bytes");
  }
  return { name, title, version, description };
}

/** Rejects incomplete or contradictory pagination metadata before it becomes model-visible. */
function validateFixturePagination(operationId: string, pagination: FixturePagination): void {
  // Every physical operation must state whether Engine owns provider traversal.
  if (!pagination || typeof pagination.supported !== "boolean" || typeof pagination.caller_bound_supported !== "boolean") {
    throw new Error(`fixture operation "${operationId}" missing pagination metadata`);
  }
  // Unsupported operations cannot advertise a bound that execution would reject.
  if (!pagination.supported) {
    // Absence must be represented without decorative limits or caller controls.
    if (pagination.caller_bound_supported || pagination.engine_max_pages !== undefined) {
      throw new Error(`fixture operation "${operationId}" has invalid unsupported pagination metadata`);
    }
    return;
  }
  // Engine limits must be positive integers so strict lower-bound guidance is well-defined.
  if (!Number.isInteger(pagination.engine_max_pages) || (pagination.engine_max_pages ?? 0) < 1) {
    throw new Error(`fixture operation "${operationId}" has invalid pagination limit`);
  }
  // A caller-owned positive reduction exists exactly when the Engine limit exceeds one page.
  if (pagination.caller_bound_supported !== ((pagination.engine_max_pages ?? 0) > 1)) {
    throw new Error(`fixture operation "${operationId}" has contradictory pagination bound metadata`);
  }
}

/**
 * loadFixture deliberately indexes without reshaping nested contracts, because
 * search_docs must expose the same reviewed bytes that Go authorized.
 */
export function loadFixture(path: string): Fixture {
  const raw = readFileSync(path, "utf-8");
  const parsed = JSON.parse(raw) as FixtureFile;
  // A physical array remains mandatory because existing physical-only fixtures
  // and runtime enforcement share that stable top-level contract.
  if (!Array.isArray(parsed.operations)) {
    throw new Error("fixture.json missing an \"operations\" array");
  }
  const unified = parsed.unified_operations;
  // Descriptor schema admission belongs at fixture load so search_docs never
  // guesses how to project a future compiler contract.
  if (unified !== undefined && (unified.schema_version !== 3 || !Array.isArray(unified.operations))) {
    throw new Error("fixture.json has an unsupported Unified descriptor");
  }
  // Standalone fixtures have no dictionary; compact roots retain the exact version-keyed shared source.
  return new Fixture(parsed.operations, unified?.operations ?? [], parsed.schema_definitions ?? {}, parsed.server);
}
