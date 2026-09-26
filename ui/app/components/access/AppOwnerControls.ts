import { createElement, type ChangeEvent, type ReactElement } from "react";
import type { AppBuildSelector, AppOwningTeam } from "../../lib/app-builder-contract";

interface AppOwnerControlsProps {
  ownerTeams: AppOwningTeam[];
  ownerTeamId: string;
  buckets: AppBuildSelector[];
  bucketId: string;
  onOwnerTeamChange: (teamId: string) => void;
  onBucketChange: (bucketId: string) => void;
  onCreateCredential?: () => void;
}

/** Keeps ownership and credential choices compact while explaining team-scoped access when relevant. */
export function AppOwnerControls(props: AppOwnerControlsProps): ReactElement {
  return createElement(
    "div",
    { className: "space-y-3", "data-component": "app-owner-controls" },
    // Personal ownership is already explicit in the selector; team ownership needs its access warning.
    props.ownerTeamId ? createElement(
      "div",
      { className: "rounded-lg border border-blue-100 bg-blue-50/60 p-2 text-xs leading-4 text-blue-900" },
      "Fused checks both your access and the owning team's access. Only services and credential sets available to both are shown."
    ) : null,
    ownerTeamControl(props),
    bucketControl(props)
  );
}

/** Renders the ownership selector without repeating its personal default below the field. */
function ownerTeamControl(props: AppOwnerControlsProps): ReactElement {
  return createElement(
    "div",
    null,
    createElement("label", { className: "mb-1 block text-sm font-medium text-slate-700" }, "Owning team"),
    createElement(
      "select",
      {
        value: props.ownerTeamId,
        onChange: (event: ChangeEvent<HTMLSelectElement>) => props.onOwnerTeamChange(event.target.value),
        className: "w-full rounded-lg border border-slate-300 bg-white px-2.5 py-1.5 text-sm",
        "data-track": "select_app_owner_team",
      },
      createElement("option", { value: "" }, "Personal (you)"),
      ...props.ownerTeams.map((team) => createElement("option", { key: team.id, value: team.id }, team.name))
    )
  );
}

/** Keeps the credential selector and its empty-state guidance close to the create action. */
function bucketControl(props: AppOwnerControlsProps): ReactElement {
  return createElement(
    "div",
    null,
    createElement(
      "div",
      { className: "mb-1 flex items-center justify-between gap-3" },
      createElement("label", { className: "block text-sm font-medium text-slate-700" }, "Credential set"),
      props.onCreateCredential
        ? createElement(
            "button",
            {
              type: "button",
              onClick: props.onCreateCredential,
              className: "text-xs font-semibold text-blue-600 hover:text-blue-700 hover:underline",
              "data-track": "create_builder_credential",
            },
            "Create credential"
          )
        : null
    ),
    createElement(
      "select",
      {
        required: true,
        value: props.bucketId,
        disabled: props.buckets.length === 0,
        onChange: (event: ChangeEvent<HTMLSelectElement>) => props.onBucketChange(event.target.value),
        className: "w-full rounded-lg border border-slate-300 bg-white px-2.5 py-1.5 text-sm disabled:bg-slate-100",
        "data-track": "select_app_bucket",
      },
      createElement("option", { value: "" }, "Choose a credential set"),
      ...props.buckets.map((bucket) => createElement("option", { key: bucket.resource_id, value: bucket.resource_id }, bucket.display_name))
    ),
    props.buckets.length === 0
      ? createElement("p", { className: "mt-1.5 text-xs text-amber-700" }, props.ownerTeamId ? "You and this team do not share access to a credential set." : "You do not have access to a credential set.")
      : null,
    // Once a credential is available, the adjacent create link explains how to add another.
    props.onCreateCredential && props.buckets.length === 0
      ? createElement(
          "p",
          { className: "mt-1 text-xs text-slate-500" },
          props.ownerTeamId
            ? "Create it in a new tab, then return here. Only sets shared with the owning team will appear."
            : "Create it in a new tab, then return here; this list refreshes automatically."
        )
      : null
  );
}
