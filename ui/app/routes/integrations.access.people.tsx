import { useCallback, useEffect, useState, type FormEvent } from "react";
import type { MetaFunction } from "@remix-run/react";
import { UserPlus, Users } from "lucide-react";
import { useToast } from "~/components/Toast";
import { PersonalCredentialPanel } from "~/components/access/PersonalCredentialPanel";
import { WorkspacePermissionGate, useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import {
  changeUserStatus,
  createUser,
  getUser,
  issueUserCredential,
  listUsers,
  revokeUserCredential,
  updateUser,
  type IssuedCredentialPayload,
  type User,
	type UserSummary,
} from "~/lib/people";

export const meta: MetaFunction = () => [{ title: "People - Fused" }];

export default function PeoplePage() {
	const { access } = useCurrentActorAccess();
	return <WorkspacePermissionGate permission="access.read" area="people and personal keys">
		<PeopleManager canManage={hasWorkspacePermission(access, "access.manage")} canManageOwners={hasWorkspacePermission(access, "account.manage")} />
	</WorkspacePermissionGate>;
}

function PeopleManager({ canManage, canManageOwners }: { canManage: boolean; canManageOwners: boolean }) {
  const toast = useToast();
  const [users, setUsers] = useState<UserSummary[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const [selected, setSelected] = useState<User | null>(null);
  const [search, setSearch] = useState("");
  const [appliedSearch, setAppliedSearch] = useState("");
  const [userTotal, setUserTotal] = useState(0);
  const [includeSuspended, setIncludeSuspended] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [newEmail, setNewEmail] = useState("");
  const [newName, setNewName] = useState("");
  const [editEmail, setEditEmail] = useState("");
  const [editName, setEditName] = useState("");
  const [issued, setIssued] = useState<IssuedCredentialPayload | null>(null);
  const [selectedOwnerProtected, setSelectedOwnerProtected] = useState(true);

  const refreshUsers = useCallback(async (preferredId = "") => {
    const page = await listUsers(appliedSearch, includeSuspended);
    setUsers(page.items);
    setUserTotal(page.total);
    setSelectedId((current) => selectAvailableUser(page.items, preferredId || current));
  }, [appliedSearch, includeSuspended]);

  const refreshSelected = useCallback(async () => {
    setIssued(null);
    if (!selectedId) {
      setSelected(null);
      setSelectedOwnerProtected(false);
      return;
    }
	// Fail closed while the target's protected status is being resolved. The
	// server enforces this too; the UI avoids presenting controls that will fail.
	setSelectedOwnerProtected(true);
    const user = await getUser(selectedId);
    setSelected(user);
	setSelectedOwnerProtected(user.owner_protected);
    setEditEmail(user.email);
    setEditName(user.display_name);
  }, [selectedId]);

  useEffect(() => {
    setLoading(true);
    refreshUsers().catch((error: unknown) => toast.error(errorMessage(error))).finally(() => setLoading(false));
  }, [refreshUsers]);

  useEffect(() => {
    refreshSelected().catch((error: unknown) => toast.error(errorMessage(error)));
  }, [refreshSelected]);

  async function handleCreate(event: FormEvent) {
    event.preventDefault();
    if (!newEmail.trim() || !newName.trim()) return;
    await runChange(async () => {
      const payload = await createUser({ email: newEmail.trim(), display_name: newName.trim() });
      setNewEmail("");
      setNewName("");
      await refreshUsers(payload.user.id);
      toast.success("Person invited.");
    });
  }

  async function handleUpdate(event: FormEvent) {
    event.preventDefault();
    if (!selected || !editEmail.trim() || !editName.trim()) return;
    await runChange(async () => {
      await updateUser(selected.id, { email: editEmail.trim(), display_name: editName.trim() });
      await Promise.all([refreshUsers(selected.id), refreshSelected()]);
      toast.success("Person updated.");
    });
  }

  async function handleStatus() {
    if (!selected) return;
    const action = selected.status === "SUSPENDED" ? "reactivate" : "suspend";
    await runChange(async () => {
      await changeUserStatus(selected.id, action);
      await Promise.all([refreshUsers(selected.id), refreshSelected()]);
      toast.success(action === "suspend" ? "Person suspended." : "Person reactivated.");
    });
  }

  async function handleIssue(name: string) {
    if (!selected) return;
    await runChange(async () => {
      const payload = await issueUserCredential(selected.id, name);
      setIssued(payload);
      const [, user] = await Promise.all([refreshUsers(selected.id), getUser(selected.id)]);
      setSelected(user);
      toast.success("Personal key created.");
    });
  }

  async function handleRevoke(credentialId: string) {
    if (!selected) return;
    await runChange(async () => {
      await revokeUserCredential(selected.id, credentialId);
      if (issued?.credential.id === credentialId) setIssued(null);
      await refreshSelected();
      toast.success("Personal key revoked.");
    });
  }

  async function runChange(change: () => Promise<void>) {
    setSaving(true);
    try {
      await change();
    } catch (error: unknown) {
      toast.error(errorMessage(error));
    } finally {
      setSaving(false);
    }
  }

  return <div className="space-y-6">
    <header><p className="text-sm font-medium text-blue-600">Access</p><h1 className="text-2xl font-bold text-slate-900 flex items-center gap-2"><Users className="w-6 h-6" /> People</h1><p className="text-slate-500 mt-1">Invite people, manage their status, and create personal sign-in keys.</p></header>
    {canManage && <InvitePersonForm email={newEmail} name={newName} saving={saving} onEmail={setNewEmail} onName={setNewName} onSubmit={handleCreate} />}
    <div className="grid gap-6 lg:grid-cols-[300px_1fr]">
      <PeopleList users={users} total={userTotal} search={search} selectedId={selectedId} loading={loading} includeSuspended={includeSuspended} onSearch={setSearch} onApplySearch={() => setAppliedSearch(search.trim())} onIncludeSuspended={setIncludeSuspended} onSelect={setSelectedId} />
      <PersonEditor user={selected} email={editEmail} name={editName} saving={saving} issued={issued} canManage={canManage && (canManageOwners || !selectedOwnerProtected)} ownerProtected={selectedOwnerProtected && !canManageOwners} onEmail={setEditEmail} onName={setEditName} onUpdate={handleUpdate} onStatus={handleStatus} onIssue={handleIssue} onRevoke={handleRevoke} onClearSecret={() => setIssued(null)} />
    </div>
  </div>;
}

function InvitePersonForm(props: { email: string; name: string; saving: boolean; onEmail: (value: string) => void; onName: (value: string) => void; onSubmit: (event: FormEvent) => void }) {
  return <section className="bg-white border border-slate-200 rounded-xl p-5 shadow-sm"><h2 className="text-base font-semibold text-slate-900 mb-1">Invite a person</h2><p className="text-xs text-slate-500 mb-3">They become active when you create their first personal key.</p><form onSubmit={props.onSubmit} className="grid gap-3 md:grid-cols-[1fr_1fr_auto]" toolname="invite_person" tooldescription="Invite a person to the workspace."><input type="email" required value={props.email} onChange={(event) => props.onEmail(event.target.value)} placeholder="person@example.com" aria-label="Email address" className="rounded-lg border border-slate-300 px-3 py-2 text-sm" /><input required value={props.name} onChange={(event) => props.onName(event.target.value)} placeholder="Display name" aria-label="Display name" className="rounded-lg border border-slate-300 px-3 py-2 text-sm" /><button type="submit" disabled={props.saving} className="inline-flex items-center justify-center gap-2 rounded-lg bg-blue-600 px-4 py-2 text-sm font-semibold text-white disabled:opacity-50"><UserPlus className="w-4 h-4" /> Invite</button></form></section>;
}

function PeopleList(props: { users: UserSummary[]; total: number; search: string; selectedId: string; loading: boolean; includeSuspended: boolean; onSearch: (value: string) => void; onApplySearch: () => void; onIncludeSuspended: (value: boolean) => void; onSelect: (id: string) => void }) {
  return <section className="bg-white border border-slate-200 rounded-xl shadow-sm overflow-hidden"><div className="px-4 py-3 border-b border-slate-200 flex items-center justify-between"><h2 className="font-semibold text-slate-900">People</h2><label className="text-xs text-slate-500 flex items-center gap-1.5"><input type="checkbox" checked={props.includeSuspended} onChange={(event) => props.onIncludeSuspended(event.target.checked)} /> Suspended</label></div><form onSubmit={(event) => { event.preventDefault(); props.onApplySearch(); }} className="flex gap-2 border-b border-slate-100 p-3"><input type="search" value={props.search} onChange={(event) => props.onSearch(event.target.value)} placeholder="Search people" aria-label="Search people" className="min-w-0 flex-1 rounded-lg border border-slate-300 px-3 py-2 text-sm" /><button type="submit" className="rounded-lg border border-slate-300 px-3 py-2 text-xs font-semibold text-slate-700">Search</button></form>{!props.loading && props.total > props.users.length && <p className="border-b border-amber-200 bg-amber-50 px-4 py-2 text-xs text-amber-900" role="status">Showing {props.users.length} of {props.total} people. Search by name or email to find someone outside this list.</p>}<div className="divide-y divide-slate-100">{props.loading && <p className="p-4 text-sm text-slate-500">Loading people…</p>}{!props.loading && props.users.length === 0 && <p className="p-4 text-sm text-slate-500">No people found.</p>}{props.users.map((user) => <button key={user.id} type="button" onClick={() => props.onSelect(user.id)} className={`w-full text-left px-4 py-3 ${user.id === props.selectedId ? "bg-blue-50 text-blue-800" : "hover:bg-slate-50 text-slate-700"}`}><span className="block text-sm font-medium truncate">{user.display_name}</span><span className="block text-xs text-slate-500 truncate mt-0.5">{user.email} · {statusLabel(user.status)}</span></button>)}</div></section>;
}

function PersonEditor(props: { user: User | null; email: string; name: string; saving: boolean; issued: IssuedCredentialPayload | null; canManage: boolean; ownerProtected: boolean; onEmail: (value: string) => void; onName: (value: string) => void; onUpdate: (event: FormEvent) => void; onStatus: () => void; onIssue: (name: string) => void; onRevoke: (id: string) => void; onClearSecret: () => void }) {
  if (!props.user) return <section className="bg-white border border-slate-200 rounded-xl p-5 shadow-sm"><p className="text-sm text-slate-500">Select a person to manage them.</p></section>;
  const archived = props.user.status === "ARCHIVED";
  return <section className="bg-white border border-slate-200 rounded-xl p-5 shadow-sm space-y-6"><PersonEditorHeader user={props.user} canManage={props.canManage} saving={props.saving} archived={archived} onStatus={props.onStatus} />{!props.canManage && <p className="text-xs text-slate-500" role="status">{props.ownerProtected ? "Only a workspace Owner can change this person." : "You have read-only access to people."}</p>}<PersonDetailsForm email={props.email} name={props.name} canManage={props.canManage} saving={props.saving} archived={archived} onEmail={props.onEmail} onName={props.onName} onUpdate={props.onUpdate} /><MembershipSummary memberships={props.user.memberships} truncated={props.user.memberships_truncated} /><PersonalCredentialPanel credentials={props.user.credentials} truncated={props.user.credentials_truncated} issuedSecret={props.issued?.secret ?? null} disabled={!props.canManage || props.saving || archived} onIssue={props.onIssue} onRevoke={props.onRevoke} onClearSecret={props.onClearSecret} /></section>;
}

function PersonEditorHeader({ user, canManage, saving, archived, onStatus }: { user: User; canManage: boolean; saving: boolean; archived: boolean; onStatus: () => void }) {
  return <div className="flex items-start justify-between gap-4"><div><h2 className="text-lg font-semibold text-slate-900">{user.display_name}</h2><p className="text-xs text-slate-500">{statusLabel(user.status)} · {user.id}</p></div>{canManage && !archived && <button type="button" disabled={saving} onClick={onStatus} className="text-sm font-semibold text-rose-600 disabled:opacity-50">{user.status === "SUSPENDED" ? "Reactivate" : "Suspend"}</button>}</div>;
}

function PersonDetailsForm(props: { email: string; name: string; canManage: boolean; saving: boolean; archived: boolean; onEmail: (value: string) => void; onName: (value: string) => void; onUpdate: (event: FormEvent) => void }) {
  return <form onSubmit={props.onUpdate} className="grid gap-3 md:grid-cols-[1fr_1fr_auto]"><input type="email" required value={props.email} disabled={!props.canManage} onChange={(event) => props.onEmail(event.target.value)} aria-label="Edit email address" className="rounded-lg border border-slate-300 px-3 py-2 text-sm disabled:bg-slate-50" /><input required value={props.name} disabled={!props.canManage} onChange={(event) => props.onName(event.target.value)} aria-label="Edit display name" className="rounded-lg border border-slate-300 px-3 py-2 text-sm disabled:bg-slate-50" />{props.canManage && <button type="submit" disabled={props.saving || props.archived} className="rounded-lg border border-slate-300 px-4 py-2 text-sm font-semibold text-slate-700 disabled:opacity-50">Save</button>}</form>;
}

function MembershipSummary({ memberships, truncated }: { memberships: User["memberships"]; truncated: boolean }) {
  return <section><h3 className="text-sm font-semibold text-slate-900 mb-2">Teams</h3>{truncated && <p className="mb-2 text-xs text-amber-800" role="status">Showing the first 100 team memberships.</p>}{memberships.length === 0 ? <p className="text-sm text-slate-500">Not in a team yet.</p> : <div className="flex flex-wrap gap-2">{memberships.map((membership) => <span key={membership.team_id} className="rounded-full bg-slate-100 px-3 py-1 text-xs text-slate-700">{membership.team_name} · {membership.membership_role === "MANAGER" ? "Manager" : "Member"}</span>)}</div>}</section>;
}

function selectAvailableUser(users: UserSummary[], preferredId: string): string {
  if (preferredId && users.some((user) => user.id === preferredId)) return preferredId;
  return users[0]?.id ?? "";
}

function statusLabel(status: User["status"]): string {
  return status.charAt(0) + status.slice(1).toLowerCase();
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The request could not be completed.";
}
