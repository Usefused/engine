import assert from "node:assert/strict";
import test from "node:test";

import { openServiceLink, serviceDetailPath } from "./service-navigation.ts";

test("builds canonical service detail routes", () => {
  assert.equal(serviceDetailPath("service-id", "jira"), "/integrations/jira");
  assert.equal(
    serviceDetailPath("service-id", "@creative joe/jira cloud"),
    "/integrations/creative%20joe/jira%20cloud"
  );
  assert.equal(serviceDetailPath("service-id"), "/integrations/service-id");
});

// serviceClick models the cancellation state shared by a button and its bubbling row-link event.
function serviceClick(overrides = {}) {
  return {
    defaultPrevented: false,
    button: 0,
    metaKey: false,
    ctrlKey: false,
    shiftKey: false,
    altKey: false,
    // The same event object reaches the link after an action cancels its default navigation.
    preventDefault() { this.defaultPrevented = true; },
    ...overrides,
  };
}

// installTabOpener isolates browser effects while preserving any existing window fixture after the test.
function installTabOpener(t, open) {
  const previous = Object.getOwnPropertyDescriptor(globalThis, "window");
  Object.defineProperty(globalThis, "window", { configurable: true, value: { open } });
  // Restoring the descriptor keeps this test independent of other browser helpers.
  t.after(() => {
    // A pre-existing window must survive; an originally absent one must remain absent.
    if (previous) Object.defineProperty(globalThis, "window", previous);
    else delete globalThis.window;
  });
}

// A confirmation action cancels navigation synchronously even though its prompt resolves later.
test("a cancelled row action never opens the service tab", (t) => {
  const open = t.mock.fn();
  installTabOpener(t, open);
  const event = serviceClick();
  event.preventDefault();

  openServiceLink(event, "/integrations/jira");

  assert.equal(open.mock.callCount(), 0);
  assert.equal(event.defaultPrevented, true);
});

// Ordinary row clicks still use the existing authenticated-tab path exactly once.
test("a normal service click opens its detail tab and cancels duplicate native navigation", (t) => {
  const replace = t.mock.fn();
  const open = t.mock.fn(() => ({ opener: {}, location: { replace } }));
  installTabOpener(t, open);
  const event = serviceClick();

  openServiceLink(event, "/integrations/jira");

  assert.equal(open.mock.callCount(), 1);
  assert.deepEqual(replace.mock.calls[0].arguments, ["/integrations/jira"]);
  assert.equal(event.defaultPrevented, true);
});

// Browser-controlled modified clicks must not also trigger the custom popup path.
test("modified and non-primary service clicks retain native navigation", (t) => {
  const open = t.mock.fn();
  installTabOpener(t, open);
  for (const override of [{ button: 1 }, { button: 2 }, { metaKey: true }, { ctrlKey: true }, { shiftKey: true }, { altKey: true }]) {
    const event = serviceClick(override);
    openServiceLink(event, "/integrations/jira");
    assert.equal(event.defaultPrevented, false);
  }
  assert.equal(open.mock.callCount(), 0);
});

// Popup blocking must leave the existing native link fallback available.
test("a blocked authenticated tab does not suppress native navigation", (t) => {
  const open = t.mock.fn(() => null);
  installTabOpener(t, open);
  const event = serviceClick();

  openServiceLink(event, "/integrations/jira");

  assert.equal(open.mock.callCount(), 1);
  assert.equal(event.defaultPrevented, false);
});
