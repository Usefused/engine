import { api } from "./api";
import type { UnifiedAppSource } from "./unified-app-edit";
import type { UnifiedTemplate } from "./unified-app-contract";
import { checkTemplateDependency, type TemplateChecks, type TemplateDependency } from "./unified-template";

export interface TemplateApp { app_family_id: string; app_id: string | null; name: string; version: string | null }
export interface TemplateAppVersion { app_id: string; version: string }

/** Pages the authorized Unified App catalogue without dropping apps beyond the first page. */
export async function listTemplateApps(search: string, offset: number, archived: boolean) {
  const result = await api.mcpGraphql<{ appFamilies: { items: TemplateApp[]; total: number } }>(`query TemplateApps($search: String!, $offset: Int!, $archived: Boolean!) {
    appFamilies(kind: "unified_app", search: $search, archived: $archived, limit: 20, offset: $offset) {
      items { app_family_id app_id: latest_version_id name version: latest_version } total
    }
  }`, { search, offset, archived });
  return result.appFamilies;
}

/** Resolves version history from an authorized immutable app, including the create-flow return link. */
export async function listTemplateAppVersions(appID: string): Promise<TemplateAppVersion[]> {
  const result = await api.mcpGraphql<{ app: { app_family_id: string; kind: string } | null }>(`query TemplateAppIdentity($id: String!) {
    app(app_id: $id) { app_family_id kind }
  }`, { id: appID });
  // A manipulated return URL cannot turn another app kind into a source template.
  if (result.app?.kind !== "unified_app") throw new Error("Unified App not found.");
  const history = await api.mcpGraphql<{ appVersions: TemplateAppVersion[] }>(`query TemplateAppVersions($family: String!) {
    appVersions(app_family_id: $family) { app_id version }
  }`, { family: result.app.app_family_id });
  return history.appVersions;
}

/** Reads private source through the existing family-manage permission boundary. */
export function readTemplateApp(appID: string): Promise<UnifiedAppSource> {
  return api.appConfig.source<UnifiedAppSource>(appID);
}

/** Checks exact Registry contracts without activating services or changing their visibility. */
export async function checkTemplate(template: Pick<UnifiedTemplate, "services">): Promise<TemplateChecks> {
  const checks = await Promise.all(Object.entries(template.services).map(async ([key, pin]) => {
    const data = await api.graphql<TemplateDependency>(`query TemplateDependency($id: String!, $version: String!) {
      service(id: $id, version: $version) { id canonical_ref is_public }
      serviceVersions(serviceId: $id) { id name status is_public }
      serviceOperations(serviceId: $id, version: $version) { name }
    }`, { id: pin.service_id, version: pin.version });
    return checkTemplateDependency(key, pin, data);
  }));
  return { blockers: checks.flatMap((item) => item.blockers), publicBlockers: checks.flatMap((item) => item.publicBlockers) };
}
