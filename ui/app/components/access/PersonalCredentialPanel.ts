import { createElement, useState, type FormEvent, type ReactElement } from "react";
import type { ControlCredential } from "../../lib/people";

interface PersonalCredentialPanelProps {
  credentials: ControlCredential[];
  truncated?: boolean;
  issuedSecret: string | null;
  disabled?: boolean;
  onIssue: (name: string) => void;
  onRevoke: (credentialId: string) => void;
  onClearSecret: () => void;
}

export function PersonalCredentialPanel(props: PersonalCredentialPanelProps): ReactElement {
  const [name, setName] = useState("personal");
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (name.trim()) props.onIssue(name.trim());
  };
  return createElement(
    "section",
    { className: "rounded-lg border border-slate-200 overflow-hidden" },
    createElement("div", { className: "bg-slate-50 px-4 py-3 border-b border-slate-200" },
      createElement("h3", { className: "text-sm font-semibold text-slate-900" }, "Personal keys"),
      createElement("p", { className: "text-xs text-slate-500 mt-0.5" }, "Keys let this person sign in to this Engine.")),
    props.issuedSecret ? issuedSecretNotice(props.issuedSecret, props.onClearSecret) : null,
    createElement("form", { onSubmit: submit, className: "flex items-end gap-2 p-4 border-b border-slate-100" },
      createElement("label", { className: "flex-1 flex flex-col gap-1" },
        createElement("span", { className: "text-xs font-medium text-slate-700" }, "Key name"),
        createElement("input", { value: name, onChange: (event) => setName(event.target.value), disabled: props.disabled, className: "w-full rounded-lg border border-slate-300 px-3 py-2 text-sm" })),
      createElement("button", { type: "submit", disabled: props.disabled, className: "rounded-lg bg-blue-600 px-3 py-2 text-sm font-semibold text-white disabled:opacity-50" }, "Create key")),
    props.truncated ? createElement("p", { className: "border-b border-amber-200 bg-amber-50 px-4 py-2 text-xs text-amber-900", role: "status" }, "Showing the 100 most recent personal keys.") : null,
    createElement("div", { className: "divide-y divide-slate-100 px-4" }, ...credentialRows(props))
  );
}

function issuedSecretNotice(secret: string, onClear: () => void): ReactElement {
  return createElement("div", { className: "m-4 rounded-lg border border-amber-300 bg-amber-50 p-4", role: "alert" },
    createElement("p", { className: "text-sm font-semibold text-amber-900" }, "Copy this key now. It will not be shown again."),
    createElement("p", { className: "text-xs text-amber-800 mt-1" }, "Keep it secret. Anyone with this key can act as this person."),
    createElement("code", { className: "block mt-3 break-all rounded bg-white p-2 text-xs text-slate-900" }, secret),
    createElement("div", { className: "mt-3 flex gap-2" },
      createElement("button", { type: "button", onClick: () => navigator.clipboard.writeText(secret), className: "rounded bg-amber-700 px-3 py-1.5 text-xs font-semibold text-white" }, "Copy key"),
      createElement("button", { type: "button", onClick: onClear, className: "rounded border border-amber-400 px-3 py-1.5 text-xs font-semibold text-amber-900" }, "I've saved it")));
}

function credentialRows(props: PersonalCredentialPanelProps): ReactElement[] {
  if (props.credentials.length === 0) return [createElement("p", { key: "empty", className: "py-4 text-sm text-slate-500" }, "No personal keys yet.")];
  return props.credentials.map((credential) => createElement("div", { key: credential.id, className: "flex items-center justify-between gap-3 py-3" },
    createElement("div", { className: "min-w-0" },
      createElement("p", { className: "text-sm font-medium text-slate-800 truncate" }, credential.name),
      createElement("p", { className: "text-xs text-slate-500" }, `${credential.key_prefix}… · ${credentialStatus(credential)}`),
      createElement("p", { className: "text-xs text-slate-500" }, credential.last_used_at ? `Last used ${new Date(credential.last_used_at).toLocaleString()}` : "Never used")),
    !credential.revoked_at ? createElement("button", { type: "button", disabled: props.disabled, onClick: () => props.onRevoke(credential.id), className: "text-xs font-semibold text-rose-600 disabled:opacity-50" }, "Revoke") : null));
}

function credentialStatus(credential: ControlCredential): "Revoked" | "Expired" | "Active" {
  if (credential.revoked_at) return "Revoked";
  if (credential.expires_at && new Date(credential.expires_at).getTime() <= Date.now()) return "Expired";
  return "Active";
}
