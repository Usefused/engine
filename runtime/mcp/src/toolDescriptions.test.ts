import { describe, expect, it } from "vitest";
import { EXECUTE_ARGUMENT_DESCRIPTIONS, EXECUTE_TOOL_DESCRIPTION, MCP_SERVER_INSTRUCTIONS, SEARCH_DOCS_ARGUMENT_DESCRIPTIONS, SEARCH_DOCS_TOOL_DESCRIPTION } from "./toolDescriptions.js";

/** Measures fixed prose separately from task-specific schemas and results. */
function totalCharacters(description: string, argumentsByName: Record<string, string>): number {
  return description.length + Object.values(argumentsByName).reduce((total, value) => total + value.length, 0);
}

describe("compact model-visible MCP guidance", () => {
  // One discovery result should suffice when the callable schema is complete.
  it("explains readiness without requiring redundant exact lookup", () => {
    expect(totalCharacters(SEARCH_DOCS_TOOL_DESCRIPTION, SEARCH_DOCS_ARGUMENT_DESCRIPTIONS)).toBeLessThan(800);
    expect(SEARCH_DOCS_TOOL_DESCRIPTION).toContain("execution_ready=true");
    expect(SEARCH_DOCS_TOOL_DESCRIPTION).toContain("no extra lookup");
    expect(SEARCH_DOCS_TOOL_DESCRIPTION).toContain("next_action");
    expect(SEARCH_DOCS_TOOL_DESCRIPTION).toContain("one concise operation intent");
    expect(SEARCH_DOCS_TOOL_DESCRIPTION).toContain("not a whole workflow");
  });
  // Composition must be taught in tool metadata without a user-supplied system prompt.
  it("teaches workflow composition and returning data over debug exceptions", () => {
    expect(totalCharacters(EXECUTE_TOOL_DESCRIPTION, EXECUTE_ARGUMENT_DESCRIPTIONS)).toBeLessThan(1200);
    expect(EXECUTE_TOOL_DESCRIPTION).toContain("operationId and params");
    expect(EXECUTE_TOOL_DESCRIPTION).toContain("do not throw");
    expect(EXECUTE_TOOL_DESCRIPTION).toContain("unknown outcome");
    expect(EXECUTE_TOOL_DESCRIPTION).toContain("one script for a multi-step task");
    expect(EXECUTE_TOOL_DESCRIPTION).toContain("stop writes, reconcile with reads");
    expect(EXECUTE_TOOL_DESCRIPTION).toContain("resume only unattempted steps");
    expect(EXECUTE_ARGUMENT_DESCRIPTIONS.script).toContain("Session helpers are synchronous");
    expect(EXECUTE_ARGUMENT_DESCRIPTIONS.saveAs).toContain("return session.get(key)");
  });
  // Standing policy remains short while still covering auth, state lifetime, pagination and replay.
  it("keeps initialization under a strict context budget", () => {
    expect(MCP_SERVER_INSTRUCTIONS.length).toBeLessThan(1000);
    expect(MCP_SERVER_INSTRUCTIONS).toContain("never invent or pass credentials");
    expect(MCP_SERVER_INSTRUCTIONS).toContain("never pass a session ID");
    expect(MCP_SERVER_INSTRUCTIONS).toContain("new connection cannot read earlier state");
    expect(MCP_SERVER_INSTRUCTIONS).toContain("exact operationId detail");
    expect(MCP_SERVER_INSTRUCTIONS).toContain("recovery_action");
    expect(MCP_SERVER_INSTRUCTIONS).toContain("unknown outcome");
    expect(MCP_SERVER_INSTRUCTIONS).toContain("continue only unattempted steps");
  });
});
