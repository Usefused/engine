import type { ChooseDescribeService, DescribeProgress, DescribeSelectionReady } from "./app-describe-contract";
import { describeApp } from "./app-describe-api";
import { api } from "./api";
import { decodeUnifiedRelease, type UnifiedDraft, type UnifiedRelease, type UnifiedTemplate } from "./unified-app-contract";
import type { AppPlanResponse } from "./app-builder-contract";

type WireRelease = Omit<UnifiedRelease, "template"> & { template: string };
const releaseFields = `id publisher hash public is_owner template`;

/** Preserves the Unified App adapter while sharing discovery with SDK and MCP creation. */
export async function describeUnifiedApp(goal: string, progress: DescribeProgress, chooseService?: ChooseDescribeService, selectionReady?: DescribeSelectionReady): Promise<UnifiedDraft> {
  const proposal = await describeApp(goal, "unified_app", progress, chooseService, selectionReady);
  return { ...proposal, source: proposal.source! };
}

/** Activates the reviewed exact dependencies, retaining successful activations if a later stage fails. */
export async function planUnifiedApp(config: Record<string, unknown>, services: UnifiedDraft["services"], progress: (message: string) => void): Promise<AppPlanResponse> {
  const activated: string[] = [];
  try {
    const workspace = await api.workspace.getServices();
    for (const [key, pin] of Object.entries(services)) {
      const enabled = workspace.find((service) => service.service_id === pin.service_id)?.enabled_versions?.some((version) => version.service_version_id === pin.service_version_id && version.status !== "deprecated");
      // Reviewed, already-active dependencies remain untouched.
      if (enabled) continue;
      progress(`Enabling ${key} ${pin.version}…`);
      await api.workspace.addService(pin.service_id, key, pin.version, pin.service_version_id);
      activated.push(key);
    }
    progress("Compiling and validating your app…");
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(JSON.stringify(config)));
    const source_hash = `sha256:${Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
    return await api.appConfig.plan<AppPlanResponse>("unified-app", { config_key: `unified_app:${config.name}:${config.version}`, source_hash, config });
  } catch (cause) {
    // Service activation is a separate durable boundary and must be visible after partial failure.
    throw new Error(`${String(cause)} Services enabled during this attempt: ${activated.join(", ") || "none"}.`);
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
