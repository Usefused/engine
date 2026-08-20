import assert from "node:assert/strict";
import test from "node:test";

import { unwrapGraphQLResponse } from "./graphql-response.ts";

// This test keeps successful Registry and Engine GraphQL responses unchanged.
test("returns GraphQL data when the response has no errors", () => {
  assert.deepEqual(unwrapGraphQLResponse({ data: { service: { id: "svc" } } }), {
    service: { id: "svc" },
  });
});

// This test ensures concurrent schema validation failures are reported together.
test("aggregates every GraphQL error message", () => {
  assert.throws(
    () =>
      unwrapGraphQLResponse({
        data: null,
        errors: [
          { message: "Cannot query field scope" },
          { message: "Cannot query field retry_after" },
        ],
      }),
    /Cannot query field scope\nCannot query field retry_after/
  );
});

// This test ignores empty provider messages rather than emitting blank lines.
test("filters empty GraphQL error messages", () => {
  assert.throws(
    () =>
      unwrapGraphQLResponse({
        data: null,
        errors: [{ message: "  " }, { message: "Unauthorized" }],
      }),
    /^Error: Unauthorized$/
  );
});
