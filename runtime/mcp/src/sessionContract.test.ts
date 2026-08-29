import { expect, it } from "vitest";
import { inspectionScript, modelRecoveryFromUnknown, modelVisibleExecuteErrorCode, pageScript, recoveryForError, retainedContinuation, SESSION_AGENT_RULE, SESSION_CONTRACT_METADATA } from "./sessionContract.js";

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
    "complete_authentication", "continue_stored_result", "correct_execute_arguments", "adjust_result_projection", "reinitialize_connection", "do_not_replay",
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

// Local option validation has no bridge metadata, while Engine decisions require their validated structured response.
it("derives correction only for local pre-bridge pagination failures", () => {
  const correction = { recovery_action: "correct_execute_arguments", execute_request: "correct_arguments", provider_execution: "not_started" };
  expect(recoveryForError("MCP_CALL_OPTIONS_INVALID: bad options")).toMatchObject(correction);
  expect(recoveryForError("MCP_CALL_PAGINATION_INVALID: bad value")).toMatchObject(correction);
  expect(recoveryForError("mcp_pagination_not_supported: omit it")).toMatchObject({ recovery_action: "do_not_replay", provider_execution: "unknown" });
  expect(recoveryForError("mcp_physical_pagination_not_allowed_for_unified: omit it")).toMatchObject({ recovery_action: "do_not_replay", provider_execution: "unknown" });
  expect(recoveryForError("mcp_pagination_max_pages: traversal stopped")).toMatchObject({ recovery_action: "do_not_replay", provider_execution: "unknown" });
});

// Structured recovery is admitted as a whole closed contract rather than inferred from bridge prose.
it("validates bridge recovery fields before use", () => {
  const recovery = { recovery_action: "correct_execute_arguments", execute_request: "correct_arguments", provider_execution: "not_started", automatic_replay: false } as const;
  expect(modelRecoveryFromUnknown(recovery)).toEqual(recovery);
  expect(modelRecoveryFromUnknown({ ...recovery, provider_execution: "private-state" })).toBeUndefined();
  expect(modelRecoveryFromUnknown({ ...recovery, automatic_replay: true })).toBeUndefined();
});

// Only Engine-owned lowercase pagination prefixes may bypass the generic outer execute code.
it("preserves stable pagination codes without admitting arbitrary lowercase prefixes", () => {
  expect(modelVisibleExecuteErrorCode("mcp_pagination_not_supported: omit it")).toBe("mcp_pagination_not_supported");
  expect(modelVisibleExecuteErrorCode("mcp_physical_pagination_not_allowed_for_unified: omit it")).toBe("mcp_physical_pagination_not_allowed_for_unified");
  expect(modelVisibleExecuteErrorCode("MCP_CALL_OPTIONS_INVALID: bad options")).toBe("MCP_CALL_OPTIONS_INVALID");
  expect(modelVisibleExecuteErrorCode("mcp_pagination_continuation_invalid: contract mismatch")).toBe("mcp_pagination_continuation_invalid");
  expect(recoveryForError("mcp_pagination_continuation_invalid: contract mismatch")).toMatchObject({ recovery_action: "do_not_replay", provider_execution: "unknown" });
  expect(modelVisibleExecuteErrorCode("mcp_pagination_intent_invalid: future reason")).toBe("MCP_EXECUTE_FAILED");
  expect(recoveryForError("mcp_pagination_intent_invalid: future reason")).toMatchObject({ recovery_action: "do_not_replay", provider_execution: "unknown" });
  expect(modelVisibleExecuteErrorCode("provider_private_code: details")).toBe("MCP_EXECUTE_FAILED");
});
