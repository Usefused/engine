import { api } from "./api";
import {
  ARTIFACT_BUILDER_OPERATIONS,
  artifactApplyInput,
  artifactPlanInput,
  type ArtifactBuildSelectorPage,
  type ArtifactKind,
  type ArtifactOwningTeamPage,
  type ArtifactPlanResponse,
  type ArtifactSelectorResourceType,
} from "./artifact-builder-contract";

export async function listArtifactOwningTeams(search = "", limit = 50, offset = 0): Promise<ArtifactOwningTeamPage> {
  const data = await api.mcpGraphql<{ artifactOwningTeams: ArtifactOwningTeamPage }>(ARTIFACT_BUILDER_OPERATIONS.owningTeams, { search, limit, offset });
  return data.artifactOwningTeams;
}

export async function listArtifactBuildSelectors(
  ownerTeamId: string,
  resourceType: ArtifactSelectorResourceType,
  search = "",
  limit = 100,
  offset = 0,
): Promise<ArtifactBuildSelectorPage> {
  const data = await api.mcpGraphql<{ artifactBuildSelectors: ArtifactBuildSelectorPage }>(ARTIFACT_BUILDER_OPERATIONS.selectors, {
    ownerTeamId,
    resourceType,
    search,
    limit,
    offset,
  });
  return data.artifactBuildSelectors;
}

export async function planArtifact(
  kind: ArtifactKind,
  ownerTeamId: string,
  config: Record<string, unknown>,
): Promise<ArtifactPlanResponse> {
  const sourceHash = await sha256JSON(config);
  return api.artifactConfig.plan<ArtifactPlanResponse>(kind, artifactPlanInput(kind, ownerTeamId, sourceHash, config));
}

export async function applyArtifact<T>(kind: ArtifactKind, plan: ArtifactPlanResponse): Promise<T> {
  return api.artifactConfig.apply<T>(kind, artifactApplyInput(plan));
}

export async function planAndApplyArtifact<T>(kind: ArtifactKind, ownerTeamId: string, config: Record<string, unknown>): Promise<T> {
  const plan = await planArtifact(kind, ownerTeamId, config);
  return applyArtifact<T>(kind, plan);
}

async function sha256JSON(value: Record<string, unknown>): Promise<string> {
  const bytes = new TextEncoder().encode(JSON.stringify(value));
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return `sha256:${Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}
