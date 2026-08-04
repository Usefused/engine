import assert from "node:assert/strict";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { ArtifactOwnerControls } from "../components/access/ArtifactOwnerControls.ts";

test("renders owning team and intersection-only credential choices in product language", () => {
  const html = renderToStaticMarkup(createElement(ArtifactOwnerControls, {
    ownerTeams: [{ id: "team-1", name: "Support", slug: "support" }],
    ownerTeamId: "team-1",
    buckets: [{ resource_type: "BUCKET", resource_id: "bucket-1", display_name: "Support production" }],
    bucketId: "bucket-1",
    onOwnerTeamChange() {},
    onBucketChange() {},
  }));

  for (const label of ["Owning team", "Support", "Credential set", "Support production", "both your access and the owning team"] ) {
    assert.match(html, new RegExp(label, "i"));
  }
  assert.doesNotMatch(html, /permission|binding|actor_allowed|team_allowed/i);
});

test("explains personal ownership and shared credential choices", () => {
  const noTeams = renderToStaticMarkup(createElement(ArtifactOwnerControls, {
    ownerTeams: [], ownerTeamId: "", buckets: [], bucketId: "", onOwnerTeamChange() {}, onBucketChange() {},
  }));
	assert.match(noTeams, /Personal \(you\)/i);
	assert.match(noTeams, /personal ownership still works/i);

  const noBuckets = renderToStaticMarkup(createElement(ArtifactOwnerControls, {
    ownerTeams: [{ id: "team-1", name: "Support", slug: "support" }], ownerTeamId: "team-1", buckets: [], bucketId: "", onOwnerTeamChange() {}, onBucketChange() {},
  }));
  assert.match(noBuckets, /do not share access to a credential set/i);
});
