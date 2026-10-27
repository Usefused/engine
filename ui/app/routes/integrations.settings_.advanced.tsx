import { Link, type MetaFunction } from "@remix-run/react";
import { KeyRound, Settings } from "lucide-react";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { WorkspacePermissionGate, useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { CataloguePageHeader } from "~/components/layout/CataloguePageHeader";

/** Label the administrative settings destination in browser navigation. */
export const meta: MetaFunction = () => [{ title: "Advanced settings - Fused" }];

/** Keep OAuth registrations discoverable as administration rather than a connection workflow. */
export default function AdvancedSettingsPage() {
  const { access } = useCurrentActorAccess();
  return <WorkspacePermissionGate permission="access.read" area="advanced access settings">
    <div className="min-w-0 space-y-6">
      {/* Access-only administrators must not be sent into account settings they cannot read. */}
      {hasWorkspacePermission(access, "account.read") && <Link to="/integrations/settings" className="inline-flex min-h-11 items-center text-sm font-medium text-slate-600 hover:text-slate-900">← Settings</Link>}
      <CataloguePageHeader title="Advanced settings" icon={Settings} description="Administrative settings for applications requesting access to your workspace." />
      <Link to="/integrations/settings/oauth-clients" className="flex items-start gap-3 rounded-xl border border-slate-200 bg-white p-5 shadow-sm hover:border-slate-300 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500">
        <KeyRound className="mt-0.5 h-5 w-5 shrink-0 text-slate-500" aria-hidden="true" />
        <span><span className="block font-semibold text-slate-900">OAuth Clients</span><span className="mt-1 block text-sm leading-6 text-slate-500">Register applications allowed to request access through Fused OAuth. Each user reviews their grants in Authorized Apps.</span></span>
      </Link>
    </div>
  </WorkspacePermissionGate>;
}
