import assert from "node:assert/strict";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { AppOwnerControls } from "../components/access/AppOwnerControls.ts";

test("renders owning team and intersection-only credential choices in product language", () => {
  const html = renderToStaticMarkup(createElement(AppOwnerControls, {
    ownerTeams: [{ id: "team-1", name: "Support", slug: "support" }],
    ownerTeamId: "team-1",
    buckets: [{ resource_type: "BUCKET", resource_id: "bucket-1", display_name: "Support production" }],
    bucketId: "bucket-1",
    onOwnerTeamChange() {},
    onBucketChange() {},
    onCreateCredential() {},
  }));

  for (const label of ["Owning team", "Support", "Credential set", "Support production", "Create credential", "both your access and the owning team"] ) {
    assert.match(html, new RegExp(label, "i"));
  }
  assert.doesNotMatch(html, /permission|binding|actor_allowed|team_allowed/i);
  assert.match(html, /data-track="create_builder_credential"/);
  assert.match(html, /type="button"/);
});

test("explains personal ownership and shared credential choices", () => {
  const noTeams = renderToStaticMarkup(createElement(AppOwnerControls, {
    ownerTeams: [], ownerTeamId: "", buckets: [], bucketId: "", onOwnerTeamChange() {}, onBucketChange() {},
  }));
	assert.match(noTeams, /Personal \(you\)/i);
	assert.match(noTeams, /personal ownership still works/i);

  const noBuckets = renderToStaticMarkup(createElement(AppOwnerControls, {
    ownerTeams: [{ id: "team-1", name: "Support", slug: "support" }], ownerTeamId: "team-1", buckets: [], bucketId: "", onOwnerTeamChange() {}, onBucketChange() {},
  }));
  assert.match(noBuckets, /do not share access to a credential set/i);
});
