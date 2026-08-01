import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { URL } from "node:url";
import test from "node:test";

const rootSource = readFileSync(new URL("../root.tsx", import.meta.url), "utf8");
const shellSource = readFileSync(new URL("../index.html", import.meta.url), "utf8");
const clientEntrySource = readFileSync(new URL("../entry.client.tsx", import.meta.url), "utf8");
const serverEntrySource = readFileSync(new URL("../entry.server.tsx", import.meta.url), "utf8");

test("hydrates only the stable SPA application root", () => {
  assert.doesNotMatch(rootSource, /export function Layout/);
  assert.match(rootSource, /export function HydrateFallback/);
  assert.doesNotMatch(rootSource, /dangerouslySetInnerHTML/);
  assert.match(shellSource, /<div id="app"><!-- Remix SPA --><\/div>/);
  assert.match(clientEntrySource, /hydrateRoot\(\s*app,/);
  assert.match(serverEntrySource, /shell\.replace\("<!-- Remix SPA -->", app\)/);
});
