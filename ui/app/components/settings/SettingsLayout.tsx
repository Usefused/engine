import type { ReactNode } from "react";
import { useSearchParams } from "@remix-run/react";
import { Settings } from "lucide-react";
import { useCurrentActorAccess } from "~/components/access/CurrentActorAccess";
import { CataloguePageHeader } from "~/components/layout/CataloguePageHeader";
import { SectionTabs, type SectionTab } from "~/components/layout/SectionTabs";
import { hasWorkspacePermission } from "~/lib/current-actor-access";

const ACCOUNT_SECTIONS = [
  { id: "general", label: "General" },
  { id: "billing", label: "Billing" },
  { id: "branding", label: "Connection branding" },
  { id: "managed-auth", label: "Managed auth" },
  { id: "api-key", label: "API key" },
];

// Unknown query values retain the default account view rather than hiding every panel.
export function settingsSection(requested: string | null) {
  return ACCOUNT_SECTIONS.find(/** Match only supported account sections. */ (tab) => tab.id === requested)?.id ?? "general";
}

// Share the outer navigation and full-width shell across account settings and advanced administration.
export function SettingsLayout({ activeSection, children }: { activeSection: string; children: ReactNode }) {
  const { access } = useCurrentActorAccess();
  const [searchParams] = useSearchParams();
  const tabs: SectionTab[] = [];
  // Access-only administrators retain Advanced without links to account settings they cannot read.
  if (hasWorkspacePermission(access, "account.read")) {
    // Absolute destinations work from Advanced and preserve unrelated account query parameters.
    for (const { id, label } of ACCOUNT_SECTIONS) {
      const params = new URLSearchParams(searchParams);
      params.set("tab", id);
      tabs.push({ label, to: `/integrations/settings?${params.toString()}`,
        // Account sections share a pathname, so selection follows the resolved query section.
        isActive: () => activeSection === id });
    }
  }
  // Registration administration keeps its existing permission boundary and stable deep link.
  if (hasWorkspacePermission(access, "access.read")) {
    tabs.push({ label: "Advanced", to: "/integrations/settings/advanced",
      // Nested registration pages remain within the Advanced section.
      isActive: () => activeSection === "advanced" });
  }
  return <div className="min-w-0 w-full space-y-6">
    <CataloguePageHeader title="Settings" icon={Settings} description="Manage your account, connections, and API credentials." />
    <SectionTabs label="Settings sections" variant="segmented" tabs={tabs} />
    {children}
  </div>;
}
