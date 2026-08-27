import { expect, it } from "vitest";
import { inspectionScript, pageScript, recoveryForError, retainedContinuation, SESSION_AGENT_RULE, SESSION_CONTRACT_METADATA } from "./sessionContract.js";

// The public contract must teach both human-readable and machine-readable consumers without carrying an opaque ID.
it("advertises client-owned session state without exposing session identity", () => {
  expect(SESSION_AGENT_RULE).toContain("already attached");
  expect(SESSION_AGENT_RULE).toContain("Never invent");
  expect(SESSION_AGENT_RULE).toContain("recovery_action");
  expect(SESSION_CONTRACT_METADATA).toMatchObject({
    schema_version: 1,
    transport_session: "client_managed",
    session_id_input: false,
    script_scope: "current_mcp_connection",
    automatic_execute_replay: false,
  });
  expect(SESSION_CONTRACT_METADATA.recovery_actions).toEqual([
    "continue_stored_result", "correct_execute_arguments", "adjust_result_projection", "reinitialize_connection", "do_not_replay",
  ]);
  expect(JSON.stringify(SESSION_CONTRACT_METADATA)).not.toContain("session_id\":");
});

// Exact continuations keep result retrieval inside execute and cannot accidentally repeat a provider operation.
it("builds executable session-only continuation requests", () => {
  const reference = 'fused-result:quoted"value';
  const page = retainedContinuation(pageScript(reference, "/messages", ["id"], 0), 2048);
  expect(page).toMatchObject({ recovery_action: "continue_stored_result", execute_request: "use_next_request", provider_execution: "complete", automatic_replay: false });
  expect(page.session).toEqual({ scope: "current_mcp_connection", same_session_required: true });
  expect(page.next_request).toMatchObject({ tool: "execute", arguments: { outputBudgetBytes: 2048 } });
  expect(page.next_request.arguments.script).toBe(`return session.page(${JSON.stringify(reference)}, {path:"/messages",fields:["id"],offset:0});`);
  expect(page.next_request.arguments.script).not.toContain("call(");

  const inspect = inspectionScript(reference, "large value", 1024);
  expect(inspect).toContain("session.get");
  expect(inspect).toContain("adjust_result_projection");
  expect(inspect).not.toContain("call(");
});

// Recovery classification keeps argument corrections separate from reconnects and unknown outcomes.
it("maps runtime failures to closed model actions", () => {
  expect(recoveryForError("MCP_OUTPUT_BUDGET_INVALID: bad options")).toMatchObject({ recovery_action: "correct_execute_arguments", execute_request: "correct_arguments", provider_execution: "not_started" });
  expect(recoveryForError("MCP_RESULT_ROW_TOO_LARGE: narrow it")).toMatchObject({ recovery_action: "adjust_result_projection", execute_request: "adjust_projection", provider_execution: "complete" });
  expect(recoveryForError("MCP_BRIDGE_SESSION_UNAVAILABLE: gone")).toMatchObject({ recovery_action: "reinitialize_connection", execute_request: "reformat_if_session_state_used", provider_execution: "not_started" });
  expect(recoveryForError("MCP_BRIDGE_RESPONSE_INVALID: unknown")).toMatchObject({ recovery_action: "do_not_replay", execute_request: "do_not_replay", provider_execution: "unknown" });
});
