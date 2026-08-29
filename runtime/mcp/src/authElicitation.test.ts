import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { ErrorCode, UrlElicitationRequiredError } from "@modelcontextprotocol/sdk/types.js";
import { describe, expect, it } from "vitest";
import { mcpAuthElicitationError } from "./authElicitation.js";

describe("mcpAuthElicitationError", () => {
  /** The standard MCP error retains Fused replay metadata without putting it in model-visible prose. */
  it("builds one URL elicitation with conservative recovery", () => {
    const error = mcpAuthElicitationError({
      action: "reconnect",
      url: "https://provider.example.com/oauth",
      elicitationId: "opaque-1",
      expiresAt: "2026-08-29T17:00:00Z",
      recovery: {
        recovery_action: "complete_authentication",
        execute_request: "do_not_replay",
        provider_execution: "unknown",
        automatic_replay: false,
      },
    });

    expect(error).toBeInstanceOf(UrlElicitationRequiredError);
    expect(error.code).toBe(ErrorCode.UrlElicitationRequired);
    expect(error.elicitations).toEqual([{
      mode: "url",
      message: "Reconnect your provider account to continue.",
      elicitationId: "opaque-1",
      url: "https://provider.example.com/oauth",
      _meta: {
        "com.usefused/auth": {
          schema_version: 1,
          action: "reconnect",
          expires_at: "2026-08-29T17:00:00Z",
          recovery_action: "complete_authentication",
          execute_request: "do_not_replay",
          provider_execution: "unknown",
          automatic_replay: false,
        },
      },
    }]);
  });

  /** A real MCP server boundary must preserve the standard JSON-RPC code and elicitation data seen by clients. */
  it("serializes URL elicitation through an MCP tool call", async () => {
    const server = new McpServer({ name: "fused-test", version: "1.0.0" });
    server.registerTool("execute", {}, async () => {
      // The production execute handler throws this typed error after Engine returns a trusted auth action.
      throw mcpAuthElicitationError({
        action: "connect",
        url: "https://provider.example.com/oauth",
        elicitationId: "opaque-2",
        expiresAt: "2026-08-29T18:00:00Z",
        recovery: {
          recovery_action: "complete_authentication",
          execute_request: "retry_after_auth",
          provider_execution: "not_started",
          automatic_replay: false,
        },
      });
    });
    const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
    const client = new Client(
      { name: "fused-test-client", version: "1.0.0" },
      { capabilities: { elicitation: { url: {} } } },
    );
    await server.connect(serverTransport);
    await client.connect(clientTransport);

    let failure: unknown;
    try {
      await client.callTool({ name: "execute", arguments: {} });
    } catch (error) {
      failure = error;
    }
    await client.close();

    expect(failure).toBeInstanceOf(UrlElicitationRequiredError);
    expect(failure).toMatchObject({
      code: ErrorCode.UrlElicitationRequired,
      data: {
        elicitations: [{
          mode: "url",
          message: "Connect your provider account to continue.",
          elicitationId: "opaque-2",
          url: "https://provider.example.com/oauth",
          _meta: {
            "com.usefused/auth": {
              schema_version: 1,
              action: "connect",
              expires_at: "2026-08-29T18:00:00Z",
              recovery_action: "complete_authentication",
              execute_request: "retry_after_auth",
              provider_execution: "not_started",
              automatic_replay: false,
            },
          },
        }],
      },
    });
  });
});
