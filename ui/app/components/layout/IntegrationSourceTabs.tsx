import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { hasWorkspacePermission } from "~/lib/current-actor-access";
import { SectionTabs } from "~/components/layout/SectionTabs";

/** Group integration sources in the outer segmented navigation without widening Bridge visibility. */
export function IntegrationSourceTabs() {
  const { access } = useCurrentActorAccess();
  // Local installations remain discoverable only to workspace Bridge readers.
  const bridgeTabs = hasWorkspacePermission(access, "bridge.read")
    ? [{ label: "Bridges", to: "/integrations/bridges" }]
    : [];
  return <SectionTabs label="Integration sources" variant="segmented" tabs={[
    {
      label: "Services",
      to: "/integrations",
      // The parent URL must not select Services while the Bridge catalogue is open.
      isActive: (pathname) => pathname === "/integrations",
    },
    ...bridgeTabs,
  ]} />;
}
