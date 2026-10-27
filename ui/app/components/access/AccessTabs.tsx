import { SectionTabs } from "~/components/layout/SectionTabs";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";

/** Keep personal authorizations available without exposing workspace administration tabs. */
export function AccessTabs() {
  const { access } = useCurrentActorAccess();
  // People and teams require workspace authority; personal consent management does not.
  const adminTabs = hasWorkspacePermission(access, "access.read") ? [
    { label: "People", to: "/integrations/access/people" },
    { label: "Teams", to: "/integrations/access/teams" },
  ] : [];
  return <SectionTabs tabs={[...adminTabs, { label: "Authorized Apps", to: "/integrations/access/authorized-apps" }]} label="Access navigation" />;
}
