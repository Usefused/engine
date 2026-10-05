import assert from "node:assert/strict";
import test from "node:test";

import { GraphQLRequestError, unwrapGraphQLResponse } from "./graphql-response.ts";
import { REQUEST_ERROR_MESSAGE } from "./request-errors.ts";

// This test keeps successful Registry and Engine GraphQL responses unchanged.
test("returns GraphQL data when the response has no errors", () => {
  assert.deepEqual(unwrapGraphQLResponse({ data: { service: { id: "svc" } } }), {
    service: { id: "svc" },
  });
});

// Users get one readable failure while developers retain all incompatible fields for diagnosis.
test("keeps document diagnostics out of displayed GraphQL errors", () => {
  const errors = [{ message: "Cannot query field scope" }, { message: "Cannot query field retry_after" }];
  assert.throws(
    () =>
      unwrapGraphQLResponse({
        data: null,
        errors,
      }),
    // Rendering Error.message or String(error) must never reveal the document details.
    (error) => {
      assert.ok(error instanceof GraphQLRequestError);
      assert.equal(error.message, REQUEST_ERROR_MESSAGE);
      assert.doesNotMatch(String(error), /GraphQL|scope|retry_after/);
      assert.deepEqual(error.serverErrors, errors);
      return true;
    }
  );
});

// Stable parser codes work with new wording while an actionable error in the same response remains available.
test("maps coded document errors without swallowing business guidance or partial failures", () => {
  assert.throws(() => unwrapGraphQLResponse({ data: { partial: true }, errors: [
    { message: "Internal schema diagnostic", extensions: { code: "GRAPHQL_VALIDATION_FAILED" } },
    { message: "Choose a bucket before continuing." },
  ] }), { message: `${REQUEST_ERROR_MESSAGE}\nChoose a bucket before continuing.` });
});

// Raw parser errors may contain query text; it belongs only in retained diagnostics.
test("maps legacy parser and argument errors", () => {
  for (const message of ['Syntax Error: Expected Name, found }', 'Unknown argument "old_field" on field "Query.apps".', 'Field "apps" argument "id" of type "String!" is required.', 'Variable "$ids" got invalid value 1.']) {
    assert.throws(() => unwrapGraphQLResponse({ data: null, errors: [{ message }] }), { message: REQUEST_ERROR_MESSAGE });
  }
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
