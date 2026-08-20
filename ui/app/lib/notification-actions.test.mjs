import assert from "node:assert/strict";
import test from "node:test";

import { canMutateNotification } from "../components/notifications/notificationActions.ts";

test("notification controls require permission and an Engine-backed row", () => {
  assert.equal(canMutateNotification("engine", true), true);
  assert.equal(canMutateNotification("engine", false), false);
  assert.equal(canMutateNotification("registry", true), false);
  assert.equal(canMutateNotification("registry", false), false);
});
