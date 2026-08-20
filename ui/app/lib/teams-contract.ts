export const TEAM_FIELDS = `
  id name slug description status created_at updated_at
  bindings {
    id team_id role_slug role_display_name resource_type resource_id
    resource_display_name created_at
  }
`;

export const TEAM_OPERATIONS = {
  list: `
    query Teams($search: String!, $limit: Int!, $offset: Int!, $includeArchived: Boolean!) {
      teams(search: $search, limit: $limit, offset: $offset, include_archived: $includeArchived) {
        total
        items { ${TEAM_FIELDS} }
      }
    }
  `,
  editor: `
    query TeamAccessEditor($id: ID!) {
      team(id: $id) { ${TEAM_FIELDS} }
    }
  `,
  services: `
    query TeamAccessServices($limit: Int!, $offset: Int!) {
      workspaceServicePage(limit: $limit, offset: $offset) {
        total
        data { service_id service_name }
      }
    }
  `,
  buckets: `
    query TeamAccessBuckets($limit: Int!, $offset: Int!) {
      bucketSummaryPage(limit: $limit, offset: $offset) {
        total
        items { id name }
      }
    }
  `,
  create: `
    mutation CreateTeam($input: CreateTeamInput!) {
      createTeam(input: $input) {
        team { ${TEAM_FIELDS} }
        authorization_revision changed
      }
    }
  `,
  update: `
    mutation UpdateTeam($id: ID!, $input: UpdateTeamInput!) {
      updateTeam(id: $id, input: $input) {
        team { ${TEAM_FIELDS} }
        authorization_revision changed
      }
    }
  `,
  archive: `
    mutation ArchiveTeam($id: ID!) {
      archiveTeam(id: $id) {
        team { ${TEAM_FIELDS} }
        authorization_revision changed
      }
    }
  `,
  workspaceRole: `
    mutation SetTeamWorkspaceRole($teamId: ID!, $role: TeamWorkspaceRole) {
      setTeamWorkspaceRole(team_id: $teamId, role: $role) {
        binding { id team_id role_slug role_display_name resource_type resource_id resource_display_name created_at }
        authorization_revision changed
      }
    }
  `,
  grantService: bindingOperation("GrantTeamServiceAccess", "grantTeamServiceAccess", "service_id"),
  revokeService: bindingOperation("RevokeTeamServiceAccess", "revokeTeamServiceAccess", "service_id"),
  grantBucket: bindingOperation("GrantTeamBucketAccess", "grantTeamBucketAccess", "bucket_id"),
  revokeBucket: bindingOperation("RevokeTeamBucketAccess", "revokeTeamBucketAccess", "bucket_id"),
  grantApp: appBindingOperation("GrantTeamAppAccess", "grantTeamAppAccess"),
  revokeApp: appBindingOperation("RevokeTeamAppAccess", "revokeTeamAppAccess"),
} as const;

function bindingOperation(operation: string, field: string, resourceArgument: string): string {
  return `
    mutation ${operation}($teamId: ID!, $resourceId: ID!, $level: TeamAccessLevel!) {
      ${field}(team_id: $teamId, ${resourceArgument}: $resourceId, level: $level) {
        binding { id team_id role_slug role_display_name resource_type resource_id resource_display_name created_at }
        authorization_revision changed
      }
    }
  `;
}

function appBindingOperation(operation: string, field: string): string {
  return `
    mutation ${operation}($teamId: ID!, $resourceId: ID!, $level: TeamAppAccessLevel!) {
      ${field}(team_id: $teamId, app_family_id: $resourceId, level: $level) {
        binding { id team_id role_slug role_display_name resource_type resource_id resource_display_name created_at }
        authorization_revision changed
      }
    }
  `;
}
