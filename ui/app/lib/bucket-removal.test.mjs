import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const routeSource = readFileSync(new URL("../routes/integrations.buckets.tsx", import.meta.url), "utf8");
const route = ts.createSourceFile("route.tsx", routeSource, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const functionNames = ["removeBucketSecret", "removeBucketValue", "removeBucketConnection", "connectedUserDeletePrompt", "withSaving"];
// Execute the actual route handlers with transport stubs, not a parallel confirmation implementation.
const handlerSource = route.statements.filter((node) => ts.isFunctionDeclaration(node) && functionNames.includes(node.name?.text)).map((node) => node.getText(route)).join("\n");
const javascript = ts.transpileModule(handlerSource, { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText;
const loadHandlers = new Function("api", "deleteBucketAuthConnection", "errorMessage", `${javascript}\nreturn { removeBucketSecret, removeBucketValue, removeBucketConnection };`);

const secret = { bucket_id: "bucket-a", service_id: "service-a", key_name: "username", key_names: ["username", "password"] };
const value = { bucket_id: "bucket-a", service_id: "service-a", key_name: "REGION", value: "never-repeat-this-value" };
const connection = { bucket_id: "bucket-a", id: "connection-a", end_user_ref: "customer-a" };
const cases = [
  { kind: "secret", handler: "removeBucketSecret", item: secret, answer: true, target: ["bucket-a", "service-a", ["username", "password"]] },
  { kind: "value", handler: "removeBucketValue", item: value, answer: true, target: ["bucket-a", "service-a", "REGION"] },
  { kind: "connection", handler: "removeBucketConnection", item: connection, answer: "customer-a", target: ["bucket-a", "connection-a"] },
];

/** Records confirmation, mutations, and UI effects using synthetic identities only. */
function harness(answer, fail = false) {
  const calls = { prompts: [], mutations: [], states: [], reloads: [], successes: [], errors: [] };
  // Simulate the existing transport boundary without touching any real credential.
  async function mutate(...args) {
    calls.mutations.push(args);
    // A failed request must restore busy state and never announce successful deletion.
    if (fail) throw new Error("synthetic failure");
  }
  // Hold confirmations open when a promise is supplied to test pre-consent behavior.
  async function ask(message, options) {
    calls.prompts.push({ message, options });
    return await answer;
  }
  const handlers = loadHandlers({ workspace: { deleteSecrets: mutate, deleteBucketValue: mutate } }, mutate, (_error, fallback) => fallback);
  return {
    calls,
    // Invoke the real handler with bounded observable state and notification callbacks.
    run(scenario) {
      return handlers[scenario.handler](scenario.item, (state) => calls.states.push(state), (id) => calls.reloads.push(id), {
        confirm: ask, prompt: ask,
        success: (message) => calls.successes.push(message),
        error: (message) => calls.errors.push(message),
      });
    },
  };
}

for (const scenario of cases) {
  // A pending confirmation must not set saving state or touch the backend.
  test(`${scenario.kind}: opening confirmation and cancelling perform no mutation`, async () => {
    let resolve;
    const pending = new Promise((done) => { resolve = done; });
    const check = harness(pending);
    const running = check.run(scenario);
    assert.equal(check.calls.prompts.length, 1);
    assert.deepEqual(check.calls.mutations, []);
    assert.deepEqual(check.calls.states, []);
    resolve(null);
    await running;
    assert.deepEqual(check.calls.mutations, []);
    assert.deepEqual(check.calls.reloads, []);
    assert.deepEqual(check.calls.successes, []);
    assert.deepEqual(check.calls.errors, []);
  });

  // Confirmation must preserve the exact bucket, service, and grouped credential keys.
  test(`${scenario.kind}: explicit confirmation removes exactly the selected target`, async () => {
    const check = harness(scenario.answer);
    await check.run(scenario);
    assert.deepEqual(check.calls.mutations, [scenario.target]);
    assert.deepEqual(check.calls.reloads, ["bucket-a"]);
    assert.equal(check.calls.successes.length, 1);
    assert.deepEqual(check.calls.states, scenario.kind === "connection" ? ["connection-a", null] : [true, false]);
    assert.doesNotMatch(check.calls.prompts[0].message, /never-repeat-this-value/);
  });

  // Errors must not leave the row busy or imply that backend deletion succeeded.
  test(`${scenario.kind}: deletion failure restores state without a success notification`, async () => {
    const check = harness(scenario.answer, true);
    await check.run(scenario);
    assert.equal(check.calls.mutations.length, 1);
    assert.equal(check.calls.errors.length, 1);
    assert.deepEqual(check.calls.successes, []);
    assert.deepEqual(check.calls.reloads, []);
    assert.deepEqual(check.calls.states, scenario.kind === "connection" ? ["connection-a", null] : [true, false]);
  });
}

// Non-boolean truthy results cannot accidentally grant permission to remove a secret or value.
test("secret and env confirmations fail closed for every non-true result", async () => {
  for (const scenario of cases.slice(0, 2)) {
    for (const answer of [false, undefined, "true", {}]) {
      const check = harness(answer);
      await check.run(scenario);
      assert.deepEqual(check.calls.mutations, []);
      assert.deepEqual(check.calls.states, []);
    }
  }
});

// Connected-user deletion deliberately requires an exact, non-empty reference.
test("connected users reject blank and mismatched confirmation", async () => {
  for (const answer of ["", "customer-b", " customer-a "]) {
    const check = harness(answer);
    await check.run(cases[2]);
    assert.deepEqual(check.calls.mutations, []);
    assert.equal(check.calls.errors.length, 1);
  }
  const empty = harness("");
  await empty.run({ ...cases[2], item: { ...connection, end_user_ref: "" } });
  assert.deepEqual(empty.calls.mutations, []);
});

// Legacy single-key rows and grouped rows must describe exactly what the mutation removes.
test("secret confirmation identifies grouped keys and preserves the single-key fallback", async () => {
  const grouped = harness(true);
  await grouped.run(cases[0]);
  assert.match(grouped.calls.prompts[0].message, /"username", "password"/);
  const single = harness(true);
  await single.run({ ...cases[0], item: { ...secret, key_names: [] } });
  assert.deepEqual(single.calls.mutations, [["bucket-a", "service-a", ["username"]]]);
});

// The three-dot trigger must remain a disclosure, never a direct destructive callback.
test("row options use a separate menu and all removal callbacks retain guarded handlers", () => {
  const list = readFileSync(new URL("../components/buckets/BucketEntryList.tsx", import.meta.url), "utf8");
  const menu = readFileSync(new URL("../components/buckets/BucketEntryMenu.tsx", import.meta.url), "utf8");
  assert.match(list, /actions\.canRemove && <BucketEntryMenu/);
  assert.doesNotMatch(list, /onClick=\{entry\.onRemove\}/);
  assert.match(menu, /aria-haspopup="menu"/);
  assert.match(menu, /role="menuitem" onClick=\{requestRemoval\}/);
  assert.match(routeSource, /removeBucketSecret\(item, setSaving, reloadBucketState, toast\)/);
  assert.match(routeSource, /removeBucketValue\(item, setSaving, reloadBucketState, toast\)/);
  assert.match(routeSource, /removeBucketConnection\(\s*item,/);
});
