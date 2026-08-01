export const USER_SUMMARY_FIELDS = `id email display_name status owner_protected created_at updated_at`;
export const USER_FIELDS = `${USER_SUMMARY_FIELDS}
  memberships_truncated credentials_truncated
  memberships { team_id team_name team_slug membership_role created_at }
  credentials { id name key_prefix expires_at last_used_at revoked_at created_at }
`;
export const TEAM_MEMBER_FIELDS = `user_id email display_name status membership_role created_at`;
export const CREDENTIAL_FIELDS = `id name key_prefix expires_at last_used_at revoked_at created_at`;

export const PEOPLE_OPERATIONS = {
  list: `query Users($search: String!, $limit: Int!, $offset: Int!, $includeSuspended: Boolean!) {
    users(search: $search, limit: $limit, offset: $offset, include_suspended: $includeSuspended) {
      total items { ${USER_SUMMARY_FIELDS} }
    }
  }`,
  detail: `query User($id: ID!) { user(id: $id) { ${USER_FIELDS} } }`,
  create: userMutation("CreateUser", "createUser", "$input: CreateUserInput!", "input: $input"),
  update: userMutation("UpdateUser", "updateUser", "$id: ID!, $input: UpdateUserInput!", "id: $id, input: $input"),
  suspend: userMutation("SuspendUser", "suspendUser", "$id: ID!", "id: $id"),
  reactivate: userMutation("ReactivateUser", "reactivateUser", "$id: ID!", "id: $id"),
  issueCredential: `mutation IssueUserCredential($userId: ID!, $name: String!) {
    issueUserCredential(user_id: $userId, name: $name) {
      credential { ${CREDENTIAL_FIELDS} }
      secret authorization_revision changed
    }
  }`,
  revokeCredential: `mutation RevokeUserCredential($userId: ID!, $credentialId: ID!) {
    revokeUserCredential(user_id: $userId, credential_id: $credentialId) {
      credential { ${CREDENTIAL_FIELDS} }
      authorization_revision changed
    }
  }`,
  teamMembers: `query TeamMembers($teamId: ID!, $limit: Int!, $offset: Int!) {
    teamMembers(team_id: $teamId, limit: $limit, offset: $offset) {
      total items { ${TEAM_MEMBER_FIELDS} }
    }
  }`,
  addTeamMember: `mutation AddTeamMember($teamId: ID!, $email: String!, $role: TeamMembershipRole!) {
    addTeamMember(team_id: $teamId, email: $email, membership_role: $role) {
      membership { ${TEAM_MEMBER_FIELDS} }
      authorization_revision changed
    }
  }`,
  removeTeamMember: `mutation RemoveTeamMember($teamId: ID!, $userId: ID!) {
    removeTeamMember(team_id: $teamId, user_id: $userId) {
      membership { ${TEAM_MEMBER_FIELDS} }
      authorization_revision changed
    }
  }`,
} as const;

function userMutation(operation: string, field: string, variableTypes: string, args: string): string {
  return `mutation ${operation}(${variableTypes}) {
    ${field}(${args}) { user { ${USER_FIELDS} } authorization_revision changed }
  }`;
}
