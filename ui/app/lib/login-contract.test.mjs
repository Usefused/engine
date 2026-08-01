import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { URL } from "node:url";
import test from "node:test";

const loginSource = readFileSync(new URL("../routes/login.tsx", import.meta.url), "utf8");
const apiSource = readFileSync(new URL("./api.ts", import.meta.url), "utf8");

test("validates login with a permission shared by every workspace role", () => {
  assert.match(loginSource, /await api\.getAccount\(\)/);
  assert.doesNotMatch(loginSource, /query \{ sdks/);
});

test("keeps embedded API requests on the Engine origin", () => {
  assert.match(apiSource, /__FUSED_ENV\?\.BACKEND_URL/);
  assert.match(apiSource, /return "";/);
  assert.doesNotMatch(apiSource, /return "http:\/\/localhost:8081"/);
});
