import assert from "node:assert/strict";
import test from "node:test";

import { serviceDetailPath } from "./service-navigation.ts";

test("builds canonical service detail routes", () => {
  assert.equal(serviceDetailPath("service-id", "jira"), "/integrations/jira");
  assert.equal(
    serviceDetailPath("service-id", "@creative joe/jira cloud"),
    "/integrations/creative%20joe/jira%20cloud"
  );
  assert.equal(serviceDetailPath("service-id"), "/integrations/service-id");
});
