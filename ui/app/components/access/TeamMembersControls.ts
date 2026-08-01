import { createElement, useState, type ChangeEvent, type FormEvent, type ReactElement } from "react";
import type { TeamMember, TeamMembershipRole } from "../../lib/people";

interface TeamMembersControlsProps {
  members: TeamMember[];
  disabled?: boolean;
  onAdd: (email: string, role: TeamMembershipRole) => void;
  onRemove: (userId: string) => void;
}

export function TeamMembersControls(props: TeamMembersControlsProps): ReactElement {
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<TeamMembershipRole>("MEMBER");
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!email.trim()) return;
    props.onAdd(email.trim(), role);
    setEmail("");
  };
  return createElement("section", { className: "mt-6 rounded-lg border border-slate-200 overflow-hidden", "data-component": "team-members-controls" },
    createElement("div", { className: "bg-slate-50 px-4 py-3 border-b border-slate-200" },
      createElement("h3", { className: "text-sm font-semibold text-slate-900" }, "People"),
      createElement("p", { className: "text-xs text-slate-500 mt-0.5" }, "Add by email. A new person is invited automatically.")),
    createElement("form", { onSubmit: submit, className: "grid gap-2 p-4 sm:grid-cols-[1fr_130px_auto] border-b border-slate-100" },
      createElement("input", { type: "email", value: email, onChange: (event) => setEmail(event.target.value), disabled: props.disabled, required: true, placeholder: "person@example.com", "aria-label": "Member email", className: "rounded-lg border border-slate-300 px-3 py-2 text-sm" }),
      createElement("select", { value: role, onChange: (event: ChangeEvent<HTMLSelectElement>) => setRole(event.target.value as TeamMembershipRole), disabled: props.disabled, "aria-label": "Membership role", className: "rounded-lg border border-slate-300 px-2 py-2 text-sm" },
        createElement("option", { value: "MEMBER" }, "Member"), createElement("option", { value: "MANAGER" }, "Manager")),
      createElement("button", { type: "submit", disabled: props.disabled, className: "rounded-lg bg-blue-600 px-3 py-2 text-sm font-semibold text-white disabled:opacity-50" }, "Add person")),
    createElement("div", { className: "divide-y divide-slate-100 px-4" }, ...memberRows(props)));
}

function memberRows(props: TeamMembersControlsProps): ReactElement[] {
  if (props.members.length === 0) return [createElement("p", { key: "empty", className: "py-4 text-sm text-slate-500" }, "No people in this team yet.")];
  return props.members.map((member) => createElement("div", { key: member.user_id, className: "flex items-center justify-between gap-3 py-3" },
    createElement("div", { className: "min-w-0" },
      createElement("p", { className: "text-sm font-medium text-slate-800 truncate" }, member.display_name),
      createElement("p", { className: "text-xs text-slate-500 truncate" }, `${member.email} · ${member.membership_role === "MANAGER" ? "Manager" : "Member"}`)),
    createElement("button", { type: "button", disabled: props.disabled, onClick: () => props.onRemove(member.user_id), className: "text-xs font-semibold text-rose-600 disabled:opacity-50" }, "Remove")));
}
