#!/usr/bin/env node
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { loadFixture } from "./fixture.js";
import { searchDocs } from "./searchDocs.js";
import { callClientOptionsFromEnv } from "./callClient.js";
import { runExecute, SessionState } from "./sandbox.js";

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
  "Before referencing an operationId in a script for the first time, fetch its full schema with an operationId-mode search_docs call (not just a query-mode summary, which is deliberately schema-free and not safe to write a call against).",
  "search_docs with no arguments lists every available operation. search_docs with a query fuzzy-matches. search_docs with an operationId returns full request/response detail for exactly that operation.",
  "An execute script's body should end with `return <value>` -- that value is what gets reported back as the tool result.",
].join(" ");

function main(): void {
  const fixturePath = process.env.FUSED_FIXTURE_PATH;
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
        "List, fuzzy-search, or fetch full schema for operations available on this MCP server. " +
        "Call with no arguments to list everything. Call with `query` to fuzzy-search. Call with " +
        "`operationId` to get the full request/response schema for exactly that operation -- required " +
        "before referencing that operationId in an execute script.",
      inputSchema: {
        query: z
          .string()
          .optional()
          .describe("Fuzzy/keyword search across operation name, description, and path."),
        operationId: z
          .string()
          .optional()
          .describe("Exact operation ID to fetch full request/response schema for."),
      },
    },
    async (args) => {
      const result = searchDocs(fixture, args);
      return { content: [{ type: "text", text: JSON.stringify(result) }] };
    },
  );

  server.registerTool(
    "execute",
    {
      title: "Execute a script",
      description:
        "Run a short TypeScript script that can call one or more operations via call(operationId, params) " +
        "and chain their results, returning one final value. Fetch full schema via search_docs before " +
        "referencing an operationId for the first time.",
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
    async (args) => {
      const outcome = await runExecute(args.script, callOptions, session);
      if (outcome.error !== undefined) {
        return { content: [{ type: "text", text: outcome.error }], isError: true };
      }
      return { content: [{ type: "text", text: JSON.stringify(outcome.result ?? null) }] };
    },
  );

  const transport = new StdioServerTransport();
  void server.connect(transport);
}

main();
