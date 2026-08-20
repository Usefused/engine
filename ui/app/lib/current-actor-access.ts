import { api } from "./api";
import type { CurrentActorAccess } from "./current-actor-permissions";

export {
  hasAnyPermission,
  hasResourcePermission,
  hasWorkspacePermission,
} from "./current-actor-permissions";
export type { CurrentActorAccess, CurrentActorGrant } from "./current-actor-permissions";

const CURRENT_ACTOR_ACCESS_QUERY = `query CurrentActorAccess {
  currentActorAccess {
    subject_id
    workspace_id
    kind
    authorization_revision
    grants { permission resource_type resource_id }
  }
}`;

export async function loadCurrentActorAccess(): Promise<CurrentActorAccess> {
  const data = await api.mcpGraphql<{ currentActorAccess: CurrentActorAccess }>(CURRENT_ACTOR_ACCESS_QUERY);
  return data.currentActorAccess;
}
