import { api } from "./api";
import { readAllBoundedPages, type BoundedPage } from "./bounded-pages";
import { TEAM_OPERATIONS } from "./teams-contract";

export type TeamStatus = "active" | "archived";
export type TeamWorkspaceRole = "OWNER" | "ADMIN" | "BUILDER" | "VIEWER";
export type TeamAccessLevel = "USER" | "MANAGER";
export type TeamAppAccessLevel = "READER" | "MANAGER";
export type TeamResourceType = "service" | "bucket";

export interface TeamBinding {
  id: string;
  team_id: string;
  role_slug: string;
  role_display_name: string;
  resource_type: string;
  resource_id: string;
  resource_display_name: string;
  created_at: string;
}

export interface Team {
  id: string;
  name: string;
  slug: string;
  description: string;
  status: TeamStatus;
  bindings: TeamBinding[];
  created_at: string;
  updated_at: string;
}

export interface TeamPage {
  items: Team[];
  total: number;
}

export interface TeamMutationPayload {
  team: Team;
  authorization_revision: number;
  changed: boolean;
}

export interface TeamBindingMutationPayload {
  binding: TeamBinding | null;
  authorization_revision: number;
  changed: boolean;
}

export interface TeamEditorData {
  team: Team;
  services: Array<{ id: string; name: string }>;
  buckets: Array<{ id: string; name: string }>;
}

export interface TeamEditorAccess {
  services: boolean;
  buckets: boolean;
}

type TeamResource = { id: string; name: string };
type TeamResourcePage = BoundedPage<TeamResource>;

const TEAM_RESOURCE_PAGE_SIZE = 100;
const TEAM_RESOURCE_MAX_PAGES = 1000;

export async function listTeams(search = "", includeArchived = false): Promise<TeamPage> {
  const data = await api.mcpGraphql<{ teams: TeamPage }>(TEAM_OPERATIONS.list, {
    search,
    limit: 50,
    offset: 0,
    includeArchived,
  });
  return data.teams;
}

/** Loads team metadata and only the independently authorized selector lists. */
export async function loadTeamEditor(teamId: string, access: TeamEditorAccess): Promise<TeamEditorData> {
  // Separate roots keep service or bucket visibility from denying the team
  // itself; the fixed reads remain parallel and never fan out per resource.
  const [team, services, buckets] = await Promise.all([
    loadTeam(teamId),
    access.services ? loadTeamServices() : Promise.resolve([]),
    access.buckets ? loadTeamBuckets() : Promise.resolve([]),
  ]);
  return { team, services, buckets };
}

/** Loads one team without coupling it to resource catalogue permissions. */
async function loadTeam(teamId: string): Promise<Team> {
  const data = await api.mcpGraphql<{ team: Team | null }>(TEAM_OPERATIONS.editor, { id: teamId });
  if (!data.team) throw new Error("Team not found.");
  return data.team;
}

/** Loads every visible service in bounded pages for the access editor. */
function loadTeamServices(): Promise<TeamResource[]> {
  return readAllBoundedPages(async (limit, offset) => {
    const data = await api.mcpGraphql<{
      workspaceServicePage: {
        data: Array<{ service_id: string; service_name: string }>;
        total: number;
      };
    }>(TEAM_OPERATIONS.services, { limit, offset });
    return {
      items: data.workspaceServicePage.data.map((service) => ({
        id: service.service_id,
        name: service.service_name,
      })),
      total: data.workspaceServicePage.total,
    };
  }, TEAM_RESOURCE_PAGE_SIZE, TEAM_RESOURCE_MAX_PAGES);
}

/** Loads every visible bucket in bounded pages for the access editor. */
function loadTeamBuckets(): Promise<TeamResource[]> {
  return readAllBoundedPages(async (limit, offset) => {
    const data = await api.mcpGraphql<{
      bucketSummaryPage: TeamResourcePage;
    }>(TEAM_OPERATIONS.buckets, { limit, offset });
    return data.bucketSummaryPage;
  }, TEAM_RESOURCE_PAGE_SIZE, TEAM_RESOURCE_MAX_PAGES);
}

export async function createTeam(input: { name: string; description?: string }): Promise<TeamMutationPayload> {
  const data = await api.mcpGraphql<{ createTeam: TeamMutationPayload }>(TEAM_OPERATIONS.create, { input });
  return data.createTeam;
}

export async function updateTeam(id: string, input: { name?: string; slug?: string; description?: string }): Promise<TeamMutationPayload> {
  const data = await api.mcpGraphql<{ updateTeam: TeamMutationPayload }>(TEAM_OPERATIONS.update, { id, input });
  return data.updateTeam;
}

export async function archiveTeam(id: string): Promise<TeamMutationPayload> {
  const data = await api.mcpGraphql<{ archiveTeam: TeamMutationPayload }>(TEAM_OPERATIONS.archive, { id });
  return data.archiveTeam;
}

export async function setTeamWorkspaceRole(teamId: string, role: TeamWorkspaceRole | null): Promise<TeamBindingMutationPayload> {
  const data = await api.mcpGraphql<{ setTeamWorkspaceRole: TeamBindingMutationPayload }>(TEAM_OPERATIONS.workspaceRole, { teamId, role });
  return data.setTeamWorkspaceRole;
}

export async function changeTeamResourceAccess(
  action: "grant" | "revoke",
  resourceType: TeamResourceType,
  teamId: string,
  resourceId: string,
  level: TeamAccessLevel
): Promise<TeamBindingMutationPayload> {
  const operationKey = `${action}${capitalize(resourceType)}` as "grantService" | "revokeService" | "grantBucket" | "revokeBucket";
  const field = `${action}Team${capitalize(resourceType)}Access` as keyof TeamAccessMutationData;
  const data = await api.mcpGraphql<TeamAccessMutationData>(TEAM_OPERATIONS[operationKey], { teamId, resourceId, level });
  return data[field];
}

export async function changeTeamAppAccess(
  action: "grant" | "revoke",
  teamId: string,
  appFamilyId: string,
  level: TeamAppAccessLevel
): Promise<TeamBindingMutationPayload> {
  const operationKey = action === "grant" ? "grantApp" : "revokeApp";
  const field = action === "grant" ? "grantTeamAppAccess" : "revokeTeamAppAccess";
  const data = await api.mcpGraphql<TeamAppAccessMutationData>(TEAM_OPERATIONS[operationKey], {
    teamId,
    resourceId: appFamilyId,
    level,
  });
  return data[field];
}

type TeamAccessMutationData = {
  grantTeamServiceAccess: TeamBindingMutationPayload;
  revokeTeamServiceAccess: TeamBindingMutationPayload;
  grantTeamBucketAccess: TeamBindingMutationPayload;
  revokeTeamBucketAccess: TeamBindingMutationPayload;
};

type TeamAppAccessMutationData = {
  grantTeamAppAccess: TeamBindingMutationPayload;
  revokeTeamAppAccess: TeamBindingMutationPayload;
};

function capitalize(value: TeamResourceType): "Service" | "Bucket" {
  return value === "service" ? "Service" : "Bucket";
}
