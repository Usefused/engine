import { describe, expect, it } from "vitest";
import {
  EXECUTE_ARGUMENT_DESCRIPTIONS,
  EXECUTE_TOOL_DESCRIPTION,
  MCP_SERVER_INSTRUCTIONS,
  SEARCH_DOCS_ARGUMENT_DESCRIPTIONS,
  SEARCH_DOCS_TOOL_DESCRIPTION,
} from "./toolDescriptions.js";

/** Counts the deterministic fixed prose injected before any tool result is returned. */
function totalCharacters(description: string, argumentsByName: Record<string, string>): number {
  return description.length + Object.values(argumentsByName).reduce((total, value) => total + value.length, 0);
}

describe("model-visible MCP guidance", () => {
  // Discovery guidance must stay compact while retaining the readiness handshake.
  it("keeps search_docs within its fixed context budget", () => {
    expect(totalCharacters(SEARCH_DOCS_TOOL_DESCRIPTION, SEARCH_DOCS_ARGUMENT_DESCRIPTIONS)).toBeLessThanOrEqual(900);
    expect(SEARCH_DOCS_TOOL_DESCRIPTION).toContain("execution_ready=true");
    expect(SEARCH_DOCS_TOOL_DESCRIPTION).toContain("next_action");
    expect(SEARCH_DOCS_TOOL_DESCRIPTION).toContain("params_schema");
  });

  // Execution guidance retains the non-replay and session boundaries without duplicating full recovery docs.
  it("keeps execute within its fixed context budget", () => {
    expect(totalCharacters(EXECUTE_TOOL_DESCRIPTION, EXECUTE_ARGUMENT_DESCRIPTIONS)).toBeLessThanOrEqual(1_500);
    expect(EXECUTE_TOOL_DESCRIPTION).toContain("execution_ready=true");
    expect(EXECUTE_TOOL_DESCRIPTION).toContain("pagination guidance");
    expect(EXECUTE_TOOL_DESCRIPTION).toContain("never replay it automatically");
    expect(EXECUTE_ARGUMENT_DESCRIPTIONS.script).toContain("Never pass a session ID");
  });

  // Initialization remains the one concise source of cross-tool policy and recovery rules.
  it("keeps server instructions bounded and safety-complete", () => {
    expect(MCP_SERVER_INSTRUCTIONS.length).toBeLessThanOrEqual(3_500);
    expect(MCP_SERVER_INSTRUCTIONS).toContain("next_action");
    expect(MCP_SERVER_INSTRUCTIONS).toContain("never invent or pass credentials");
    expect(MCP_SERVER_INSTRUCTIONS).toContain("provider_execution=complete");
    expect(MCP_SERVER_INSTRUCTIONS).toContain("never replay mutations automatically");
  });
});
