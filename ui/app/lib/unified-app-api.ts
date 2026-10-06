import type { ChooseDescribeService, DescribeProgress, DescribeSelectionReady } from "./app-describe-contract";
import { describeApp } from "./app-describe-api";
import { api } from "./api";
import { APIRequestError } from "./authorization-error";
import { decodeUnifiedRelease, type UnifiedDraft, type UnifiedRelease, type UnifiedTemplate } from "./unified-app-contract";
import type { AppPlanResponse } from "./app-builder-contract";

type WireRelease = Omit<UnifiedRelease, "template"> & { template: string };
const releaseFields = `id publisher hash public is_owner template`;

/** Preserves the Unified App adapter while sharing discovery with SDK and MCP creation. */
export async function describeUnifiedApp(goal: string, progress: DescribeProgress, chooseService?: ChooseDescribeService, selectionReady?: DescribeSelectionReady): Promise<UnifiedDraft> {
  const proposal = await describeApp(goal, "unified_app", progress, chooseService, selectionReady);
  return { ...proposal, source: proposal.source! };
}

/** Compiles a fresh plan; agent callers can prohibit dependency activation and persisted metadata repair. */
export async function planUnifiedApp(config: Record<string, unknown>, services: UnifiedDraft["services"], progress: (message: string) => void, ownerTeam = "", allowDependencyChanges = true): Promise<AppPlanResponse> {
  const activated: string[] = [];
  try {
    const workspace = await api.workspace.getServices();
    for (const [key, pin] of Object.entries(services)) {
      const enabled = workspace.find((service) => service.service_id === pin.service_id)?.enabled_versions?.some((version) => version.service_version_id === pin.service_version_id && version.status !== "deprecated");
      // Reviewed, already-active dependencies remain untouched.
      if (enabled) continue;
      // Agent compilation cannot silently activate workspace capabilities; the user's compile action owns setup.
      if (!allowDependencyChanges) throw new Error(`Enable ${key} ${pin.version} using Validate and compile before agent validation.`);
      progress(`Enabling ${key} ${pin.version}…`);
      await api.workspace.addService(pin.service_id, key, pin.version, pin.service_version_id);
      activated.push(key);
    }
    progress("Compiling and validating your app…");
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(JSON.stringify(config)));
    const source_hash = `sha256:${Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
    const request = { owner_team: ownerTeam, config_key: `unified_app:${config.name}:${config.version}`, source_hash, config };
    try {
      return await api.appConfig.plan<AppPlanResponse>("unified-app", request);
    } catch (cause) {
      // Only a typed legacy-identity failure permits refreshing exact reviewed pins; other failures remain untouched.
      if (!(cause instanceof APIRequestError) || cause.code !== "service_provider_identity_unavailable") throw cause;
      // Repairing persisted service metadata remains an explicit user action in the form.
      if (!allowDependencyChanges) throw cause;
      await refreshUnifiedServiceContracts(services, progress);
      progress("Service metadata refreshed. Compiling and validating your app…");
      // A single fresh plan binds the refreshed contract; a repeated failure must surface instead of looping.
      return await api.appConfig.plan<AppPlanResponse>("unified-app", request);
    }
  } catch (cause) {
    // Service activation is a separate durable boundary and must be visible after partial failure.
    throw new Error(`${String(cause)} Services enabled during this attempt: ${activated.join(", ") || "none"}.`);
  }
}

/** Repairs saved identities without selecting newer versions or changing authored operations. */
async function refreshUnifiedServiceContracts(services: UnifiedDraft["services"], progress: (message: string) => void): Promise<void> {
  const refreshed = new Set<string>();
  for (const [key, pin] of Object.entries(services)) {
    const identity = `${pin.service_id}:${pin.service_version_id}`;
    // Multiple aliases of one immutable version share one authoritative refresh.
    if (refreshed.has(identity)) continue;
    progress(`Refreshing saved metadata for ${key} ${pin.version}…`);
    await api.workspace.refreshServiceContract(pin.service_id, pin.service_version_id);
    refreshed.add(identity);
  }
}

/** Pages published templates through Registry's authenticated catalogue transport. */
export async function listUnifiedTemplates(search = "", offset = 0, ids: string[] = []): Promise<{ items: UnifiedRelease[]; total: number }> {
  const result = await api.graphql<{ unifiedAppTemplates: { items: WireRelease[]; total: number } }>(`query UnifiedAppTemplates($search: String!, $ids: [ID!], $limit: Int!, $offset: Int!) {
    unifiedAppTemplates(search: $search, ids: $ids, limit: $limit, offset: $offset) { total items { ${releaseFields} } }
  }`, { search, ids, limit: 20, offset });
  return { total: result.unifiedAppTemplates.total, items: result.unifiedAppTemplates.items.map(decodeUnifiedRelease) };
}

/** Publishes only the explicit portable template; bucket and app credentials are never copied. */
export async function publishUnifiedTemplate(template: UnifiedTemplate, isPublic: boolean): Promise<UnifiedRelease> {
  const result = await api.graphql<{ publishUnifiedAppTemplate: WireRelease }>(`mutation PublishUnifiedAppTemplate($template: String!, $public: Boolean!) {
    publishUnifiedAppTemplate(template: $template, public: $public) { ${releaseFields} }
  }`, { template: JSON.stringify(template), public: isPublic });
  return decodeUnifiedRelease(result.publishUnifiedAppTemplate);
}

/** Changes discoverability independently of immutable template content. */
export async function setUnifiedTemplateVisibility(id: string, isPublic: boolean): Promise<UnifiedRelease> {
  const result = await api.graphql<{ setUnifiedAppTemplateVisibility: WireRelease }>(`mutation SetUnifiedAppTemplateVisibility($id: ID!, $public: Boolean!) {
    setUnifiedAppTemplateVisibility(id: $id, public: $public) { ${releaseFields} }
  }`, { id, public: isPublic });
  return decodeUnifiedRelease(result.setUnifiedAppTemplateVisibility);
}
