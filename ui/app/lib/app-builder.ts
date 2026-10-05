import { api } from "./api";
import {
  APP_BUILDER_OPERATIONS,
  appApplyInput,
  appPlanInput,
  type AppBuildSelectorPage,
  type AppKind,
  type AppOwningTeamPage,
  type AppPlanResponse,
  type AppPlanReviewer,
  type AppSelectorResourceType,
} from "./app-builder-contract";

export async function listAppOwningTeams(search = "", limit = 50, offset = 0): Promise<AppOwningTeamPage> {
  const data = await api.mcpGraphql<{ appOwningTeams: AppOwningTeamPage }>(APP_BUILDER_OPERATIONS.owningTeams, { search, limit, offset });
  return data.appOwningTeams;
}

export async function listAppBuildSelectors(
  ownerTeamId: string,
  resourceType: AppSelectorResourceType,
  search = "",
  limit = 100,
  offset = 0,
): Promise<AppBuildSelectorPage> {
  const data = await api.mcpGraphql<{ appBuildSelectors: AppBuildSelectorPage }>(APP_BUILDER_OPERATIONS.selectors, {
	ownerTeamId: ownerTeamId || null,
    resourceType,
    search,
    limit,
    offset,
  });
  return data.appBuildSelectors;
}

export async function planApp(
  kind: AppKind,
  ownerTeamSlug: string,
  config: Record<string, unknown>,
): Promise<AppPlanResponse> {
  const sourceHash = await sha256JSON(config);
	return api.appConfig.plan<AppPlanResponse>(kind, appPlanInput(kind, ownerTeamSlug, sourceHash, config));
}

export async function applyApp<T>(kind: AppKind, plan: AppPlanResponse): Promise<T> {
  return api.appConfig.apply<T>(kind, appApplyInput(plan));
}

/** Defers publication until credential readiness has been reviewed, preserving cancellation and fresh plan receipts. */
export async function planAndApplyApp<T>(kind: AppKind, ownerTeamSlug: string, config: Record<string, unknown>, review: AppPlanReviewer): Promise<T | null> {
	const plan = await planApp(kind, ownerTeamSlug, config);
  const approved = await review(plan, () => planApp(kind, ownerTeamSlug, config));
  // Cancelling credential setup leaves the reviewed config editable and creates no app.
  if (!approved) return null;
  return applyApp<T>(kind, approved);
}

async function sha256JSON(value: Record<string, unknown>): Promise<string> {
  const bytes = new TextEncoder().encode(JSON.stringify(value));
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return `sha256:${Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}
