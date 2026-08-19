import assert from "node:assert/strict";
import test from "node:test";

import {
  APIRequestError,
  advancedPermissionDiagnostics,
  apiErrorMessage,
  isAuthenticationFailure,
  isImportVersionRequired,
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

test("explains invalid app ownership without internal identifiers", () => {
	assert.equal(
		apiErrorMessage(400, { error: "owner team was not found or is archived" }),
		"Choose an active team, or use personal ownership."
	);
	assert.match(apiErrorMessage(409, { error: "app owner is immutable" }), /already has an owner/);
	assert.doesNotMatch(apiErrorMessage(409, { error: "app owner is immutable" }), /owner_team_id|app\.manage/);
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

test("keeps Engine-authored bucket readiness errors actionable", () => {
  const message = apiErrorMessage(400, {
    error: "The selected credential set is missing required authentication material.",
    code: "bucket_credentials_missing",
    category: "validation",
    retryable: false,
    details: {
      missing: [
        "11111111-1111-1111-1111-111111111111 (basic:jira_username)",
        "11111111-1111-1111-1111-111111111111 (basic:jira_password)",
      ],
    },
    remediation: "Add the required credentials and create the plan again.",
  });

  assert.match(message, /selected credential set is missing/i);
  assert.match(message, /Basic auth credential jira_username/);
  assert.match(message, /Basic auth credential jira_password/);
  assert.doesNotMatch(message, /11111111|bucket_readiness/);
  assert.match(message, /create the plan again/);
});

test("preserves structured Engine errors and diagnostics", () => {
  const payload = normalizeAPIErrorPayload({
    error: {
      message: "The Registry could not complete SDK generation.",
      code: "registry_request_failed",
      category: "dependency",
      retryable: true,
      details: { http_status: 503 },
      trace_id: "0123456789abcdef0123456789abcdef",
    },
  });
  const error = new APIRequestError(503, payload);

  assert.equal(error.message, "The Registry could not complete SDK generation.");
  assert.equal(error.code, "registry_request_failed");
  assert.equal(error.category, "dependency");
  assert.equal(error.retryable, true);
  assert.deepEqual(error.details, { http_status: 503 });
  assert.equal(error.traceId, "0123456789abcdef0123456789abcdef");
});

// This recovery test keeps the UI keyed to bounded server identity rather than
// mutable or potentially unsafe error wording.
test("preserves the version-required code for import recovery", () => {
  const payload = normalizeAPIErrorPayload({
    error: {
      message: "The imported source does not declare a version.",
      code: "import_version_required",
      category: "validation",
      retryable: false,
      remediation: "Enter a version and try again.",
    },
  });
  const error = new APIRequestError(400, payload);

  assert.equal(isImportVersionRequired(error), true);
  assert.equal(
    error.message,
    "The imported source does not declare a version. Enter a version and try again."
  );
  assert.equal(error.category, "validation");
  assert.equal(error.retryable, false);
  assert.equal(
    isImportVersionRequired(new Error("version is required when the imported source does not declare one")),
    false
  );
  assert.equal(
    isImportVersionRequired(new APIRequestError(400, { error: "other", code: "other" })),
    false
  );
});

test("does not treat a removed top-level plan error shape as structured", () => {
  const payload = normalizeAPIErrorPayload({
    error: "The Registry could not complete SDK generation.",
    code: "registry_request_failed",
    category: "dependency",
  });

  assert.equal(payload.code, undefined);
  assert.equal(payload.category, undefined);
});

test("does not echo arbitrary upstream errors", () => {
  const message = apiErrorMessage(400, {
    error: "provider failed with Authorization: Bearer secret-value",
  });

  assert.equal(message, "Fused could not use this request. Check the selected team and inputs.");
  assert.doesNotMatch(message, /secret-value/);
});
