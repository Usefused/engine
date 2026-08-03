export type ArtifactKind = "sdk" | "mcp" | "webhook";
export type ArtifactSelectorResourceType = "SERVICE" | "BUCKET";

export interface ArtifactOwningTeam {
  id: string;
  name: string;
  slug: string;
}

export interface ArtifactBuildSelector {
  resource_type: ArtifactSelectorResourceType;
  resource_id: string;
  display_name: string;
}

export interface ArtifactOwningTeamPage {
  items: ArtifactOwningTeam[];
  total: number;
}

export interface ArtifactBuildSelectorPage {
  items: ArtifactBuildSelector[];
  total: number;
}

export interface ArtifactPlanResponse {
  plan_id: string;
  owner_type: "subject" | "team";
  config_key: string;
  source_hash: string;
  summary: Record<string, unknown>;
}

export interface ArtifactPlanInput {
  owner_team?: string;
  config_key: string;
  source_hash: string;
  config: Record<string, unknown>;
}

export interface ArtifactApplyInput {
  plan_id: string;
  source_hash: string;
}

export const ARTIFACT_BUILDER_OPERATIONS = {
  owningTeams: `
    query ArtifactOwningTeams($search: String, $limit: Int!, $offset: Int!) {
      artifactOwningTeams(search: $search, limit: $limit, offset: $offset) {
        total
        items { id name slug }
      }
    }
  `,
  selectors: `
    query ArtifactBuildSelectors($ownerTeamId: ID, $resourceType: ArtifactSelectorResourceType!, $search: String, $limit: Int!, $offset: Int!) {
      artifactBuildSelectors(owner_team_id: $ownerTeamId, resource_type: $resourceType, search: $search, limit: $limit, offset: $offset) {
        total
        items {
          resource_type resource_id display_name
        }
      }
    }
  `,
} as const;

export function artifactConfigKey(kind: ArtifactKind, config: Record<string, unknown>): string {
  const name = requiredArtifactText(config.name, "name");
  if (kind === "webhook") return `webhook:${name}`;
  return `${kind}:${name}:${requiredArtifactText(config.version, "version")}`;
}

export function artifactPlanInput(
  kind: ArtifactKind,
  ownerTeamSlug: string,
  sourceHash: string,
  config: Record<string, unknown>,
): ArtifactPlanInput {
	const input: ArtifactPlanInput = {
    config_key: artifactConfigKey(kind, config),
    source_hash: sourceHash,
    config,
  };
	if (ownerTeamSlug.trim()) input.owner_team = ownerTeamSlug.trim();
	return input;
}

export function artifactApplyInput(plan: ArtifactPlanResponse): ArtifactApplyInput {
  // Apply proves intent with the saved plan and hash. It deliberately has no
  // owner override, preventing a browser or agent from changing ownership.
  return { plan_id: plan.plan_id, source_hash: plan.source_hash };
}

function requiredArtifactText(value: unknown, field: string): string {
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(`Artifact ${field} is required.`);
  }
  return value.trim();
}
