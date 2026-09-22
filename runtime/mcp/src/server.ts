#!/usr/bin/env node
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { SEARCH_DOCS_DEFINITION, EXECUTE_DEFINITION } from "./toolDefinitions.js";
import { loadFixture } from "./fixture.js";
import {
  DocumentationSection,
  searchDocs,
} from "./searchDocs.js";
import { callClientOptionsFromEnv, remoteSearch } from "./callClient.js";
import { DEFAULT_EXECUTE_LIMITS, runExecute, SessionState } from "./sandbox.js";
import {
  DOCUMENTATION_OUTPUT_POLICY,
  serializeBoundedJson,
} from "./outputLimits.js";
import { mcpAuthElicitationError } from "./authElicitation.js";
import {
  MCP_SERVER_INSTRUCTIONS,
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
    SEARCH_DOCS_DEFINITION,
    // Documentation is serialized at the handler boundary so every discovery
    // mode shares one wire-size policy without changing catalogue semantics.
    async (args, extra) => {
      // Opted-in compatibility sessions share Engine's validated classifier and bounded result path.
      if (fixture.server["fused-intelligent-classifier"] === true) {
        return remoteSearch(callOptions, args, extra.signal);
      }
      // MCP's schema inference erases custom Zod output types after the section predicate validates them.
      const result = searchDocs(fixture, { ...args, section: args.section as DocumentationSection | undefined });
      const output = serializeBoundedJson(result, DOCUMENTATION_OUTPUT_POLICY);
      return { content: [{ type: "text", text: output.text }], isError: output.isError };
    },
  );

  server.registerTool(
    "execute",
    EXECUTE_DEFINITION,
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
