import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

const peopleRoute = readFileSync(new URL("../routes/integrations.access.people.tsx", import.meta.url), "utf8");

// Locks the list and drawer into separate layout surfaces.
test("people remain in a full-width list while details render in a drawer", () => {
  assert.doesNotMatch(peopleRoute, /lg:grid-cols-\[300px_1fr\]/);
  assert.match(peopleRoute, /onSelect=\{setSelectedId\} \/>/);
  assert.match(peopleRoute, /selectedId && <PersonDetailsDrawer/);
  assert.match(peopleRoute, /fixed inset-y-0 right-0/);
});

// Prevents data refreshes from silently choosing a person for the operator.
test("opening details requires a person selection", () => {
  assert.match(peopleRoute, /aria-label=\{`Open details for \$\{user\.display_name\}`\}/);
  assert.match(peopleRoute, /if \(preferredId && users\.some/);
  assert.match(peopleRoute, /return "";/);
  assert.doesNotMatch(peopleRoute, /return users\[0\]/);
});

// Preserves the accessible dialog contract used by keyboard and screen-reader users.
test("person details drawer exposes dialog and close semantics", () => {
  assert.match(peopleRoute, /role="dialog" aria-modal="true"/);
  assert.match(peopleRoute, /aria-labelledby="person-details-title"/);
  assert.match(peopleRoute, /aria-label="Close person details"/);
  assert.match(peopleRoute, /onClose=\{\(\) => setSelectedId\(""\)\}/);
});

// Keeps dense person controls wide while preventing a horizontal scrollbar.
test("person details use a wide drawer with vertical scrolling only", () => {
  assert.match(peopleRoute, /md:max-w-\[940px\] xl:max-w-\[1080px\]/);
  assert.match(peopleRoute, /overflow-y-auto overflow-x-hidden/);
  assert.doesNotMatch(peopleRoute, /role="tablist"/);
  assert.doesNotMatch(peopleRoute, /overflow-x-auto/);
});
