import assert from "node:assert/strict";
import test from "node:test";
import { createReferencedSecret } from "./secret-creation.ts";

// The form must store the exact value while returning only a portable reference to its parent.
test("secret creation preserves secret bytes and checks every metadata page", async () => {
  const reads = [], writes = [];
  const reference = await createReferencedSecret({ bucket: { id: "bucket-1", name: "billing" }, keyName: " signing_key ", value: "  exact-value\n" }, {
    // A partial first page verifies the name check does not assume the first page is complete.
    async read(bucketId, limit, offset) {
      reads.push([bucketId, limit, offset]);
      return { items: [{ key_name: `secret:existing-${offset}` }], total: 2 };
    },
    // Record the payload instead of contacting a credential store.
    async save(payload) { writes.push(payload); },
  });
  assert.equal(reference, "${bucket.billing.secret.signing_key}");
  assert.deepEqual(reads, [["bucket-1", 100, 0], ["bucket-1", 100, 1]]);
  assert.deepEqual(writes, [{ bucketId: "bucket-1", keyName: "signing_key", value: "  exact-value\n" }]);
});

// A create action must not knowingly overwrite a secret discovered beyond the first page.
test("duplicate names fail before the upsert endpoint", async () => {
  await assert.rejects(createReferencedSecret({ bucket: { id: "bucket-1", name: "billing" }, keyName: "signing_key", value: "new" }, {
    // Exercise both single-key and grouped metadata without retrieving secret values.
    async read(_bucketId, _limit, offset) {
      // The duplicate intentionally occurs only on the second page.
      return { items: [offset ? { key_name: "", key_names: ["secret:signing_key"], service_id: "00000000-0000-0000-0000-000000000000" } : { key_name: "secret:other" }], total: 2 };
    },
    // Any write would overwrite a credential instead of creating a new one.
    async save() { assert.fail("must not overwrite"); },
  }), /already exists/);
});

// Invalid reference segments cannot result in an unusable stored credential.
test("invalid names and empty values fail before network access", async () => {
  for (const change of [{ keyName: "a.b" }, { keyName: "two words" }, { keyName: "$key" }, { bucket: { id: "1", name: "a.b" } }, { keyName: "a}" }, { value: "" }]) {
    await assert.rejects(createReferencedSecret({ bucket: { id: "1", name: "billing" }, keyName: "key", value: "value", ...change }, {
      // Validation should precede metadata discovery.
      async read() { assert.fail("must not read"); },
      // Validation should precede persistence.
      async save() { assert.fail("must not write"); },
    }), /Choose a bucket|Enter a secret/);
  }
});

// Failed reads and writes never manufacture a successful reference or retry a mutation.
test("credential API failures stop creation", async () => {
  let writes = 0;
  const input = { bucket: { id: "1", name: "billing" }, keyName: "key", value: "value" };
  await assert.rejects(createReferencedSecret(input, {
    // A permissions failure must not be interpreted as an empty store.
    async read() { throw new Error("denied"); },
    // Count any accidental mutation after failed discovery.
    async save() { writes++; },
  }), /denied/);
  assert.equal(writes, 0);
  await assert.rejects(createReferencedSecret(input, {
    // An empty authorized store allows one explicit write attempt.
    async read() { return { items: [], total: 0 }; },
    // Transport uncertainty must propagate without an automatic retry.
    async save() { writes++; throw new Error("offline"); },
  }), /offline/);
  assert.equal(writes, 1);
});
