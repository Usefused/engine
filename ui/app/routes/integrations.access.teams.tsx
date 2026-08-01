import { useCallback, useEffect, useState, type FormEvent } from "react";
import type { MetaFunction } from "@remix-run/react";
import { Users, Plus, Archive } from "lucide-react";
import { useToast } from "~/components/Toast";
import {
  TeamAccessControls,
  teamResourceLevels,
  teamWorkspaceRole,
} from "~/components/access/TeamAccessControls";
import { TeamMembersControls } from "~/components/access/TeamMembersControls";
import { WorkspacePermissionGate, useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import {
  addTeamMember,
  listTeamMembers,
  removeTeamMember,
  type TeamMember,
  type TeamMembershipRole,
} from "~/lib/people";
import {
  archiveTeam,
  changeTeamArtifactAccess,
  changeTeamResourceAccess,
  createTeam,
  listTeams,
  loadTeamEditor,
  setTeamWorkspaceRole,
  updateTeam,
  type Team,
  type TeamAccessLevel,
  type TeamArtifactAccessLevel,
  type TeamEditorData,
  type TeamResourceType,
  type TeamWorkspaceRole,
} from "~/lib/teams";

export const meta: MetaFunction = () => [{ title: "Teams - Fused" }];

export default function TeamsPage() {
	const { access } = useCurrentActorAccess();
	return <WorkspacePermissionGate permission="access.read" area="teams and workspace access">
		<TeamsManager canManage={hasWorkspacePermission(access, "access.manage")} canManageOwners={hasWorkspacePermission(access, "account.manage")} />
	</WorkspacePermissionGate>;
}

function TeamsManager({ canManage, canManageOwners }: { canManage: boolean; canManageOwners: boolean }) {
  const toast = useToast();
  const [teams, setTeams] = useState<Team[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const [editor, setEditor] = useState<TeamEditorData | null>(null);
  const [members, setMembers] = useState<TeamMember[]>([]);
  const [includeArchived, setIncludeArchived] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [newName, setNewName] = useState("");
  const [newDescription, setNewDescription] = useState("");
  const [editName, setEditName] = useState("");
  const [editDescription, setEditDescription] = useState("");

  const refreshTeams = useCallback(async (preferredId = "") => {
    const page = await listTeams("", includeArchived);
    setTeams(page.items);
    setSelectedId((current) => selectAvailableTeam(page.items, preferredId || current));
  }, [includeArchived]);

  const refreshEditor = useCallback(async () => {
    if (!selectedId) {
      setEditor(null);
      setMembers([]);
      return;
    }
    const [data, memberPage] = await Promise.all([loadTeamEditor(selectedId), listTeamMembers(selectedId)]);
    setEditor(data);
    setMembers(memberPage.items);
    setEditName(data.team.name);
    setEditDescription(data.team.description);
  }, [selectedId]);

  useEffect(() => {
    setLoading(true);
    refreshTeams().catch((error: unknown) => toast.error(errorMessage(error))).finally(() => setLoading(false));
  }, [refreshTeams]);

  useEffect(() => {
    refreshEditor().catch((error: unknown) => toast.error(errorMessage(error)));
  }, [refreshEditor]);

  async function handleCreate(event: FormEvent) {
    event.preventDefault();
    if (!newName.trim()) return;
    await runChange(async () => {
      const payload = await createTeam({ name: newName.trim(), description: newDescription.trim() });
      setNewName("");
      setNewDescription("");
      await refreshTeams(payload.team.id);
      toast.success("Team created.");
    });
  }

  async function handleUpdate(event: FormEvent) {
    event.preventDefault();
    if (!editor || !editName.trim()) return;
    await runChange(async () => {
      await updateTeam(editor.team.id, { name: editName.trim(), description: editDescription.trim() });
      await Promise.all([refreshTeams(editor.team.id), refreshEditor()]);
      toast.success("Team details saved.");
    });
  }

  async function handleArchive() {
    if (!editor || !window.confirm(`Archive ${editor.team.name}? Remove its access and reassign owned SDKs first.`)) return;
    await runChange(async () => {
      await archiveTeam(editor.team.id);
      await refreshTeams();
      toast.success("Team archived.");
    });
  }

  async function handleWorkspaceRole(role: TeamWorkspaceRole | null) {
    if (!editor) return;
    await runChange(async () => {
      await setTeamWorkspaceRole(editor.team.id, role);
      await refreshEditor();
      toast.success("Workspace role updated.");
    });
  }

  async function handleResourceAccess(resourceType: TeamResourceType, resourceId: string, level: TeamAccessLevel | null) {
    if (!editor) return;
    await runChange(async () => {
      await replaceResourceAccess(editor.team, resourceType, resourceId, level);
      await refreshEditor();
      toast.success("Team access updated.");
    });
  }

  async function handleArtifactAccess(artifactId: string, level: TeamArtifactAccessLevel | null) {
    if (!editor) return;
    await runChange(async () => {
      await replaceArtifactAccess(editor.team, artifactId, level);
      await refreshEditor();
      toast.success(level ? "Build shared with this team." : "Build access removed.");
    });
  }

  async function handleAddMember(email: string, role: TeamMembershipRole) {
    if (!editor) return;
    await runChange(async () => {
      const payload = await addTeamMember(editor.team.id, email, role);
      const page = await listTeamMembers(editor.team.id);
      setMembers(page.items);
      toast.success(payload.changed ? "Person added to the team." : "Team membership is already up to date.");
    });
  }

  async function handleRemoveMember(userId: string) {
    if (!editor) return;
    await runChange(async () => {
      await removeTeamMember(editor.team.id, userId);
      const page = await listTeamMembers(editor.team.id);
      setMembers(page.items);
      toast.success("Person removed from the team.");
    });
  }

  async function runChange(change: () => Promise<void>) {
    setSaving(true);
    try {
      await change();
    } catch (error: unknown) {
      // APIRequestError carries the server's safe permission guidance, so the
      // page never replaces an actionable denial with a generic failure.
      toast.error(errorMessage(error));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-6">
      <header>
        <p className="text-sm font-medium text-blue-600">Access</p>
        <h1 className="text-2xl font-bold text-slate-900 flex items-center gap-2"><Users className="w-6 h-6" /> Teams</h1>
        <p className="text-slate-500 mt-1">Give groups a workspace role and simple Use or Manage access.</p>
      </header>

      <TeamCreateControls canManage={canManage} name={newName} description={newDescription} saving={saving} onName={setNewName} onDescription={setNewDescription} onCreate={handleCreate} />

      <div className="grid gap-6 lg:grid-cols-[280px_1fr]">
        <TeamsList teams={teams} selectedId={selectedId} includeArchived={includeArchived} loading={loading} onIncludeArchived={setIncludeArchived} onSelect={setSelectedId} />
        <TeamEditorPanel editor={editor} members={members} name={editName} description={editDescription} saving={saving} canManage={canManage} canManageOwners={canManageOwners} onName={setEditName} onDescription={setEditDescription} onUpdate={handleUpdate} onArchive={handleArchive} onWorkspaceRole={handleWorkspaceRole} onResourceAccess={handleResourceAccess} onArtifactAccess={handleArtifactAccess} onAddMember={handleAddMember} onRemoveMember={handleRemoveMember} />
      </div>
    </div>
  );
}

function TeamCreateControls(props: { canManage: boolean; name: string; description: string; saving: boolean; onName: (value: string) => void; onDescription: (value: string) => void; onCreate: (event: FormEvent) => void }) {
  if (!props.canManage) return <p className="rounded-lg border border-slate-200 bg-white px-4 py-3 text-sm text-slate-600" role="status">You have read-only access to teams.</p>;
  return <section className="bg-white border border-slate-200 rounded-xl p-5 shadow-sm"><h2 className="text-base font-semibold text-slate-900 mb-3">Create a team</h2><form onSubmit={props.onCreate} className="grid gap-3 md:grid-cols-[1fr_2fr_auto]" toolname="create_team" tooldescription="Create a workspace team."><input value={props.name} onChange={(event) => props.onName(event.target.value)} required maxLength={100} placeholder="Team name" aria-label="Team name" className="rounded-lg border border-slate-300 px-3 py-2 text-sm" /><input value={props.description} onChange={(event) => props.onDescription(event.target.value)} maxLength={500} placeholder="What does this team work on?" aria-label="Team description" className="rounded-lg border border-slate-300 px-3 py-2 text-sm" /><button type="submit" disabled={props.saving} className="inline-flex items-center justify-center gap-2 rounded-lg bg-blue-600 px-4 py-2 text-sm font-semibold text-white disabled:opacity-50"><Plus className="w-4 h-4" /> Create</button></form></section>;
}

function TeamsList(props: { teams: Team[]; selectedId: string; includeArchived: boolean; loading: boolean; onIncludeArchived: (value: boolean) => void; onSelect: (id: string) => void }) {
  return <section className="bg-white border border-slate-200 rounded-xl shadow-sm overflow-hidden"><div className="px-4 py-3 border-b border-slate-200 flex items-center justify-between"><h2 className="font-semibold text-slate-900">Teams</h2><label className="text-xs text-slate-500 flex items-center gap-1.5"><input type="checkbox" checked={props.includeArchived} onChange={(event) => props.onIncludeArchived(event.target.checked)} /> Archived</label></div><div className="divide-y divide-slate-100">{props.loading && <p className="p-4 text-sm text-slate-500">Loading teams…</p>}{!props.loading && props.teams.length === 0 && <p className="p-4 text-sm text-slate-500">No teams yet.</p>}{props.teams.map((team) => <TeamListButton key={team.id} team={team} selected={team.id === props.selectedId} onSelect={props.onSelect} />)}</div></section>;
}

function TeamEditorPanel(props: { editor: TeamEditorData | null; members: TeamMember[]; name: string; description: string; saving: boolean; canManage: boolean; canManageOwners: boolean; onName: (value: string) => void; onDescription: (value: string) => void; onUpdate: (event: FormEvent) => void; onArchive: () => void; onWorkspaceRole: (role: TeamWorkspaceRole | null) => void; onResourceAccess: (resourceType: TeamResourceType, resourceId: string, level: TeamAccessLevel | null) => void; onArtifactAccess: (artifactId: string, level: TeamArtifactAccessLevel | null) => void; onAddMember: (email: string, role: TeamMembershipRole) => void; onRemoveMember: (userId: string) => void }) {
  if (!props.editor) return <section className="bg-white border border-slate-200 rounded-xl p-5 shadow-sm"><p className="text-sm text-slate-500">Select a team to manage its access.</p></section>;
  // access.manage covers ordinary teams; account.manage is additionally
  // required for an existing Owner team so Admins never see controls that the
  // transactional Owner-protection fence will reject.
  const canEditTeam = props.canManage && (props.canManageOwners || teamWorkspaceRole(props.editor.team) !== "OWNER");
  const disabled = !canEditTeam || teamEditorDisabled(props.saving, props.editor.team.status);
  return <section className="bg-white border border-slate-200 rounded-xl p-5 shadow-sm"><TeamEditorHeader team={props.editor.team} canManage={canEditTeam} saving={props.saving} onArchive={props.onArchive} /><TeamDetailsForm team={props.editor.team} name={props.name} description={props.description} saving={props.saving} canManage={canEditTeam} onName={props.onName} onDescription={props.onDescription} onUpdate={props.onUpdate} /><TeamAccessControls team={props.editor.team} services={props.editor.services} buckets={props.editor.buckets} disabled={disabled} canManageOwners={props.canManageOwners} onWorkspaceRoleChange={props.onWorkspaceRole} onResourceAccessChange={props.onResourceAccess} onArtifactAccessChange={props.onArtifactAccess} /><TeamMembersControls members={props.members} disabled={disabled} onAdd={props.onAddMember} onRemove={props.onRemoveMember} /></section>;
}

function TeamEditorHeader({ team, canManage, saving, onArchive }: { team: Team; canManage: boolean; saving: boolean; onArchive: () => void }) {
  return <div className="flex items-start justify-between gap-4 mb-5"><div><h2 className="text-lg font-semibold text-slate-900">{team.name}</h2><p className="text-xs text-slate-500">{team.status === "active" ? "Active" : "Archived"} · {team.slug}</p></div>{canManage && team.status === "active" && <button type="button" onClick={onArchive} disabled={saving} className="inline-flex items-center gap-1.5 text-sm text-rose-600 hover:text-rose-700 disabled:opacity-50"><Archive className="w-4 h-4" /> Archive</button>}</div>;
}

function TeamDetailsForm(props: { team: Team; name: string; description: string; saving: boolean; canManage: boolean; onName: (value: string) => void; onDescription: (value: string) => void; onUpdate: (event: FormEvent) => void }) {
  return <form onSubmit={props.onUpdate} className="grid gap-3 md:grid-cols-[1fr_2fr_auto] mb-6"><input value={props.name} disabled={!props.canManage} onChange={(event) => props.onName(event.target.value)} required maxLength={100} aria-label="Edit team name" className="rounded-lg border border-slate-300 px-3 py-2 text-sm disabled:bg-slate-50" /><input value={props.description} disabled={!props.canManage} onChange={(event) => props.onDescription(event.target.value)} maxLength={500} placeholder="Team description" aria-label="Edit team description" className="rounded-lg border border-slate-300 px-3 py-2 text-sm disabled:bg-slate-50" />{props.canManage && <button type="submit" disabled={teamEditorDisabled(props.saving, props.team.status)} className="rounded-lg border border-slate-300 px-4 py-2 text-sm font-semibold text-slate-700 disabled:opacity-50">Save</button>}</form>;
}

function TeamListButton({ team, selected, onSelect }: { team: Team; selected: boolean; onSelect: (id: string) => void }) {
  const selectedClass = selected ? "bg-blue-50 text-blue-800" : "hover:bg-slate-50 text-slate-700";
  return <button type="button" onClick={() => onSelect(team.id)} className={`w-full text-left px-4 py-3 ${selectedClass}`}>
    <span className="block text-sm font-medium truncate">{team.name}</span>
    <span className="block text-xs text-slate-500 mt-0.5">{team.status === "active" ? "Active" : "Archived"}</span>
  </button>;
}

function selectAvailableTeam(teams: Team[], preferredId: string): string {
  if (preferredId && teams.some((team) => team.id === preferredId)) return preferredId;
  return teams[0]?.id ?? "";
}

async function replaceResourceAccess(team: Team, resourceType: TeamResourceType, resourceId: string, desired: TeamAccessLevel | null): Promise<void> {
  const existing = teamResourceLevels(team, resourceType, resourceId);
  // Grant first so changing levels never creates a temporary loss of access if
  // the second mutation is interrupted.
  if (desired && !existing.includes(desired)) await changeTeamResourceAccess("grant", resourceType, team.id, resourceId, desired);
  for (const level of existing) {
    if (level !== desired) await changeTeamResourceAccess("revoke", resourceType, team.id, resourceId, level);
  }
}

async function replaceArtifactAccess(team: Team, artifactId: string, desired: TeamArtifactAccessLevel | null): Promise<void> {
  const existing = team.bindings.flatMap((binding) => {
    if (binding.resource_type !== "artifact" || binding.resource_id !== artifactId) return [];
    if (binding.role_slug === "artifact-reader") return ["READER" as const];
    if (binding.role_slug === "artifact-manager") return ["MANAGER" as const];
    return [];
  });
  // Grant first so moving between Read and Manage cannot briefly remove a
  // second team's runtime access if the follow-up revoke is interrupted.
  if (desired && !existing.includes(desired)) await changeTeamArtifactAccess("grant", team.id, artifactId, desired);
  for (const level of existing) {
    if (level !== desired) await changeTeamArtifactAccess("revoke", team.id, artifactId, level);
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The request could not be completed.";
}

function teamEditorDisabled(saving: boolean, status: Team["status"]): boolean {
  return saving || status !== "active";
}
