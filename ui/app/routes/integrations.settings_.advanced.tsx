import { Link, type MetaFunction } from "@remix-run/react";
import { KeyRound } from "lucide-react";
import { WorkspacePermissionGate } from "~/components/access/CurrentActorAccess";
import { SettingsLayout } from "~/components/settings/SettingsLayout";

/** Label the administrative settings destination in browser navigation. */
export const meta: MetaFunction = () => [{ title: "Advanced settings - Fused" }];

/** Keep advanced administration in the shared settings shell without widening access. */
export default function AdvancedSettingsPage() {
  return <WorkspacePermissionGate permission="access.read" area="advanced access settings">
    <SettingsLayout activeSection="advanced">
      <Link to="/integrations/settings/oauth-clients" className="flex items-start gap-3 rounded-xl border border-slate-200 bg-white p-5 shadow-sm hover:border-slate-300 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500">
        <KeyRound className="mt-0.5 h-5 w-5 shrink-0 text-slate-500" aria-hidden="true" />
        <span><span className="block font-semibold text-slate-900">OAuth Clients</span><span className="mt-1 block text-sm leading-6 text-slate-500">Register applications allowed to request access through Fused OAuth. Each user reviews their grants in Authorized Apps.</span></span>
      </Link>
    </SettingsLayout>
  </WorkspacePermissionGate>;
}
