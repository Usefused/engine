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

  // Either model-visible surface must teach correct state use without requiring another documentation call.
  it.each([EXECUTE_TOOL_DESCRIPTION, MCP_SERVER_INSTRUCTIONS])("explains named keys and retained references in each guidance surface", (guidance) => {
    expect(guidance).toContain("Session helpers are synchronous");
    expect(guidance).toContain('session.set("post", value); later read: return session.get("post")');
    expect(guidance).toContain("session.get(key) requires exactly one string");
    expect(guidance).toContain("returned result_ref");
    expect(guidance).toContain("missing stored key returns undefined");
    expect(guidance).toContain("does not list keys");
    expect(guidance).toContain("Object.keys(session.get(key))");
    expect(guidance).toContain("accepts only retained result_ref values, not keys from session.set");
    expect(guidance).toContain("Inline results are not saved automatically");
    expect(guidance).toContain("not durable storage");
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
