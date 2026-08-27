/**
 * callClient is trusted host code -- it runs in the outer Node process, not
 * inside the vm sandbox, and is the *only* place in this package allowed to
 * use fetch/network primitives directly. sandbox.ts binds a wrapped version
 * of `call` into the sandbox's global scope; the raw fetch here is never
 * itself exposed to sandboxed script -- see sandbox.ts for why that
 * distinction is the whole point (design doc, "No I/O outside call()").
 *
 * This makes an HTTP request to the Engine's own /mcp/call endpoint
 * (internal/engine/sandbox/mcp_shared_runtime.go) rather than the vendor
 * directly. Credentials are never read or held here: the request carries
 * only (operationId, params) plus the session bearer used as a
 * lookup key with session authority, and the Go side resolves provider credentials server-side from the
 * session's validated token -- see sprint/lighter_mcp_runtime_design.md,
 * "Credentials never enter the process running the script."
 */

interface CallResponse {
  result?: unknown;
  error?: string;
  code?: string;
}

/** PhysicalCallOptions carries caller-owned execution controls separately from provider parameters. */
export interface PhysicalCallOptions {
  pagination?: {
    maxPages: number;
  };
}

export interface CallClientOptions {
  sessionId: string;
  enginePort: string;
}

/** Reads the non-secret identifiers this runtime needs from its own
 * environment. Called once at startup, not per call() invocation. */
export function callClientOptionsFromEnv(): CallClientOptions {
  const sessionId = process.env.FUSED_SESSION_ID;
  const enginePort = process.env.FUSED_ENGINE_PORT;
  if (!sessionId) {
    throw new Error("FUSED_SESSION_ID is required but not set");
  }
  if (!enginePort) {
    throw new Error("FUSED_ENGINE_PORT is required but not set");
  }
  return { sessionId, enginePort };
}

/**
 * remoteCall performs the single HTTP round trip a call() invocation needs.
 * Kept as a thin, single-purpose function (separation of concerns) so the
 * things that make call() safe -- the vm allowlist, the call-count cap, the
 * per-invocation timeout -- all live in sandbox.ts instead of being tangled
 * into the transport code.
 * The invocation signal aborts both fetch and body consumption; Engine observes
 * the disconnected request through its existing execution context.
 */
export async function remoteCall(
  options: CallClientOptions,
  operationId: string,
  params: Record<string, unknown>,
  signal?: AbortSignal,
  callOptions?: PhysicalCallOptions,
): Promise<unknown> {
  signal?.throwIfAborted();
  // Omitting pagination preserves the established envelope and automatic Engine pagination behavior.
  const pagination = callOptions?.pagination;
  let response: Response;
  try {
    response = await fetch(`http://localhost:${options.enginePort}/mcp/call`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${options.sessionId}`,
      },
      body: JSON.stringify({ operation_id: operationId, params, ...(pagination ? { pagination } : {}) }),
      signal,
    });
  } catch (error) {
    // Caller cancellation already has precise invocation semantics and must not be relabeled as bridge uncertainty.
    if (signal?.aborted) throw error;
    throw new Error("MCP_BRIDGE_UNAVAILABLE: Engine bridge did not respond; request outcome is unknown. Do not automatically retry provider mutations.");
  }

  let body: CallResponse;
  try {
    body = (await response.json()) as CallResponse;
  } catch (error) {
    // Abort during body consumption belongs to the invocation owner just like abort during fetch.
    if (signal?.aborted) throw error;
    // A proxy or truncated bridge response cannot prove whether provider dispatch occurred.
    throw new Error("MCP_BRIDGE_RESPONSE_INVALID: Engine bridge returned invalid JSON; request outcome is unknown. Do not automatically retry provider mutations.");
  }
  // JSON primitives and arrays cannot satisfy the bridge's reviewed result-or-error envelope.
  if (typeof body !== "object" || body === null || Array.isArray(body)) {
    throw new Error("MCP_BRIDGE_RESPONSE_INVALID: Engine bridge returned an invalid envelope; request outcome is unknown. Do not automatically retry provider mutations.");
  }
  // Provider and bridge errors preserve the existing reviewed response contract.
  if (!response.ok || body.error) {
    const code = typeof body.code === "string" && body.code ? `${body.code}: ` : "";
    throw new Error(code + (body.error ?? `call() failed with HTTP ${response.status}`));
  }
  return body.result;
}
