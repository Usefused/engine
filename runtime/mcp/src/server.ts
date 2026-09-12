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
import { EXECUTE_MIN_OUTPUT_BYTES, EXECUTE_VISIBLE_OUTPUT_POLICY } from "./resultBudget.js";
import {
  DOCUMENTATION_OUTPUT_POLICY,
  serializeBoundedJson,
} from "./outputLimits.js";
import { SESSION_CONTRACT_METADATA } from "./sessionContract.js";
import { mcpAuthElicitationError } from "./authElicitation.js";
import {
  EXECUTE_ARGUMENT_DESCRIPTIONS,
  EXECUTE_TOOL_DESCRIPTION,
  MCP_SERVER_INSTRUCTIONS,
  SEARCH_DOCS_ARGUMENT_DESCRIPTIONS,
  SEARCH_DOCS_TOOL_DESCRIPTION,
} from "./toolDescriptions.js";

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
    {
      name: fixture.server.name,
      title: fixture.server.title,
      version: fixture.server.version,
      description: fixture.server.description,
    },
    { instructions: MCP_SERVER_INSTRUCTIONS },
  );

  server.registerTool(
    "search_docs",
    {
      title: "Search available operations",
      description: SEARCH_DOCS_TOOL_DESCRIPTION,
      inputSchema: {
        query: z
          .string()
          .max(512)
          .optional()
          .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.query),
        operationId: z
          .string()
          .max(256)
          .optional()
          .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.operationId),
        limit: z
          .number()
          .int()
          .min(1)
          .max(SEARCH_DOCS_MAX_LIMIT)
          .optional()
          .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.limit),
        section: z
          .custom(isDocumentationSection, "invalid public documentation section")
          .optional()
          .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.section),
        schemaPath: z
          .string()
          .max(2048)
          .optional()
          .describe(SEARCH_DOCS_ARGUMENT_DESCRIPTIONS.schemaPath),
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
      description: EXECUTE_TOOL_DESCRIPTION,
      _meta: { "com.usefused/session": SESSION_CONTRACT_METADATA },
      inputSchema: {
        outputBudgetBytes: z.number().int().min(EXECUTE_MIN_OUTPUT_BYTES).max(EXECUTE_VISIBLE_OUTPUT_POLICY.maxBytes).optional()
          .describe(EXECUTE_ARGUMENT_DESCRIPTIONS.outputBudgetBytes),
        script: z
          .string()
          .describe(EXECUTE_ARGUMENT_DESCRIPTIONS.script),
      },
    },
    // The sandbox returns only trusted, already-bounded text, so the handler
    // cannot accidentally serialize user-controlled objects outside its deadline.
    async (args, extra) => {
      const output = await runExecute(args.script, callOptions, session, { ...DEFAULT_EXECUTE_LIMITS, outputBudgetBytes: args.outputBudgetBytes }, undefined, extra.signal);
      // URL mode keeps browser consent in the MCP host instead of exposing a navigation instruction to the model.
      if (output.authAction) {
        throw mcpAuthElicitationError(output.authAction);
      }
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
