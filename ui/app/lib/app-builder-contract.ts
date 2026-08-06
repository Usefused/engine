export type AppKind = "sdk" | "mcp";
export type AppSelectorResourceType = "SERVICE" | "BUCKET";

export interface AppOwningTeam {
  id: string;
  name: string;
  slug: string;
}

export interface AppBuildSelector {
  resource_type: AppSelectorResourceType;
  resource_id: string;
  display_name: string;
}

export interface AppOwningTeamPage {
  items: AppOwningTeam[];
  total: number;
}

export interface AppBuildSelectorPage {
  items: AppBuildSelector[];
  total: number;
}

export interface AppPlanResponse {
  plan_id: string;
  owner_type: "subject" | "team";
  config_key: string;
  source_hash: string;
  summary: Record<string, unknown>;
}

export interface AppPlanInput {
  owner_team?: string;
  config_key: string;
  source_hash: string;
  config: Record<string, unknown>;
}

export interface AppApplyInput {
  plan_id: string;
  source_hash: string;
}

export const APP_BUILDER_OPERATIONS = {
  owningTeams: `
    query AppOwningTeams($search: String, $limit: Int!, $offset: Int!) {
      appOwningTeams(search: $search, limit: $limit, offset: $offset) {
        total
        items { id name slug }
      }
    }
  `,
  selectors: `
    query AppBuildSelectors($ownerTeamId: ID, $resourceType: AppSelectorResourceType!, $search: String, $limit: Int!, $offset: Int!) {
      appBuildSelectors(owner_team_id: $ownerTeamId, resource_type: $resourceType, search: $search, limit: $limit, offset: $offset) {
        total
        items {
          resource_type resource_id display_name
        }
      }
    }
  `,
} as const;

export function appConfigKey(kind: AppKind, config: Record<string, unknown>): string {
  const name = requiredAppText(config.name, "name");
  return `${kind}:${name}:${requiredAppText(config.version, "version")}`;
}

export function appPlanInput(
  kind: AppKind,
  ownerTeamSlug: string,
  sourceHash: string,
  config: Record<string, unknown>,
): AppPlanInput {
	const input: AppPlanInput = {
    config_key: appConfigKey(kind, config),
    source_hash: sourceHash,
    config,
  };
	if (ownerTeamSlug.trim()) input.owner_team = ownerTeamSlug.trim();
	return input;
}

export function appApplyInput(plan: AppPlanResponse): AppApplyInput {
  // Apply proves intent with the saved plan and hash. It deliberately has no
  // owner override, preventing a browser or agent from changing ownership.
  return { plan_id: plan.plan_id, source_hash: plan.source_hash };
}

function requiredAppText(value: unknown, field: string): string {
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(`App ${field} is required.`);
  }
  return value.trim();
}
