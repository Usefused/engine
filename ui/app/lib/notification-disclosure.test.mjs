import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

// source reads one UI module relative to this test without relying on the runner's working directory.
function source(relativePath) {
  return readFileSync(new URL(relativePath, import.meta.url), "utf8");
}

// Contextual service and SDK notices must start compact and expand through a native disclosure.
test("contextual notification banners are collapsed by default", () => {
  const banner = source("../components/notifications/NotificationBanner.tsx");

  assert.match(banner, /const \[expanded, setExpanded\] = useState\(false\)/);
  assert.match(banner, /aria-expanded=\{expanded\}/);
  assert.match(banner, /aria-controls=\{contentID\}/);
  assert.match(banner, /\{expanded \? \([\s\S]*<NotificationList[\s\S]*dense/);
});

// Every detail-page notification surface consumes the shared collapsed banner.
test("service and SDK details share the contextual disclosure", () => {
  const service = source("../routes/integrations.$id.tsx");
  const sdk = source("../routes/integrations.sdks.$id.tsx");

  assert.match(service, /<NotificationBanner key=\{detail\.res\.service\.id\}/);
  assert.match(sdk, /<NotificationBanner\s+key=\{sdk\.app_id\}/);
});

// The canonical Notifications page remains expanded because reviewing notifications is its primary task.
test("the dedicated Notifications page renders its list without a disclosure", () => {
  const page = source("../components/notifications/NotificationsContent.tsx");

  assert.match(page, /<NotificationList/);
  assert.doesNotMatch(page, /NotificationBanner|<details/);
});

// The header bell remains closed until the user explicitly activates it.
test("the notification bell is not an always-expanded surface", () => {
  const bell = source("../components/notifications/NotificationBell.tsx");

  assert.match(bell, /useState\(false\)/);
  assert.match(bell, /open\s*&&/);
});
