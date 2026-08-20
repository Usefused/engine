import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

import {
  generatedMethodPath,
  typescriptSDKCallExample,
} from "./sdk-code-example.ts";

const endpoint = {
  id: "endpoint",
  service_id: "service",
  name: "3dsChallengeResultPost",
  description: "Inform challenge result",
  resource: "profiles",
  version: "1",
  method: "POST",
  path: "/profiles/{profileId}/3dsecure/challenge-result",
  parameters: [
    { name: "profileId", in: "path", required: true, type: "integer", description: "" },
    { name: "X-External-Correlation-Id", in: "header", required: false, type: "string", description: "" },
  ],
  responses: {},
  spec_hash: "hash",
  created_at: "",
  updated_at: "",
  status: "active",
};

const endpointSidebarSource = readFileSync(
  new URL("../components/EndpointDetailsSidebar.tsx", import.meta.url),
  "utf8"
);

// This test locks the displayed call path to the generator's numeric operation
// identifier rule while preserving the exact parameter keys.
test("renders a valid generated TypeScript call for a numeric operationId", () => {
  assert.equal(
    generatedMethodPath("Transfer Wise", endpoint),
    "sdk.TransferWise.profiles.operation3dsChallengeResultPost"
  );
  assert.equal(
    typescriptSDKCallExample("Transfer Wise", endpoint),
    `const result = await sdk.TransferWise.profiles.operation3dsChallengeResultPost({
  "profileId": 123,
  "X-External-Correlation-Id": "string", // optional
});`
  );
});

// This test keeps request-body and root-payload examples aligned with generated
// method signatures without copying source examples into the browser.
test("includes required object fields and separates array payload roots", () => {
  assert.match(
    typescriptSDKCallExample("Wise", endpoint, {
      type: "object",
      required: ["challengeResult"],
      properties: { challengeResult: { type: "string" } },
    }),
    /"challengeResult": "string"/
  );
  assert.match(
    typescriptSDKCallExample("Wise", { ...endpoint, parameters: [] }, { type: "array", items: { type: "string" } }),
    /operation3dsChallengeResultPost\(\[\]\);/
  );
});

// This contract keeps the labelled operation identity and copyable generated
// example attached to the endpoint detail surface that users inspect.
test("endpoint details labels operationId and renders the generated example", () => {
  assert.match(endpointSidebarSource, /\["Operation ID", endpoint\.name\]/);
  assert.match(endpointSidebarSource, /<GeneratedSDKExample/);
  assert.match(endpointSidebarSource, /Copy generated SDK example/);
  assert.match(endpointSidebarSource, /typescriptSDKCallExample\(serviceName, endpoint, requestSchema\)/);
});
