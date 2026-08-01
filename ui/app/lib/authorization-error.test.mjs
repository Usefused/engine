import assert from "node:assert/strict";
import test from "node:test";

import {
  APIRequestError,
  advancedPermissionDiagnostics,
  apiErrorMessage,
  isAuthenticationFailure,
  normalizeAPIErrorPayload,
} from "./authorization-error.ts";

test("formats stable authentication errors", () => {
  const message = apiErrorMessage(401, { error: "authentication_required" });
  assert.match(message, /Authentication required/);
  assert.equal(
    isAuthenticationFailure(401, { error: "authentication_required" }),
    true
  );
});

test("formats permission details as product language without raw diagnostics", () => {
  const error = new APIRequestError(403, {
    error: "permission_denied",
    missing: [
      {
        permission: "bucket.values.read",
        resource_type: "bucket",
        resource_id: "11111111-1111-1111-1111-111111111111",
      },
    ],
  });

  assert.match(error.message, /You or the owning team need access to view the selected bucket/);
  assert.doesNotMatch(error.message, /bucket\.values\.read/);
  assert.doesNotMatch(error.message, /11111111-1111-1111-1111-111111111111/);
  assert.doesNotMatch(error.message, /production|secret/i);
  assert.equal(error.status, 403);
  assert.equal(error.missing.length, 1);
  assert.deepEqual(advancedPermissionDiagnostics(error.missing), [
    "bucket.values.read on bucket (11111111-1111-1111-1111-111111111111)",
  ]);
});

test("uses only a display name explicitly returned by the server", () => {
  const message = apiErrorMessage(403, {
    error: "permission_denied",
    missing: [
      {
        permission: "service.consume",
        resource_type: "service",
        resource_id: "22222222-2222-2222-2222-222222222222",
        display_name: "GitHub",
      },
    ],
  });

  assert.match(message, /use service "GitHub"/);
  assert.doesNotMatch(message, /service\.consume|22222222-2222-2222-2222-222222222222/);
});

test("keeps a permission denial actionable when details are omitted", () => {
  assert.equal(
    apiErrorMessage(403, { error: "permission_denied" }),
    "Permission denied. Ask a workspace administrator for access."
  );
});

test("normalizes null, scalar, and array error bodies", () => {
  for (const payload of [null, 7, "denied", ["permission_denied"]]) {
    const normalized = normalizeAPIErrorPayload(payload);
    assert.deepEqual(normalized, {});
    assert.equal(new APIRequestError(403, normalized).message, "This action is not available with your current workspace access.");
  }
});

test("filters null and partial missing requirements", () => {
  const payload = normalizeAPIErrorPayload({
    error: "permission_denied",
    missing: [
      null,
      {},
      { permission: "bucket.read" },
      {
        permission: "service.consume",
        resource_type: "service",
        resource_id: "33333333-3333-3333-3333-333333333333",
      },
    ],
  });
  const error = new APIRequestError(403, payload);

  assert.equal(error.missing.length, 1);
  assert.match(error.message, /use the selected service/);
  assert.doesNotMatch(error.message, /bucket\.read|undefined|null/);
});

test("explains missing or forged owner-team intent without internal identifiers", () => {
  assert.equal(
    apiErrorMessage(400, { error: "owner_team_id is required for a new artifact" }),
    "Choose an owning team before creating this SDK, MCP server, or webhook."
  );
  assert.match(apiErrorMessage(409, { error: "artifact owner team is immutable" }), /already belongs to another team/);
  assert.doesNotMatch(apiErrorMessage(409, { error: "artifact owner team is immutable" }), /owner_team_id|artifact\.manage/);
});

test("explains owning-team membership denial in plain language", () => {
  const message = apiErrorMessage(403, {
    error: "permission_denied",
    missing: [{ permission: "access.manage", resource_type: "workspace", resource_id: "33333333-3333-3333-3333-333333333333" }],
  });
  assert.match(message, /not a member of the owning team/);
  assert.doesNotMatch(message, /access\.manage|33333333/);
});

test("malformed missing entries fall back to generic permission guidance", () => {
  const payload = normalizeAPIErrorPayload({
    error: "permission_denied",
    missing: [null, {}, { resource_type: "bucket" }],
  });

  assert.equal(
    new APIRequestError(403, payload).message,
    "Permission denied. Ask a workspace administrator for access."
  );
});
