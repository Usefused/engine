import { SectionTabs } from "~/components/layout/SectionTabs";

const ACCESS_TABS = [
  { label: "People", to: "/integrations/access/people" },
  { label: "Teams", to: "/integrations/access/teams" },
  { label: "OAuth Clients", to: "/integrations/access/oauth-clients" },
  { label: "Connected Apps", to: "/integrations/access/connected-apps" },
];

/** Keeps Access destinations on the same scrollable navigation strip as other sections. */
export function AccessTabs() {
  return <SectionTabs tabs={ACCESS_TABS} label="Access navigation" />;
}
