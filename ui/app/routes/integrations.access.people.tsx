import { useCallback, useEffect, useState, type FormEvent } from "react";
import type { MetaFunction } from "@remix-run/react";
import { UserPlus, Users, X } from "lucide-react";
import { useToast } from "~/components/Toast";
import { PersonalCredentialPanel } from "~/components/access/PersonalCredentialPanel";
import { WorkspacePermissionGate, useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { SectionTabs } from "~/components/layout/SectionTabs";

const ACCESS_TABS = [
  { label: "People", to: "/integrations/access/people" },
  { label: "Teams", to: "/integrations/access/teams" },
];
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

type PersonEditorProps = { user: User | null; email: string; name: string; saving: boolean; issued: IssuedCredentialPayload | null; canManage: boolean; ownerProtected: boolean; currentSubjectId: string; onEmail: (value: string) => void; onName: (value: string) => void; onUpdate: (event: FormEvent) => void; onStatus: () => void; onIssue: (name: string) => void; onRevoke: (id: string) => void; onClearSecret: () => void };
type PersonDetailsDrawerProps = PersonEditorProps & { onClose: () => void };

export default function PeoplePage() {
	const { access } = useCurrentActorAccess();
	return <>
		<SectionTabs tabs={ACCESS_TABS} />
		<WorkspacePermissionGate permission="access.read" area="people and personal keys">
			<PeopleManager canManage={hasWorkspacePermission(access, "access.manage")} canManageOwners={hasWorkspacePermission(access, "account.manage")} />
		</WorkspacePermissionGate>
	</>;
}

function PeopleManager({ canManage, canManageOwners }: { canManage: boolean; canManageOwners: boolean }) {
  const toast = useToast();
  const { access } = useCurrentActorAccess();
  const currentSubjectId = access?.subject_id ?? "";
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
    // A selection survives refreshes only while its person remains visible;
    // the list otherwise stays full-width until someone explicitly opens it.
    setSelectedId((current) => selectAvailableUser(page.items, preferredId || current));
  }, [appliedSearch, includeSuspended]);

  const refreshSelected = useCallback(async () => {
    setIssued(null);
    if (!selectedId) {
      setSelected(null);
      setSelectedOwnerProtected(false);
      return;
    }
	// Clear the prior record before loading another selection so the drawer
	// never presents one person's controls under another person's row state.
	setSelected(null);
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
      toast.success("Person added. Create a sign-in key when they need access.");
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
    const confirmed = await toast.confirm(
      action === "suspend"
        ? `Suspend ${selected.display_name}? They will immediately lose access.`
        : `Reactivate ${selected.display_name}? They will regain access.`
    );
    if (!confirmed) return;
    await runChange(async () => {
      await changeUserStatus(selected.id, action);
      await Promise.all([refreshUsers(selected.id), refreshSelected()]);
      toast.success(action === "suspend" ? "Person suspended." : "Person reactivated.");
    });
  }

  async function handleIssue(name: string) {
    if (!selected) return;
    const confirmed = await toast.confirm(`Create a new personal key for ${selected.display_name}? The secret will be shown once.`);
    if (!confirmed) return;
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
    const credential = selected.credentials.find((c) => c.id === credentialId);
    const confirmed = await toast.confirm(`Revoke key "${credential?.name ?? ""}"? This person will no longer be able to sign in with it.`);
    if (!confirmed) return;
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
    <header><p className="text-sm font-medium text-blue-600">Access</p><h1 className="text-2xl font-bold text-slate-900 flex items-center gap-2"><Users className="w-6 h-6" /> People</h1><p className="text-slate-500 mt-1">Add people, manage their status, and create personal sign-in keys.</p></header>
    {canManage && <AddPersonForm email={newEmail} name={newName} saving={saving} onEmail={setNewEmail} onName={setNewName} onSubmit={handleCreate} />}
    <PeopleList users={users} total={userTotal} search={search} selectedId={selectedId} loading={loading} includeSuspended={includeSuspended} onSearch={setSearch} onApplySearch={() => setAppliedSearch(search.trim())} onIncludeSuspended={setIncludeSuspended} onSelect={setSelectedId} />
    {selectedId && <PersonDetailsDrawer user={selected} email={editEmail} name={editName} saving={saving} issued={issued} canManage={canManage && (canManageOwners || !selectedOwnerProtected)} ownerProtected={selectedOwnerProtected && !canManageOwners} currentSubjectId={currentSubjectId} onClose={() => setSelectedId("")} onEmail={setEditEmail} onName={setEditName} onUpdate={handleUpdate} onStatus={handleStatus} onIssue={handleIssue} onRevoke={handleRevoke} onClearSecret={() => setIssued(null)} />}
  </div>;
}

function AddPersonForm(props: { email: string; name: string; saving: boolean; onEmail: (value: string) => void; onName: (value: string) => void; onSubmit: (event: FormEvent) => void }) {
  return <section className="bg-white border border-slate-200 rounded-xl p-5 shadow-sm">
    <h2 className="text-base font-semibold text-slate-900 mb-1">Add a person</h2>
    <p className="text-xs text-slate-500 mb-3">This does not send an email. Create a personal key separately when they need to sign in.</p>
    <form onSubmit={props.onSubmit} className="grid gap-3 md:grid-cols-[1fr_1fr_auto] items-end" toolname="add_person" tooldescription="Add a person to the workspace without sending an invitation email.">
      <label className="flex flex-col gap-1">
        <span className="text-xs font-medium text-slate-700">Email</span>
        <input type="email" required value={props.email} onChange={(event) => props.onEmail(event.target.value)} placeholder="person@example.com" className="rounded-lg border border-slate-300 px-3 py-2 text-sm" />
      </label>
      <label className="flex flex-col gap-1">
        <span className="text-xs font-medium text-slate-700">Display name</span>
        <input required value={props.name} onChange={(event) => props.onName(event.target.value)} placeholder="Display name" className="rounded-lg border border-slate-300 px-3 py-2 text-sm" />
      </label>
      <button type="submit" disabled={props.saving} className="inline-flex items-center justify-center gap-2 rounded-lg bg-slate-950 px-4 py-2 text-sm font-semibold text-white hover:bg-slate-800 disabled:opacity-50"><UserPlus className="w-4 h-4" /> Add person</button>
    </form>
  </section>;
}

// PeopleList uses the available content width so identity fields remain easy
// to scan before a row opens its independent detail surface.
function PeopleList(props: { users: UserSummary[]; total: number; search: string; selectedId: string; loading: boolean; includeSuspended: boolean; onSearch: (value: string) => void; onApplySearch: () => void; onIncludeSuspended: (value: boolean) => void; onSelect: (id: string) => void }) {
  return <section className="overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm">
    <div className="flex flex-col gap-3 border-b border-slate-200 px-4 py-4 sm:flex-row sm:items-center sm:justify-between">
      <div><h2 className="font-semibold text-slate-900">People</h2><p className="mt-0.5 text-xs text-slate-500">{props.total} {props.total === 1 ? "person" : "people"}</p></div>
      <label className="flex items-center gap-1.5 text-xs text-slate-500"><input type="checkbox" checked={props.includeSuspended} onChange={(event) => props.onIncludeSuspended(event.target.checked)} /> Include suspended</label>
    </div>
    <form onSubmit={(event) => { event.preventDefault(); props.onApplySearch(); }} className="flex gap-2 border-b border-slate-100 p-4"><input type="search" value={props.search} onChange={(event) => props.onSearch(event.target.value)} placeholder="Search people" aria-label="Search people" className="min-w-0 flex-1 rounded-lg border border-slate-300 px-3 py-2 text-sm" /><button type="submit" className="rounded-lg border border-slate-300 px-4 py-2 text-xs font-semibold text-slate-700">Search</button></form>
    {!props.loading && props.total > props.users.length && <p className="border-b border-amber-200 bg-amber-50 px-4 py-2 text-xs text-amber-900" role="status">Showing {props.users.length} of {props.total} people. Search by name or email to find someone outside this list.</p>}
    <div className="hidden grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)_auto] gap-4 border-b border-slate-100 bg-slate-50 px-4 py-2 text-xs font-semibold uppercase tracking-wide text-slate-500 sm:grid"><span>Name</span><span>Email</span><span>Status</span></div>
    <div className="divide-y divide-slate-100">
      {props.loading && <p className="p-4 text-sm text-slate-500">Loading people…</p>}
      {!props.loading && props.users.length === 0 && <p className="p-4 text-sm text-slate-500">No people found.</p>}
      {props.users.map((user) => <button key={user.id} type="button" onClick={() => props.onSelect(user.id)} aria-label={`Open details for ${user.display_name}`} className={`grid w-full gap-1 px-4 py-3 text-left transition-colors sm:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)_auto] sm:items-center sm:gap-4 ${user.id === props.selectedId ? "bg-blue-50 text-blue-800" : "text-slate-700 hover:bg-slate-50"}`}><span className="truncate text-sm font-medium">{user.display_name}</span><span className="truncate text-xs text-slate-500 sm:text-sm">{user.email}</span><span className="text-xs font-medium text-slate-500">{statusLabel(user.status)}</span></button>)}
    </div>
  </section>;
}

// PersonDetailsDrawer keeps the list available at full width and reveals one
// person's controls only after an explicit row selection.
function PersonDetailsDrawer(props: PersonDetailsDrawerProps) {
  return <>
    <div className="fixed inset-0 z-40 bg-slate-900/20 transition-opacity" onClick={props.onClose} />
    <aside role="dialog" aria-modal="true" aria-labelledby="person-details-title" className="fixed inset-y-0 right-0 z-50 flex w-full flex-col overflow-y-auto overflow-x-hidden border-l border-slate-200 bg-white shadow-2xl md:w-[calc(100vw-4rem)] md:max-w-[940px] xl:max-w-[1080px]">
      <div className="sticky top-0 z-10 flex items-center justify-between gap-4 border-b border-slate-100 bg-white/90 px-5 py-4 backdrop-blur sm:px-6">
        <div><h2 id="person-details-title" className="text-lg font-semibold text-slate-900">Person details</h2><p className="mt-0.5 text-xs text-slate-500">Manage profile, teams, and personal keys.</p></div>
        <button type="button" onClick={props.onClose} aria-label="Close person details" className="rounded-full p-2 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"><X className="h-5 w-5" /></button>
      </div>
      <div className="p-5 sm:p-6">
        {props.user ? <PersonEditor {...props} /> : <p className="text-sm text-slate-500" role="status">Loading person details…</p>}
      </div>
    </aside>
  </>;
}

// PersonEditor keeps the sections in one vertical flow so the wide drawer
// never requires sideways scrolling or hides controls behind navigation.
function PersonEditor(props: PersonEditorProps) {
  if (!props.user) return null;
  const archived = props.user.status === "ARCHIVED";
  const isSelf = props.user.id === props.currentSubjectId;
  return <div className="space-y-6">
    <PersonEditorHeader user={props.user} canManage={props.canManage} saving={props.saving} archived={archived} isSelf={isSelf} onStatus={props.onStatus} />
    {!props.canManage && <p className="text-xs text-slate-500" role="status">{props.ownerProtected ? "Only a workspace Owner can change this person." : "You have read-only access to people."}</p>}
    <PersonDetailsForm email={props.email} name={props.name} canManage={props.canManage} saving={props.saving} archived={archived} onEmail={props.onEmail} onName={props.onName} onUpdate={props.onUpdate} />
    <MembershipSummary memberships={props.user.memberships} truncated={props.user.memberships_truncated} />
    <PersonalCredentialPanel credentials={props.user.credentials} truncated={props.user.credentials_truncated} issuedSecret={props.issued?.secret ?? null} disabled={!props.canManage || props.saving || archived} onIssue={props.onIssue} onRevoke={props.onRevoke} onClearSecret={props.onClearSecret} />
  </div>;
}

function PersonEditorHeader({ user, canManage, saving, archived, isSelf, onStatus }: { user: User; canManage: boolean; saving: boolean; archived: boolean; isSelf: boolean; onStatus: () => void }) {
  return <div className="flex items-start justify-between gap-4"><div><h2 className="text-lg font-semibold text-slate-900">{user.display_name}</h2><p className="text-xs text-slate-500">{statusLabel(user.status)} · {user.id}</p></div>{canManage && !archived && !isSelf && <button type="button" disabled={saving} onClick={onStatus} className="text-sm font-semibold text-rose-600 disabled:opacity-50">{user.status === "SUSPENDED" ? "Reactivate" : "Suspend"}</button>}</div>;
}

function PersonDetailsForm(props: { email: string; name: string; canManage: boolean; saving: boolean; archived: boolean; onEmail: (value: string) => void; onName: (value: string) => void; onUpdate: (event: FormEvent) => void }) {
  return <form onSubmit={props.onUpdate} className="grid gap-3 md:grid-cols-[1fr_1fr_auto] items-end">
    <label className="flex flex-col gap-1">
      <span className="text-xs font-medium text-slate-700">Email</span>
      <input type="email" required value={props.email} disabled={!props.canManage} onChange={(event) => props.onEmail(event.target.value)} className="rounded-lg border border-slate-300 px-3 py-2 text-sm disabled:bg-slate-50" />
    </label>
    <label className="flex flex-col gap-1">
      <span className="text-xs font-medium text-slate-700">Display name</span>
      <input required value={props.name} disabled={!props.canManage} onChange={(event) => props.onName(event.target.value)} className="rounded-lg border border-slate-300 px-3 py-2 text-sm disabled:bg-slate-50" />
    </label>
    {props.canManage && <button type="submit" disabled={props.saving || props.archived} className="rounded-lg border border-slate-300 px-4 py-2 text-sm font-semibold text-slate-700 disabled:opacity-50">Save</button>}
  </form>;
}

function MembershipSummary({ memberships, truncated }: { memberships: User["memberships"]; truncated: boolean }) {
  return <section><h3 className="text-sm font-semibold text-slate-900 mb-2">Teams</h3>{truncated && <p className="mb-2 text-xs text-amber-800" role="status">Showing the first 100 team memberships.</p>}{memberships.length === 0 ? <p className="text-sm text-slate-500">Not in a team yet.</p> : <div className="flex flex-wrap gap-2">{memberships.map((membership) => <span key={membership.team_id} className="rounded-full bg-slate-100 px-3 py-1 text-xs text-slate-700">{membership.team_name} · {membership.membership_role === "MANAGER" ? "Manager" : "Member"}</span>)}</div>}</section>;
}

function selectAvailableUser(users: UserSummary[], preferredId: string): string {
  // Missing results close the drawer instead of silently opening the first
  // person, preserving explicit user selection as the only open action.
  if (preferredId && users.some((user) => user.id === preferredId)) return preferredId;
  return "";
}

function statusLabel(status: User["status"]): string {
  // INVITED is an internal lifecycle state, not evidence that an email was
  // sent. Describe the action still required so operators are not misled.
  if (status === "INVITED") return "Sign-in key required";
  return status.charAt(0) + status.slice(1).toLowerCase();
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "The request could not be completed.";
}
