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
import { runExecute, SessionState } from "./sandbox.js";
import {
  DOCUMENTATION_OUTPUT_POLICY,
  serializeBoundedJson,
} from "./outputLimits.js";

/**
 * Session initialization instructions (design doc, "Session Initialization
 * Instructions"): states the two tools' contract explicitly rather than
 * hoping the model infers it. Kept short -- it's an operating instruction,
 * not documentation; per-operation detail comes from search_docs on demand.
 */
const INSTRUCTIONS = [
  "This server exposes exactly two tools: search_docs and execute.",
  "Always route API calls through call(operationId, params) inside an execute script -- there is no other way to reach a vendor API from this server.",
  "Authentication, connected-user identity, and tenant/resource routing are supplied by the Engine. Never invent or pass Authorization headers, API keys, OAuth tokens, auth scheme names, fused_end_user_ref, or fused_resource_id in call params.",
  "Search once with one concise natural-language intent. When a ranked callable has schema_status.complete=true, execute it directly; otherwise retrieve only its missing advertised section with operationId, section, and an optional RFC 6901 schemaPath.",
  "For search_docs detail with kind unified, call the exact operation_id with { input, targets, selectors?, pagination?, idempotencyKey? }; targets must include every declared dependency, selectors and pagination are keyed by public target, and the Engine generates an SDK-equivalent UUID when idempotencyKey is omitted.",
  "search_docs with no arguments returns a bounded schema-free catalogue window. A query ranks physical and Unified callables together and includes callable detail. An exact operationId remains available when its public ID is already known.",
  "An execute script's body should end with `return <value>` -- that value is what gets reported back as the tool result.",
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
          .describe("One exact advertised section: parameters, request, response:<status>, input, targets, or output."),
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
        "Run a short TypeScript script that can call one or more operations via call(operationId, params) " +
        "and chain their results, returning one final value. Fetch full schema via search_docs before " +
        "referencing an operationId for the first time. Unified operations use the exact documented ID " +
        "and params { input, targets, selectors?, pagination?, idempotencyKey? }.",
      inputSchema: {
        script: z
          .string()
          .describe(
            "TypeScript to run. Use `await call(operationId, params)` to invoke operations, " +
              "`session.get(key)`/`session.set(key, value)` for state across execute calls in this " +
              "session, and end with `return <value>` for the final result.",
          ),
      },
    },
    // The sandbox returns only trusted, already-bounded text, so the handler
    // cannot accidentally serialize user-controlled objects outside its deadline.
    async (args) => {
      const output = await runExecute(args.script, callOptions, session);
      return { content: [{ type: "text", text: output.text }], isError: output.isError };
    },
  );

  const transport = new StdioServerTransport();
  void server.connect(transport);
}

main();
