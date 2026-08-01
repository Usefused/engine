import { createElement, type ChangeEvent, type ReactElement } from "react";
import type { ArtifactBuildSelector, ArtifactOwningTeam } from "../../lib/artifact-builder-contract";

interface ArtifactOwnerControlsProps {
  ownerTeams: ArtifactOwningTeam[];
  ownerTeamId: string;
  buckets: ArtifactBuildSelector[];
  bucketId: string;
  onOwnerTeamChange: (teamId: string) => void;
  onBucketChange: (bucketId: string) => void;
}

export function ArtifactOwnerControls(props: ArtifactOwnerControlsProps): ReactElement {
  return createElement(
    "div",
    { className: "space-y-5", "data-component": "artifact-owner-controls" },
    createElement(
      "div",
      { className: "rounded-xl border border-blue-100 bg-blue-50/60 p-3 text-xs text-blue-900" },
      "Fused checks both your access and the owning team's access. Only services and buckets available to both are shown."
    ),
    ownerTeamControl(props),
    bucketControl(props)
  );
}

function ownerTeamControl(props: ArtifactOwnerControlsProps): ReactElement {
  return createElement(
    "div",
    null,
    createElement("label", { className: "block text-sm font-medium text-slate-700 mb-1.5" }, "Owning team"),
    createElement(
      "select",
      {
        required: true,
        value: props.ownerTeamId,
        onChange: (event: ChangeEvent<HTMLSelectElement>) => props.onOwnerTeamChange(event.target.value),
        className: "w-full px-3 py-2.5 rounded-lg border border-slate-300 text-sm bg-white",
        "data-track": "select_artifact_owner_team",
      },
      createElement("option", { value: "" }, "Choose a team"),
      ...props.ownerTeams.map((team) => createElement("option", { key: team.id, value: team.id }, team.name))
    ),
    props.ownerTeams.length === 0
      ? createElement("p", { className: "mt-1.5 text-xs text-amber-700" }, "Join an active team, or ask an access administrator to add you.")
      : null
  );
}

function bucketControl(props: ArtifactOwnerControlsProps): ReactElement {
  return createElement(
    "div",
    null,
    createElement("label", { className: "block text-sm font-medium text-slate-700 mb-1.5" }, "Credential bucket"),
    createElement(
      "select",
      {
        required: true,
        value: props.bucketId,
        disabled: !props.ownerTeamId || props.buckets.length === 0,
        onChange: (event: ChangeEvent<HTMLSelectElement>) => props.onBucketChange(event.target.value),
        className: "w-full px-3 py-2.5 rounded-lg border border-slate-300 text-sm bg-white disabled:bg-slate-100",
        "data-track": "select_artifact_bucket",
      },
      createElement("option", { value: "" }, "Choose a bucket"),
      ...props.buckets.map((bucket) => createElement("option", { key: bucket.resource_id, value: bucket.resource_id }, bucket.display_name))
    ),
    props.ownerTeamId && props.buckets.length === 0
      ? createElement("p", { className: "mt-1.5 text-xs text-amber-700" }, "You and this team do not share access to a credential bucket.")
      : null
  );
}
