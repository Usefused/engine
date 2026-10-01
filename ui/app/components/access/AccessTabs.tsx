import { SectionTabs } from "~/components/layout/SectionTabs";

const ACCESS_TABS = [
  { label: "People", to: "/integrations/access/people" },
  { label: "Teams", to: "/integrations/access/teams" },
  { label: "OAuth Clients", to: "/integrations/access/oauth-clients" },
  { label: "Connected Apps", to: "/integrations/access/connected-apps" },
];

/** Keeps all Access destinations consistent and visible without wrapped tab labels on phones. */
export function AccessTabs() {
  return <SectionTabs tabs={ACCESS_TABS} mobileLayout="grid" label="Access navigation" />;
}
