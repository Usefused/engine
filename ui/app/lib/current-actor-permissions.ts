export interface CurrentActorGrant {
  permission: string;
  // GraphQL serializes ResourceType as an enum, so its wire values are
  // uppercase even though REST permission requirements use lowercase names.
  resource_type: "WORKSPACE" | "SERVICE" | "BUCKET" | "ARTIFACT";
  resource_id: string;
}

export interface CurrentActorAccess {
  subject_id: string;
  workspace_id: string;
  kind: string;
  authorization_revision: number;
  grants: CurrentActorGrant[];
}

export function hasWorkspacePermission(access: CurrentActorAccess | null, permission: string): boolean {
  if (!access) return false;
  // Resource-scoped grants must not unlock workspace administration. Match the
  // current workspace explicitly instead of treating any matching grant as a
  // workspace-wide permission.
  return access.grants.some((grant) =>
    grant.permission === permission &&
    grant.resource_type === "WORKSPACE" &&
    grant.resource_id === access.workspace_id
  );
}

export function hasAnyPermission(access: CurrentActorAccess | null, permission: string): boolean {
  // Resource pages may be useful with even one scoped grant; their server
  // queries still filter the rows and totals to the actor's authorized scope.
  return Boolean(access?.grants.some((grant) => grant.permission === permission));
}
