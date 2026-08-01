import assert from "node:assert/strict";
import test from "node:test";

import { PEOPLE_OPERATIONS } from "./people-contract.ts";

test("uses exact people read fields without a duplicate subject id", () => {
  assert.match(PEOPLE_OPERATIONS.list, /users\(search: \$search, limit: \$limit, offset: \$offset, include_suspended: \$includeSuspended\)/);
  assert.match(PEOPLE_OPERATIONS.detail, /user\(id: \$id\)/);
  for (const field of ["id", "email", "display_name", "status", "owner_protected", "memberships", "memberships_truncated", "credentials", "credentials_truncated", "key_prefix", "last_used_at", "revoked_at"]) {
    assert.match(PEOPLE_OPERATIONS.detail, new RegExp(`\\b${field}\\b`));
  }
  assert.doesNotMatch(PEOPLE_OPERATIONS.detail, /subject_id|secret/);
});

test("reads the server-computed Owner protection flag", () => {
  assert.match(PEOPLE_OPERATIONS.detail, /\bowner_protected\b/);
  assert.doesNotMatch(PEOPLE_OPERATIONS.detail, /userEffectiveAccess/);
});

test("uses exact user and credential mutations", () => {
  assert.match(PEOPLE_OPERATIONS.create, /\$input: CreateUserInput!/);
  assert.match(PEOPLE_OPERATIONS.create, /createUser\(input: \$input\)/);
  assert.match(PEOPLE_OPERATIONS.update, /\$input: UpdateUserInput!/);
  assert.match(PEOPLE_OPERATIONS.suspend, /suspendUser\(id: \$id\)/);
  assert.match(PEOPLE_OPERATIONS.reactivate, /reactivateUser\(id: \$id\)/);
  assert.match(PEOPLE_OPERATIONS.issueCredential, /issueUserCredential\(user_id: \$userId, name: \$name\)/);
  assert.match(PEOPLE_OPERATIONS.issueCredential, /credential \{[^}]*key_prefix[^}]*\}\s*secret authorization_revision changed/s);
  assert.match(PEOPLE_OPERATIONS.revokeCredential, /revokeUserCredential\(user_id: \$userId, credential_id: \$credentialId\)/);
  assert.doesNotMatch(PEOPLE_OPERATIONS.revokeCredential, /\bsecret\b/);
});

test("uses exact flat team membership contract", () => {
  assert.match(PEOPLE_OPERATIONS.teamMembers, /teamMembers\(team_id: \$teamId, limit: \$limit, offset: \$offset\)/);
  assert.match(PEOPLE_OPERATIONS.teamMembers, /user_id email display_name status membership_role created_at/);
  assert.match(PEOPLE_OPERATIONS.addTeamMember, /\$role: TeamMembershipRole!/);
  assert.match(PEOPLE_OPERATIONS.addTeamMember, /addTeamMember\(team_id: \$teamId, email: \$email, membership_role: \$role\)/);
  assert.match(PEOPLE_OPERATIONS.removeTeamMember, /removeTeamMember\(team_id: \$teamId, user_id: \$userId\)/);
});
