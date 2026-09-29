import { $ZodType, parseAsync, toJSONSchema, type $ZodTypeDef, type input, type output } from "zod/v4/core";

export type JsonValue = null | boolean | number | string | JsonValue[] | { [key: string]: JsonValue };
export type JsonObject = { [key: string]: JsonValue };

export interface WorkspaceOperationRequest {
  service: string;
  operation: string;
  input: JsonObject;
  pagination?: { maxPages: number };
  selector?: {
    environment?: string;
    endUserRef?: string;
    authType?: string;
    authName?: string;
    resourceId?: string;
  };
}

export interface ExecutionHost {
  fetch(requestJson: string): Promise<string>;
  dbGet(): Promise<string>;
  dbSet(valueJson: string): Promise<void>;
}

declare global {
  // The isolated worker provides this bridge; the author bundle contains no credentials or transport.
  var __fusedHost: ExecutionHost | undefined;
}

export interface ExecutionContext<Input> {
  input: Input;
}

export interface UnifiedApp<Input extends $ZodType, Output extends $ZodType> {
  readonly input: Input;
  readonly output: Output;
  readonly fetch: Readonly<{ searchable: readonly string[] }>;
  readonly execute: (context: ExecutionContext<output<Input>>) => Promise<input<Output>> | input<Output>;
}

export interface UnifiedAppConfig<Input extends $ZodType, Output extends $ZodType> {
  input: Input;
  output: Output;
  fetch?: { searchable?: readonly string[] };
  execute: (context: ExecutionContext<output<Input>>) => Promise<input<Output>> | input<Output>;
}

const SEARCH_PATH = /^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*){0,3}$/;
const FORBIDDEN_SEGMENTS = new Set(["__proto__", "prototype", "constructor"]);
const MAX_SEARCHABLE = 16;
const MAX_JSON_BYTES = 512 * 1024;

// Accept plain objects from the worker or host realm without admitting class instances.
function isPlainObject(value: object): boolean {
  const prototype = Object.getPrototypeOf(value);
  // A plain object's prototype is the last object before null in either realm.
  return prototype === null || Object.getPrototypeOf(prototype) === null;
}

// Identify values that need no recursive JSON validation.
function isJsonScalar(value: unknown): value is null | string | boolean {
  return value === null || typeof value === "string" || typeof value === "boolean";
}

// Reject malformed or unsafe paths before an immutable app version is published.
export function validateSearchablePaths(paths: readonly string[]): readonly string[] {
  // A small allowlist keeps queries and indexes bounded for every app version.
  if (paths.length > MAX_SEARCHABLE) {
    throw new Error(`fetch.searchable supports at most ${MAX_SEARCHABLE} data paths`);
  }
  const seen = new Set<string>();
  // Validate each declared path once so search policy is stable across adapters.
  for (const path of paths) {
    // Valid paths are scalar field chains rooted in the execution's data document.
    if (typeof path !== "string" || !SEARCH_PATH.test(path) || path.split(".").some((part) => FORBIDDEN_SEGMENTS.has(part))) {
      throw new Error(`Invalid fetch.searchable data path: ${String(path)}`);
    }
    // Duplicate paths would make manifest and search policy ambiguous.
    if (seen.has(path)) {
      throw new Error(`Duplicate fetch.searchable data path: ${path}`);
    }
    seen.add(path);
  }
  return Object.freeze([...paths]);
}

// Preserve the authored contract while validating its public search declaration.
export function buildUnifiedApp<Input extends $ZodType, Output extends $ZodType>(config: UnifiedAppConfig<Input, Output>): UnifiedApp<Input, Output> {
  // The builder must have runtime schemas and executable code before it can be bundled.
  if (!(config.input instanceof $ZodType) || !(config.output instanceof $ZodType) || typeof config.execute !== "function") {
    throw new Error("Unified App requires Zod input/output schemas and execute()");
  }
  // An omitted fetch policy exposes only Engine-owned metadata search fields.
  const searchable = validateSearchablePaths(config.fetch?.searchable ?? []);
  return Object.freeze({ input: config.input, output: config.output, fetch: Object.freeze({ searchable }), execute: config.execute });
}

// Ensure host-bound effects cannot run while build declarations are evaluated.
function host(): ExecutionHost {
  // A missing bridge means this code is outside an admitted Engine execution.
  if (!globalThis.__fusedHost) {
    throw new Error("Fused execution host bridge is unavailable");
  }
  return globalThis.__fusedHost;
}

// Check actual JSON values before serialization can silently discard unsupported data.
function assertJsonValue(value: unknown, label: string, ancestors: Set<object>): void {
  // JSON scalars must be finite when represented as numbers.
  if (isJsonScalar(value)) {
    return;
  }
  // JSON.stringify otherwise changes non-finite numbers into null.
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new Error(`${label} contains a non-finite number`);
    }
    return;
  }
  // Undefined, functions, symbols, and bigint have no stable JSON representation.
  if (typeof value !== "object") {
    throw new Error(`${label} must be JSON-serializable`);
  }
  // Only plain objects and arrays preserve their meaning across the Engine boundary.
  if (!Array.isArray(value) && !isPlainObject(value)) {
    throw new Error(`${label} must contain plain JSON objects`);
  }
  // Cycles cannot become a bounded provider request or JSONB document.
  if (ancestors.has(value)) {
    throw new Error(`${label} contains a cycle`);
  }
  ancestors.add(value);
  // Every nested value must remain representable after JSON serialization.
  for (const child of Object.values(value)) {
    assertJsonValue(child, label, ancestors);
  }
  ancestors.delete(value);
}

// Serialize a validated value using primitives available in the isolated worker.
function encodeJson(value: unknown, label: string, byteLimit?: number): string {
  assertJsonValue(value, label, new Set<object>());
  const encoded = JSON.stringify(value);
  // The Engine repeats this limit at storage; this catches oversized data before IPC.
  if (byteLimit !== undefined && encodeURIComponent(encoded).replace(/%[0-9A-F]{2}/g, "x").length > byteLimit) {
    throw new Error(`${label} exceeds ${byteLimit} bytes`);
  }
  return encoded;
}

// Invoke an admitted workspace operation through the Engine-owned sandbox bridge.
async function fetchOperation(request: WorkspaceOperationRequest): Promise<JsonValue> {
  // The bridge accepts identities, never a caller-controlled URL.
  if (!request || !request.service || !request.operation || typeof request.service !== "string" || typeof request.operation !== "string") {
    throw new Error("fused.fetch requires service and operation identities");
  }
  // Provider parameters must remain a named JSON object at the Engine dispatch boundary.
  if (!request.input || typeof request.input !== "object" || Array.isArray(request.input) || !isPlainObject(request.input)) {
    throw new Error("fused.fetch input must be a JSON object");
  }
  validateFetchPagination(request.pagination);
  const response = await host().fetch(encodeJson(request, "fused.fetch request"));
  return JSON.parse(response) as JsonValue;
}

// Check the author-owned page ceiling locally while leaving operation policy to the Engine.
function validateFetchPagination(pagination: WorkspaceOperationRequest["pagination"]): void {
  // Omission retains automatic pagination exactly as configured for the selected provider operation.
  if (pagination === undefined) {
    return;
  }
  // A supplied bound must be a positive JSON integer before it crosses the worker bridge.
  if (!pagination || !Number.isSafeInteger(pagination.maxPages) || pagination.maxPages < 1) {
    throw new Error("fused.fetch pagination.maxPages must be a positive integer");
  }
}

type UserBoundFetch = Readonly<{ fetch: (request: WorkspaceOperationRequest) => Promise<JsonValue> }>;

// Validate a stable OAuth connection key before an authored helper can dispatch with it.
function validateBoundUserRef(endUserRef: string): void {
  // Empty or padded references would select a different connection from the one used during OAuth consent.
  if (typeof endUserRef !== "string" || !endUserRef || endUserRef.trim() !== endUserRef) {
    throw new Error("Fused user reference must be non-empty and unpadded");
  }
}

// Apply a reviewed binding without permitting one call to switch provider connections.
async function fetchWithBoundUserRef(request: WorkspaceOperationRequest, endUserRef: string): Promise<JsonValue> {
  // Conflicting refs are an authoring error; the helper must never dispatch under another user's connection.
  if (request?.selector?.endUserRef && request.selector.endUserRef !== endUserRef) {
    throw new Error("Fused bound fetch cannot override the bound user reference");
  }
  return fetchOperation({ ...request, selector: { ...request.selector, endUserRef } });
}

// Bind one stable connected-user reference to every workspace call made through this helper.
function forUserRef(endUserRef: string): UserBoundFetch {
  validateBoundUserRef(endUserRef);
  return Object.freeze({
    // The same immutable binding is reused by each call through this helper.
    async fetch(request: WorkspaceOperationRequest): Promise<JsonValue> {
      return fetchWithBoundUserRef(request, endUserRef);
    },
  });
}

// Bind each selected service to its own stable connected-user reference.
function forServiceUserRefs(refs: Readonly<Record<string, string>>): UserBoundFetch {
  // An empty or non-object map has no safe default for an OAuth-backed service.
  if (!refs || !isPlainObject(refs) || Object.keys(refs).length === 0) {
    throw new Error("fused.forServiceUserRefs requires a non-empty service map");
  }
  const bindings = new Map<string, string>();
  for (const [service, endUserRef] of Object.entries(refs)) {
    // Exact service keys and validated references match the immutable operation selection and OAuth connection.
    if (!service || service.trim() !== service) {
      throw new Error("fused.forServiceUserRefs requires unpadded service names");
    }
    validateBoundUserRef(endUserRef);
    bindings.set(service, endUserRef);
  }
  return Object.freeze({
    // A service absent from the map must not inherit another provider's user reference.
    async fetch(request: WorkspaceOperationRequest): Promise<JsonValue> {
      const endUserRef = bindings.get(request?.service);
      // An unmapped service requires an explicit binding before it can use this helper.
      if (!endUserRef) {
        throw new Error("fused.forServiceUserRefs has no user reference for this service");
      }
      return fetchWithBoundUserRef(request, endUserRef);
    },
  });
}

// Read the current execution's single JSONB document through the Engine bridge.
async function dbGet(): Promise<JsonValue> {
  return JSON.parse(await host().dbGet()) as JsonValue;
}

// Replace the current execution's JSONB document after the local size check.
async function dbSet(value: JsonValue): Promise<void> {
  await host().dbSet(encodeJson(value, "fused.db data", MAX_JSON_BYTES));
}

export const fused = Object.freeze({
  fetch: fetchOperation,
  forUserRef,
  forServiceUserRefs,
  db: Object.freeze({ get: dbGet, set: dbSet }),
});

// Reject Zod checks that JSON Schema would silently omit from public adapters.
function rejectUnrepresentedChecks(value: unknown, seen: Set<object>): void {
  // Only schema definitions and their collections can contain executable checks.
  if (!value || typeof value !== "object" || seen.has(value)) {
    return;
  }
  seen.add(value);
  // A custom refinement has no equivalent validator in the published JSON Schema.
  if (value instanceof $ZodType) {
    const definition = value._zod.def as $ZodTypeDef & { checks?: Array<{ _zod: { def: { check: string } } }> };
    // Refined fields must not have weaker REST or MCP validation than the worker.
    if (definition.checks?.some((check) => check._zod.def.check === "custom")) {
      throw new Error("Unified App schemas cannot use custom Zod refinements");
    }
    rejectUnrepresentedChecks(definition, seen);
    return;
  }
  // Walk nested Zod shapes because a child refinement can weaken public validation.
  for (const child of Object.values(value)) {
    rejectUnrepresentedChecks(child, seen);
  }
}

// Convert a Zod contract into the JSON Schema understood by public adapters.
export function toPublicSchema(schema: $ZodType): Record<string, unknown> {
  rejectUnrepresentedChecks(schema, new Set<object>());
  const converted = toJSONSchema(schema, { unrepresentable: "throw" });
  // Public app inputs and outputs are objects with named fields.
  if (!converted || converted.type !== "object") {
    throw new Error("Unified App input and output must be JSON objects");
  }
  return converted as Record<string, unknown>;
}

// Parse both boundaries so generated client, REST, and MCP see the same authored output.
export async function runUnifiedApp<Input extends $ZodType, Output extends $ZodType>(app: UnifiedApp<Input, Output>, rawInput: unknown): Promise<output<Output>> {
  const input = await parseAsync(app.input, rawInput);
  const output = await app.execute({ input });
  const parsed = await parseAsync(app.output, output);
  encodeJson(parsed, "Unified App output");
  return parsed;
}
