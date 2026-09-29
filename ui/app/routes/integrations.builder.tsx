import { useLoaderData, useSearchParams, redirect, type MetaFunction } from "@remix-run/react";
import { api, type Service } from "~/lib/api";
import { appCreationModeFromSearch } from "~/lib/app-builder-contract";
import { AppServiceBuilder, builderPageTitle, existingBuilderMode, type Workflow, type WorkflowAppConfig, type WorkflowBuilderPin } from "~/components/apps/AppServiceBuilder";
import type { HostedAppReference } from "~/lib/unified-app-consumer";

// meta preserves shared metadata while naming the app builder route.
export const meta: MetaFunction<typeof clientLoader> = ({ matches, data, location }) => {
  const parentMeta = matches.filter((m) => m.id === "root").flatMap((m) => m.meta ?? []);
  // Loader-owned destinations pin delivery type; ordinary new apps default to all three methods.
  const mode = data?.source ? existingBuilderMode(data.source.config) : appCreationModeFromSearch(new URLSearchParams(location.search));
  return [
    ...parentMeta.filter((m) => !('title' in m)),
    { title: builderPageTitle(mode) },
  ];
};
// clientLoader requires an authenticated Engine session before building an app.
export const clientLoader = async ({ request }: { request: Request }) => {
	const session = await api.auth.session().catch(() => ({ authenticated: false }));
	const url = new URL(request.url);
	if (!session.authenticated) {
    return redirect(`/login?next=${encodeURIComponent(url.pathname + url.search)}`);
  }

  // Team-aware selectors are Engine-owned and depend on the user's chosen
  // owner. Do not broad-load Registry services before that choice exists.
  const workflows: Workflow[] = [];
  const appID = url.searchParams.get("app") ?? "";
  // Existing private config stays on Engine and requires manage authority for this exact app.
  const source = appID ? { ...(await api.appConfig.source<{ config: WorkflowAppConfig; owner_team: string; service_pins: WorkflowBuilderPin[] }>(appID)), appID } : null;
  const attachmentID = url.searchParams.get("unifiedApp");
  // Deep links carry exact identity; names and versions come from authorized Engine metadata.
  const attachment = attachmentID ? (await api.mcpGraphql<{ app: HostedAppReference }>(`query UnifiedAppConsumerSource($id: String!) { app(app_id: $id) { app_id kind name version } }`, { id: attachmentID })).app : null;
  if (attachmentID && attachment?.kind !== "unified_app") throw new Error("Unified App not found.");
  return { services: [] as Service[], total: 0, isAuth: true, workflows, source, attachment };
};

/** Keeps loader authority at the route boundary while sharing the full authoring component. */
export default function SdkBuilder() {
  const loaderData = useLoaderData<typeof clientLoader>();
  const [params] = useSearchParams();
  // Delivery or destination changes cannot inherit a pending proposal from another creation flow.
  const identity = [appCreationModeFromSearch(params), loaderData.source?.appID, loaderData.attachment?.app_id].join(":");
  return <AppServiceBuilder key={identity} loaderData={loaderData} />;
}
