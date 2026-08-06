import { createElement, type ChangeEvent, type FormEvent, type ReactElement } from "react";
import type {
  Team,
  TeamAccessLevel,
  TeamAppAccessLevel,
  TeamResourceType,
  TeamWorkspaceRole,
} from "../../lib/teams";

export interface TeamAccessResource {
  id: string;
  name: string;
}

interface TeamAccessControlsProps {
  team: Team;
  services: TeamAccessResource[];
  buckets: TeamAccessResource[];
  disabled?: boolean;
  canManageOwners?: boolean;
  onWorkspaceRoleChange: (role: TeamWorkspaceRole | null) => void;
  onResourceAccessChange: (resourceType: TeamResourceType, resourceId: string, level: TeamAccessLevel | null) => void;
  onAppAccessChange: (appFamilyId: string, level: TeamAppAccessLevel | null) => void;
}

const workspaceRoles: Array<[TeamWorkspaceRole | "NONE", string]> = [
  ["NONE", "No workspace role"],
  ["OWNER", "Owner"],
  ["ADMIN", "Admin"],
  ["BUILDER", "Builder"],
  ["VIEWER", "Viewer"],
];

const resourceLevels: Array<[TeamAccessLevel | "NONE", string]> = [
  ["NONE", "No access"],
  ["USER", "Use"],
  ["MANAGER", "Manage"],
];

const appLevels: Array<[TeamAppAccessLevel | "NONE", string]> = [
  ["NONE", "No access"],
  ["READER", "Read"],
  ["MANAGER", "Manage"],
];

export function TeamAccessControls(props: TeamAccessControlsProps): ReactElement {
  return createElement(
    "div",
    { className: "space-y-6", "data-component": "team-access-controls" },
    workspaceRoleControl(props),
    resourceSection("Service access", "Choose which services this team can use or manage.", "service", props.services, props),
    resourceSection("Credential access", "Choose which credential sets this team can use or manage.", "bucket", props.buckets, props),
    appSection(props)
  );
}

export function teamAppAccessLevels(team: Team, appFamilyId: string): TeamAppAccessLevel[] {
  return team.bindings.flatMap((item) => {
    if (item.resource_type !== "app" || item.resource_id !== appFamilyId) return [];
    if (item.role_slug === "app-manager") return ["MANAGER"];
    if (item.role_slug === "app-reader") return ["READER"];
    return [];
  });
}

export function teamWorkspaceRole(team: Team): TeamWorkspaceRole | "NONE" {
  const binding = team.bindings.find((item) => item.resource_type === "workspace");
  const role = binding?.role_slug.toUpperCase();
  if (role === "OWNER" || role === "ADMIN" || role === "BUILDER" || role === "VIEWER") return role;
  return "NONE";
}

export function changeWorkspaceRole(value: string, onChange: (role: TeamWorkspaceRole | null) => void): void {
  if (value === "NONE") {
    onChange(null);
    return;
  }
  if (value === "OWNER" || value === "ADMIN" || value === "BUILDER" || value === "VIEWER") onChange(value);
}

export function teamResourceLevel(team: Team, resourceType: TeamResourceType, resourceId: string): TeamAccessLevel | null {
  const levels = teamResourceLevels(team, resourceType, resourceId);
  if (levels.includes("MANAGER")) return "MANAGER";
  if (levels.includes("USER")) return "USER";
  return null;
}

export function teamResourceLevels(team: Team, resourceType: TeamResourceType, resourceId: string): TeamAccessLevel[] {
  return team.bindings.flatMap((item) => {
    if (item.resource_type !== resourceType || item.resource_id !== resourceId) return [];
    if (item.role_slug === `${resourceType}-manager`) return ["MANAGER"];
    if (item.role_slug === `${resourceType}-user`) return ["USER"];
    return [];
  });
}

function workspaceRoleControl(props: TeamAccessControlsProps): ReactElement {
  const current = teamWorkspaceRole(props.team);
  // Preserve Owner as the visible current value, but never offer Owner as an
  // escalation choice to an Admin. The server repeats this account.manage
  // boundary; this branch keeps the UI honest rather than acting as security.
  const roles = props.canManageOwners || current === "OWNER"
    ? workspaceRoles
    : workspaceRoles.filter(([value]) => value !== "OWNER");
  return createElement(
    "label",
    { className: "block" },
    createElement("span", { className: "block text-sm font-semibold text-slate-800 mb-1" }, "Workspace role"),
    createElement("span", { className: "block text-xs text-slate-500 mb-2" }, "Sets the team's general workspace capabilities."),
    createElement(
      "select",
      {
        value: current,
        disabled: props.disabled || (current === "OWNER" && !props.canManageOwners),
        onChange: (event: ChangeEvent<HTMLSelectElement>) => changeWorkspaceRole(event.target.value, props.onWorkspaceRoleChange),
        className: "w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm",
        "aria-label": "Workspace role",
      },
      ...roles.map(([value, label]) => createElement("option", { key: value, value }, label))
    )
  );
}

function resourceSection(
  title: string,
  description: string,
  resourceType: TeamResourceType,
  resources: TeamAccessResource[],
  props: TeamAccessControlsProps
): ReactElement {
  const rows = resources.length === 0
    ? [createElement("p", { key: "empty", className: "text-sm text-slate-500 py-3" }, `No ${resourceType}s are available.`)]
    : resources.map((resource) => resourceRow(resourceType, resource, props));
  return createElement(
    "section",
    { className: "rounded-lg border border-slate-200 overflow-hidden" },
    createElement("div", { className: "bg-slate-50 px-4 py-3 border-b border-slate-200" },
      createElement("h3", { className: "text-sm font-semibold text-slate-900" }, title),
      createElement("p", { className: "text-xs text-slate-500 mt-0.5" }, description)
    ),
    createElement("div", { className: "divide-y divide-slate-100 px-4" }, ...rows)
  );
}

function resourceRow(resourceType: TeamResourceType, resource: TeamAccessResource, props: TeamAccessControlsProps): ReactElement {
  const current = teamResourceLevel(props.team, resourceType, resource.id) ?? "NONE";
  return createElement(
    "label",
    { key: resource.id, className: "flex items-center justify-between gap-4 py-3" },
    createElement("span", { className: "text-sm text-slate-800 truncate" }, resource.name),
    createElement(
      "select",
      {
        value: current,
        disabled: props.disabled,
        onChange: (event: ChangeEvent<HTMLSelectElement>) => {
          const level = event.target.value === "NONE" ? null : event.target.value as TeamAccessLevel;
          props.onResourceAccessChange(resourceType, resource.id, level);
        },
        className: "w-32 rounded-lg border border-slate-300 bg-white px-2 py-1.5 text-sm",
        "aria-label": `${resource.name} access`,
      },
      ...resourceLevels.map(([value, label]) => createElement("option", { key: value, value }, label))
    )
  );
}

function appSection(props: TeamAccessControlsProps): ReactElement {
  const bindings = uniqueAppBindings(props.team);
  const rows = bindings.map((binding) => appRow(binding, props));
  return createElement(
    "section",
    { className: "rounded-lg border border-slate-200 overflow-hidden", "data-section": "app-access" },
    createElement(
      "div",
      { className: "bg-slate-50 px-4 py-3 border-b border-slate-200" },
      createElement("h3", { className: "text-sm font-semibold text-slate-900" }, "App and MCP server access"),
      createElement("p", { className: "text-xs text-slate-500 mt-0.5" }, "Share an app or MCP server with this team by its ID. Read allows viewing; Manage also allows changes and token management.")
    ),
    createElement(
      "div",
      { className: "divide-y divide-slate-100 px-4" },
      ...(rows.length > 0 ? rows : [createElement("p", { key: "empty", className: "text-sm text-slate-500 py-3" }, "No SDKs or MCP servers are shared with this team.")]),
      appGrantForm(props)
    )
  );
}

function appGrantForm(props: TeamAccessControlsProps): ReactElement {
  return createElement(
    "form",
    {
      className: "grid gap-2 py-3 sm:grid-cols-[1fr_110px_auto]",
      onSubmit: (event: FormEvent<HTMLFormElement>) => submitAppGrant(event, props),
    },
    createElement("input", {
      name: "app_family_id",
      required: true,
      disabled: props.disabled,
      placeholder: "App family ID",
      "aria-label": "App family ID",
      className: "rounded-lg border border-slate-300 px-3 py-2 text-sm",
    }),
    createElement(
      "select",
      { name: "level", disabled: props.disabled, "aria-label": "App or server access", className: "rounded-lg border border-slate-300 bg-white px-2 py-2 text-sm" },
      createElement("option", { value: "READER" }, "Read"),
      createElement("option", { value: "MANAGER" }, "Manage")
    ),
    createElement("button", { type: "submit", disabled: props.disabled, className: "rounded-lg border border-slate-300 px-3 py-2 text-sm font-semibold text-slate-700 disabled:opacity-50" }, "Share")
  );
}

function submitAppGrant(event: FormEvent<HTMLFormElement>, props: TeamAccessControlsProps): void {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const appFamilyId = String(form.get("app_family_id") || "").trim();
  const level = form.get("level") === "MANAGER" ? "MANAGER" : "READER";
  if (!appFamilyId) return;
  props.onAppAccessChange(appFamilyId, level);
  event.currentTarget.reset();
}

function appRow(binding: Team["bindings"][number], props: TeamAccessControlsProps): ReactElement {
  const levels = teamAppAccessLevels(props.team, binding.resource_id);
  const current = levels.includes("MANAGER") ? "MANAGER" : levels.includes("READER") ? "READER" : "NONE";
  const name = binding.resource_display_name || `Build ${binding.resource_id}`;
  return createElement(
    "label",
    { key: binding.resource_id, className: "flex items-center justify-between gap-4 py-3" },
    createElement("span", { className: "min-w-0" },
      createElement("span", { className: "block text-sm text-slate-800 truncate" }, name),
      createElement("span", { className: "block text-xs text-slate-400 truncate" }, binding.resource_id)
    ),
    createElement(
      "select",
      {
        value: current,
        disabled: props.disabled,
        onChange: (event: ChangeEvent<HTMLSelectElement>) => props.onAppAccessChange(binding.resource_id, appLevel(event.target.value)),
        className: "w-32 rounded-lg border border-slate-300 bg-white px-2 py-1.5 text-sm",
        "aria-label": `${name} access`,
      },
      ...appLevels.map(([value, label]) => createElement("option", { key: value, value }, label))
    )
  );
}

function appLevel(value: string): TeamAppAccessLevel | null {
  if (value === "READER" || value === "MANAGER") return value;
  return null;
}

function uniqueAppBindings(team: Team): Team["bindings"] {
  const seen = new Set<string>();
  return team.bindings.filter((binding) => {
    if (binding.resource_type !== "app" || seen.has(binding.resource_id)) return false;
    seen.add(binding.resource_id);
    return true;
  });
}
