import { FieldLabel } from "../forms/FieldLabel.ts";
import { Select } from "../forms/Select.ts";
import { createElement, useState, type ChangeEvent, type FormEvent, type ReactElement } from "react";
import type { TeamMember, TeamMembershipRole } from "../../lib/people";

interface TeamMembersControlsProps {
  members: TeamMember[];
  disabled?: boolean;
  onAdd: (email: string, role: TeamMembershipRole) => void;
  onRemove: (userId: string) => void;
}

/** Uses the shared select while keeping selection state and actions owned by this page. */
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
      createElement("p", { className: "text-xs text-slate-500 mt-0.5" }, "Add by email. If they are new, a person record is created without sending an email. They need a personal key to sign in.")),
    createElement("form", { onSubmit: submit, className: "grid grid-cols-[repeat(auto-fit,minmax(min(100%,12rem),1fr))] items-end gap-2 p-4 border-b border-slate-100" },
      createElement("label", { className: "flex min-w-0 flex-col gap-1 text-xs font-medium text-slate-700" },
        createElement(FieldLabel, { required: true }, "Member email"),
        createElement("input", { type: "email", value: email, onChange: (event) => setEmail(event.target.value), disabled: props.disabled, required: true, placeholder: "person@example.com", "aria-label": "Member email", className: "rounded-lg border border-slate-300 px-3 py-2 text-sm" })),
      createElement(Select, { value: role, onChange: (event: ChangeEvent<HTMLSelectElement>) => setRole(event.target.value as TeamMembershipRole), disabled: props.disabled, "aria-label": "Membership role", className: "rounded-lg border border-slate-300 px-2 py-2 text-sm" },
        createElement("option", { value: "MEMBER" }, "Member"), createElement("option", { value: "MANAGER" }, "Manager")),
      createElement("button", { type: "submit", disabled: props.disabled, className: "rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm font-semibold text-slate-700 disabled:opacity-50" }, "Add person")),
    createElement("div", { className: "divide-y divide-slate-100 px-4" }, ...memberRows(props)));
}

/** Lists team membership with neutral removal actions while preserving disabled access controls. */
function memberRows(props: TeamMembersControlsProps): ReactElement[] {
  if (props.members.length === 0) return [createElement("p", { key: "empty", className: "py-4 text-sm text-slate-500" }, "No people in this team yet.")];
  return props.members.map((member) => createElement("div", { key: member.user_id, className: "flex items-center justify-between gap-3 py-3" },
    createElement("div", { className: "min-w-0" },
      createElement("p", { className: "text-sm font-medium text-slate-800 truncate" }, member.display_name),
      createElement("p", { className: "text-xs text-slate-500 truncate" }, `${member.email} · ${member.membership_role === "MANAGER" ? "Manager" : "Member"}`)),
    createElement("button", { type: "button", disabled: props.disabled, onClick: () => props.onRemove(member.user_id), className: "text-xs font-semibold text-slate-950 disabled:opacity-50" }, "Remove")));
}
