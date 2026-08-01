import { api } from "./api";
import { PEOPLE_OPERATIONS } from "./people-contract";

export type UserStatus = "INVITED" | "ACTIVE" | "SUSPENDED" | "ARCHIVED";
export type TeamMembershipRole = "MEMBER" | "MANAGER";

export interface UserTeamMembership {
  team_id: string;
  team_name: string;
  team_slug: string;
  membership_role: TeamMembershipRole;
  created_at: string;
}

export interface ControlCredential {
  id: string;
  name: string;
  key_prefix: string;
  expires_at: string | null;
  last_used_at: string | null;
  revoked_at: string | null;
  created_at: string;
}

export interface UserSummary {
  id: string;
  email: string;
  display_name: string;
  status: UserStatus;
	owner_protected: boolean;
	created_at: string;
	updated_at: string;
}

export interface User extends UserSummary {
  memberships: UserTeamMembership[];
  memberships_truncated: boolean;
  credentials: ControlCredential[];
  credentials_truncated: boolean;
}

export interface UserPage { items: UserSummary[]; total: number }
export interface UserMutationPayload { user: User; authorization_revision: number; changed: boolean }
export interface TeamMember {
  user_id: string;
  email: string;
  display_name: string;
  status: UserStatus;
  membership_role: TeamMembershipRole;
  created_at: string;
}
export interface TeamMemberPage { items: TeamMember[]; total: number }
export interface TeamMembershipMutationPayload { membership: TeamMember | null; authorization_revision: number; changed: boolean }
export interface IssuedCredentialPayload { credential: ControlCredential; secret: string; authorization_revision: number; changed: boolean }
export interface CredentialMutationPayload { credential: ControlCredential | null; authorization_revision: number; changed: boolean }

export async function listUsers(search = "", includeSuspended = false): Promise<UserPage> {
  const data = await api.mcpGraphql<{ users: UserPage }>(PEOPLE_OPERATIONS.list, { search, limit: 200, offset: 0, includeSuspended });
  return data.users;
}

export async function getUser(id: string): Promise<User> {
  const data = await api.mcpGraphql<{ user: User | null }>(PEOPLE_OPERATIONS.detail, { id });
  if (!data.user) throw new Error("Person not found.");
  return data.user;
}

export async function createUser(input: { email: string; display_name: string }): Promise<UserMutationPayload> {
  const data = await api.mcpGraphql<{ createUser: UserMutationPayload }>(PEOPLE_OPERATIONS.create, { input });
  return data.createUser;
}

export async function updateUser(id: string, input: { email?: string; display_name?: string }): Promise<UserMutationPayload> {
  const data = await api.mcpGraphql<{ updateUser: UserMutationPayload }>(PEOPLE_OPERATIONS.update, { id, input });
  return data.updateUser;
}

export async function changeUserStatus(id: string, action: "suspend" | "reactivate"): Promise<UserMutationPayload> {
  const key = action === "suspend" ? "suspend" : "reactivate";
  const field = action === "suspend" ? "suspendUser" : "reactivateUser";
  const data = await api.mcpGraphql<Record<string, UserMutationPayload>>(PEOPLE_OPERATIONS[key], { id });
  return data[field];
}

export async function issueUserCredential(userId: string, name: string): Promise<IssuedCredentialPayload> {
  const data = await api.mcpGraphql<{ issueUserCredential: IssuedCredentialPayload }>(PEOPLE_OPERATIONS.issueCredential, { userId, name });
  return data.issueUserCredential;
}

export async function revokeUserCredential(userId: string, credentialId: string): Promise<CredentialMutationPayload> {
  const data = await api.mcpGraphql<{ revokeUserCredential: CredentialMutationPayload }>(PEOPLE_OPERATIONS.revokeCredential, { userId, credentialId });
  return data.revokeUserCredential;
}

export async function listTeamMembers(teamId: string): Promise<TeamMemberPage> {
  const data = await api.mcpGraphql<{ teamMembers: TeamMemberPage }>(PEOPLE_OPERATIONS.teamMembers, { teamId, limit: 100, offset: 0 });
  return data.teamMembers;
}

export async function addTeamMember(teamId: string, email: string, role: TeamMembershipRole): Promise<TeamMembershipMutationPayload> {
  const data = await api.mcpGraphql<{ addTeamMember: TeamMembershipMutationPayload }>(PEOPLE_OPERATIONS.addTeamMember, { teamId, email, role });
  return data.addTeamMember;
}

export async function removeTeamMember(teamId: string, userId: string): Promise<TeamMembershipMutationPayload> {
  const data = await api.mcpGraphql<{ removeTeamMember: TeamMembershipMutationPayload }>(PEOPLE_OPERATIONS.removeTeamMember, { teamId, userId });
  return data.removeTeamMember;
}
