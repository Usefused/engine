#!/usr/bin/env node
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { loadFixture } from "./fixture.js";
import {
  DocumentationSection,
  SEARCH_DOCS_MAX_LIMIT,
  isDocumentationSection,
  searchDocs,
} from "./searchDocs.js";
import { callClientOptionsFromEnv } from "./callClient.js";
import { DEFAULT_EXECUTE_LIMITS, runExecute, SessionState } from "./sandbox.js";
import { BASE64_MAX_BYTES } from "./base64.js";
import { EXECUTE_INLINE_BYTES, EXECUTE_MIN_OUTPUT_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY } from "./resultBudget.js";
import {
  DOCUMENTATION_OUTPUT_POLICY,
  serializeBoundedJson,
} from "./outputLimits.js";
import { SESSION_AGENT_RULE, SESSION_CONTRACT_METADATA } from "./sessionContract.js";

/**
 * Session initialization instructions (design doc, "Session Initialization
 * Instructions"): states the two tools' contract explicitly rather than
 * hoping the model infers it. Kept short -- it's an operating instruction,
 * not documentation; per-operation detail comes from search_docs on demand.
 */
const INSTRUCTIONS = [
  "This server exposes exactly two tools: search_docs and execute.",
  SESSION_AGENT_RULE,
  "Always route API calls through call(operationId, params, options?) inside an execute script -- there is no other way to reach a vendor API from this server. Physical calls automatically complete the endpoint's reviewed provider pagination before returning one aggregate. When the goal intentionally needs only the first N provider pages, pass {pagination:{maxPages:N}} as the optional third argument; provider page-size parameters such as maxResults do not bound total traversal.",
  "Authentication, connected-user identity, and tenant/resource routing are supplied by the Engine. Never invent or pass Authorization headers, API keys, OAuth tokens, auth scheme names, fused_end_user_ref, or fused_resource_id in call params.",
  "Search once with one concise natural-language intent. When a ranked callable has schema_status.complete=true, execute it directly; otherwise retrieve only its missing advertised section with operationId, section, and an optional RFC 6901 schemaPath.",
  "For search_docs detail with kind unified, call the exact operation_id with { input, targets, selectors?, pagination?, idempotencyKey? }; targets must include every declared dependency, selectors and pagination are keyed by public target, and the Engine generates an SDK-equivalent UUID when idempotencyKey is omitted.",
  "search_docs with no arguments returns a bounded schema-free catalogue window. A query ranks physical and Unified callables together and includes callable detail. An exact operationId remains available when its public ID is already known.",
  "An execute script's body should end with `return <value>` -- that value is what gets reported back as the tool result.",
  `Each execute has a ${DEFAULT_EXECUTE_LIMITS.timeoutMs / 1000}-second total budget including provider calls, delays, and serialization, and at most ${DEFAULT_EXECUTE_LIMITS.maxCalls} calls. Await every operation. Use await sleep(ms) for a delay; setTimeout/clearTimeout are also supported with invocation-local numeric handles. Timeout, cancellation, failure, or return clears pending timers, aborts outstanding HTTP requests, and prevents further calls from that invocation. A provider action already accepted may still complete: a timeout does not mean rollback, and you must not automatically retry mutations. Long-running workflows should not be implemented as detached timers.`,
  `Use decodeBase64(text) for UTF-8 base64 or base64url, encodeBase64(text, urlSafe=false) to encode UTF-8, and atob(text) for standard-base64 binary strings (not UTF-8 decoding). Helpers accept strings only and limit raw/decoded data to ${BASE64_MAX_BYTES} bytes per conversion; encoded input/output is bounded to base64 expansion. Malformed base64 or UTF-8 is rejected. Full Node Buffer, fetch, require, and process are unavailable. Decoding does not parse MIME/RFC822; return only needed fields under the normal output budget.`,
  "Small execute results are returned directly. Larger admitted results return MCP_RESULT_STORED with recovery_action=continue_stored_result, execute_request=use_next_request, provider_execution=complete, automatic_replay=false, and one exact next_request for the same execute tool. complete=false describes the visible structural preview, never provider pagination. Run next_request without reconstructing it or repeating the provider call. session.get accepts exactly one string key and ignores no options; session.page reads RFC 6901 array paths. References expire after five minutes or earlier eviction/session closure. MCP_RESULT_UNAVAILABLE uses recovery_action=do_not_replay and requires a deliberate recovery decision.",
  "For lists, project the needed fields in the first execute when they are already known. On overflow, read collections for exact array paths, counts, and immediate row fields. fields_complete=false or collections_complete=false means discovery omitted information; use session.get to inspect additional keys, never infer their absence. Return session.page(result_ref, {path, fields, offset}) directly to pack complete rows within the output budget. path is an RFC 6901 JSON Pointer (empty for a root array); fields are literal immediate keys, omitted for whole rows. Keep path/fields unchanged and use nextOffset for continuation; complete means no rows remain after the returned range, not that earlier pages are included. MCP_RESULT_ROW_TOO_LARGE requires narrower fields or session.get slicing, never a provider retry. outputBudgetBytes is an optional execute argument, default 16384, range 1024..65536; it limits UTF-8 JSON bytes, not tokens. Do not collect all pages into one oversized return value. For aggregates, compute inside execute and return only the answer.",
].join(" ");

/** Starts one process-scoped MCP session with bounded tool outputs. */
function main(): void {
  const fixturePath = process.env.FUSED_FIXTURE_PATH;
  // A session without its immutable fixture cannot safely expose either MCP tool.
  if (!fixturePath) {
    throw new Error("FUSED_FIXTURE_PATH is required");
  }
  const fixture = loadFixture(fixturePath);
  const callOptions = callClientOptionsFromEnv();
  // One SessionState per process lifetime -- this process is one MCP
  // session (see sprint/lighter_mcp_runtime_design.md, Sandbox and
  // Isolation Rules: one spawned process per session, never pooled), so
  // session-scoped state and process lifetime coincide here by construction.
  const session = new SessionState();

  const server = new McpServer(
    { name: "fused-mcp-shared-runtime", version: "0.1.0" },
    { instructions: INSTRUCTIONS },
  );

  server.registerTool(
    "search_docs",
    {
      title: "Search available operations",
      description:
        "List, rank, or fetch bounded documentation for operations available on this MCP server. " +
        "Call with no arguments for a schema-free window, `query` for intent ranking, or `operationId` " +
        "for exact detail. If schema_status is incomplete, retrieve one advertised `section` and " +
        "optionally narrow it with an RFC 6901 `schemaPath`.",
      inputSchema: {
        query: z
          .string()
          .max(512)
          .optional()
          .describe("One concise natural-language intent describing the operation to execute."),
        operationId: z
          .string()
          .max(256)
          .optional()
          .describe("Exact public operation ID for known-detail or lazy-section retrieval."),
        limit: z
          .number()
          .int()
          .min(1)
          .max(SEARCH_DOCS_MAX_LIMIT)
          .optional()
          .describe(`Maximum ranked query matches, from 1 to ${SEARCH_DOCS_MAX_LIMIT}; defaults to 3.`),
        section: z
          .custom(isDocumentationSection, "invalid public documentation section")
          .optional()
          .describe("One exact advertised section: parameters, request, response:<status>, input, targets, output, or definitions. Shared #/$defs references use the definitions section with schemaPath /<escaped-name>/raw."),
        schemaPath: z
          .string()
          .max(2048)
          .optional()
          .describe("Optional RFC 6901 JSON Pointer within the selected section."),
      },
    },
    // Documentation is serialized at the handler boundary so every discovery
    // mode shares one wire-size policy without changing catalogue semantics.
    async (args) => {
      // MCP's schema inference erases custom Zod output types after the section predicate validates them.
      const result = searchDocs(fixture, { ...args, section: args.section as DocumentationSection | undefined });
      const output = serializeBoundedJson(result, DOCUMENTATION_OUTPUT_POLICY);
      return { content: [{ type: "text", text: output.text }], isError: output.isError };
    },
  );

  server.registerTool(
    "execute",
    {
      title: "Execute a script",
      description:
        SESSION_AGENT_RULE + " Run a short TypeScript script that can call one or more operations via call(operationId, params, options?) " +
        "and chain their results, returning one final value. Use complete search_docs detail before " +
        "referencing an operationId for the first time. Physical operations complete reviewed provider pagination before returning one aggregate; " +
        "use the optional third argument {pagination:{maxPages:N}} only to request a narrower result bound. " +
        "Large results return provider_execution=complete and an exact retained-data next_request for this same execute tool; " +
        "run it without reconstructing session arguments or repeating provider calls. Each incomplete page supplies its exact next_request. Unified operations use the exact documented ID " +
        "and params { input, targets, selectors?, pagination?, idempotencyKey? }. " +
        `The total deadline is ${DEFAULT_EXECUTE_LIMITS.timeoutMs / 1000} seconds, including calls and sleep(ms). Timeout cancels pending work but does not undo accepted provider actions; never automatically retry mutations. ` +
        "Use decodeBase64(text) for UTF-8 base64/base64url, encodeBase64(text, urlSafe=false), or atob(text) for binary strings; Node Buffer is unavailable.",
      _meta: { "com.usefused/session": SESSION_CONTRACT_METADATA },
      inputSchema: {
        outputBudgetBytes: z.number().int().min(EXECUTE_MIN_OUTPUT_BYTES).max(EXECUTE_VISIBLE_OUTPUT_POLICY.maxBytes).optional()
          .describe(`Maximum UTF-8 JSON bytes returned by this execute and its pages; defaults to ${EXECUTE_INLINE_BYTES}. Not a token count. Keep the same budget on continuation calls.`),
        script: z
          .string()
          .describe(
            "TypeScript to run. Use `await call(operationId, params, options?)` to invoke operations; physical options may contain only `{pagination:{maxPages:N}}`. " +
              "`session.get(key)`/`session.set(key, value)` for state across execute calls in this " +
              "client-managed session; never pass a session ID. A new MCP connection has new state. End with `return <value>` for the final result.",
          ),
      },
    },
    // The sandbox returns only trusted, already-bounded text, so the handler
    // cannot accidentally serialize user-controlled objects outside its deadline.
    async (args, extra) => {
      const output = await runExecute(args.script, callOptions, session, { ...DEFAULT_EXECUTE_LIMITS, outputBudgetBytes: args.outputBudgetBytes }, undefined, extra.signal);
      return {
        content: [{ type: "text", text: output.text }], isError: output.isError,
        _meta: { "com.usefused/execute": { delivery: output.delivery, output_budget_bytes: output.outputBudgetBytes, execution_outcome: output.executionOutcome, ...output.access } },
      };
    },
  );

  const transport = new StdioServerTransport();
  void server.connect(transport);
}

main();
