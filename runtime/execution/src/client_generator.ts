import { parseExecutionManifest } from "./bundle";
import { renderPublicType } from "./client_schema";

const CLIENT_RUNTIME = String.raw`
export type ExecutionStatus = "queued" | "running" | "succeeded" | "failed" | "indeterminate";
export type ExecutionMode = "live" | "replay" | "rerun";
export type SearchScalar = string | number | boolean | null;
export type SearchField = /* SEARCH_FIELDS */;
export type SearchWhere = Partial<Record<SearchField, SearchScalar>>;
export interface ExecutionResult {
  executionId: string;
  appId: string;
  version: string;
  status: ExecutionStatus;
  mode: ExecutionMode;
  sourceExecutionId?: string;
  readHandle?: string;
  output?: ExecutionOutput;
  error?: { code: string; message: string };
  createdAt: string;
  completedAt?: string;
}
export interface ExecutionHandle extends ExecutionResult { readHandle: string }
export interface FetchLike {
  (url: string, init: { method: string; headers: Record<string, string>; body?: string }): Promise<{
    ok: boolean; status: number; json(): Promise<unknown>;
  }>;
}
export interface FusedExecutionClientOptions {
  baseUrl: string;
  appId: string;
  token: string;
  fetcher?: FetchLike;
}

export class FusedExecutionHttpError extends Error {
  // Keep the public Engine code and HTTP status without copying private response bodies.
  constructor(readonly status: number, readonly code: string, message: string) {
    super(message);
    this.name = "FusedExecutionHttpError";
  }
}

// Read an error envelope without assuming it has the expected shape.
function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

class ExecutionTransport {
  private readonly baseUrl: string;
  private readonly fetcher: FetchLike;
  // Bind this client to one exact immutable Unified App version and family token.
  constructor(private readonly options: FusedExecutionClientOptions) {
    // Credentials and app identity must be explicit before any request can run.
    if (!options.baseUrl || !options.appId || !options.token) throw new Error("Fused Execution client requires baseUrl, appId, and token");
    this.baseUrl = options.baseUrl.replace(/\/+$/, "");
    const available = options.fetcher ?? (globalThis as { fetch?: FetchLike }).fetch;
    // No transport fallback should silently change where authored code executes.
    if (!available) throw new Error("No fetch implementation is available");
    this.fetcher = available;
  }

  // Send every lifecycle command through the same Engine execution record surface.
  async request<Result>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<Result> {
    const response = await this.fetcher(this.baseUrl + path, {
      method,
      headers: { Authorization: "Bearer " + this.options.token, "Content-Type": "application/json", ...headers },
      // Read routes have no body, while execute sends the shared Engine operation envelope.
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    let payload: unknown;
    try {
      payload = await response.json();
    } catch {
      // A malformed response cannot be interpreted as a successful typed execution.
      throw new FusedExecutionHttpError(response.status, "invalid_response", "Engine response was not JSON");
    }
    // Rejected requests use the bounded public error envelope from Engine.
    if (!response.ok) {
      const error = isRecord(payload) && isRecord(payload.error) ? payload.error : {};
      throw new FusedExecutionHttpError(response.status, String(error.code ?? "request_failed"), String(error.message ?? "Execution request failed"));
    }
    return payload as Result;
  }

  // Encode the exact app ID in one place for every generated method.
  appPath(): string {
    return "/v1/apps/" + encodeURIComponent(this.options.appId);
  }
}

export class FusedExecutionClient {
  private readonly transport: ExecutionTransport;
  // Keep the generated surface as one execute operation over shared REST transport.
  constructor(options: FusedExecutionClientOptions) {
    this.transport = new ExecutionTransport(options);
  }

  // Invoke the app's one authored execute function through the shared Engine REST route.
  execute(input: ExecutionInput): Promise<ExecutionHandle> {
    return this.transport.request("POST", this.transport.appPath() + "/executions", { operation: "execute", input });
  }

  // Fetch one execution by ID using its caller-held read handle.
  fetch(executionId: string, readHandle: string): Promise<ExecutionResult> {
    return this.transport.request("GET", this.transport.appPath() + "/executions/" + encodeURIComponent(executionId), undefined, { "X-Execution-Read-Handle": readHandle });
  }

  // Search only metadata fields and data paths declared by this app version.
  search(where: SearchWhere, limit = 20): Promise<{ items: ExecutionResult[] }> {
    // Engine accepts bounded first-page searches and enforces the immutable allowlist.
    if (!Number.isInteger(limit) || limit < 1 || limit > 100) throw new Error("Search limit must be between 1 and 100");
    const query = "?where=" + encodeURIComponent(JSON.stringify(where)) + "&limit=" + limit;
    return this.transport.request("GET", this.transport.appPath() + "/executions" + query);
  }

  // Replay recorded effects under the same pinned app version.
  replay(executionId: string, readHandle: string): Promise<ExecutionHandle> {
    return this.transport.request("POST", this.transport.appPath() + "/executions/" + encodeURIComponent(executionId) + "/replay", undefined, { "X-Execution-Read-Handle": readHandle });
  }

  // Require an explicit key because rerun can repeat provider mutations.
  rerun(executionId: string, readHandle: string, idempotencyKey: string): Promise<ExecutionResult> {
    // An empty key cannot provide stable duplicate-submission identity.
    if (!idempotencyKey) throw new Error("Rerun requires an idempotency key");
    return this.transport.request("POST", this.transport.appPath() + "/executions/" + encodeURIComponent(executionId) + "/rerun", undefined, { "X-Execution-Read-Handle": readHandle, "Idempotency-Key": idempotencyKey });
  }
}

// Construct the typed external adapter without adding a second execution runtime.
export function createFusedExecutionClient(options: FusedExecutionClientOptions): FusedExecutionClient {
  return new FusedExecutionClient(options);
}
`;

// Generate one typed REST client from the app version's Zod-derived public schemas.
export function generateExecutionClientSource(rawManifest: unknown): string {
  const manifest = parseExecutionManifest(rawManifest);
  const fields = ["executionId", "status", "createdAt", ...manifest.searchable.map((path) => `data.${path}`)];
  const searchType = fields.map((field) => JSON.stringify(field)).join(" | ");
  return [
    renderPublicType(manifest.inputSchema, "ExecutionInput"),
    renderPublicType(manifest.outputSchema, "ExecutionOutput"),
    CLIENT_RUNTIME.replace("/* SEARCH_FIELDS */", searchType),
  ].join("\n\n");
}
