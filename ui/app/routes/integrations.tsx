import { FusedAgentProvider } from "~/components/agent/FusedAgentProvider";
import { useEffect, useState } from "react";
import { Outlet, useNavigate, useLocation, useRouteLoaderData } from "@remix-run/react";
import { NotificationBell } from "~/components/notifications/NotificationBell";
import { api } from "~/lib/api";
import { IntegrationsSidebar } from "~/components/layout/IntegrationsSidebar";
import { CurrentActorAccessProvider } from "~/components/access/CurrentActorAccess";
import { loginPathForLocation } from "~/lib/safe-navigation";

// IntegrationsLayout keeps parent navigation and global utilities compact above each page's own title and actions.
export default function IntegrationsLayout() {
  const navigate = useNavigate();
  const location = useLocation();
  const rootData = useRouteLoaderData<{ isAuth: boolean }>("root");
  const isAuth = rootData?.isAuth ?? false;
  const [signOutError, setSignOutError] = useState("");

  useEffect(() => {
    // Hosted app authoring and access pages belong to the authenticated Engine workspace.
    const isAuthenticatedStaticRoute = location.pathname.startsWith("/integrations/unified-apps") || location.pathname.startsWith("/integrations/webhooks") || location.pathname.startsWith("/integrations/access/") ||
      location.pathname.startsWith("/integrations/mcp/") ||
      location.pathname.startsWith("/integrations/sdks/") || [
      "/integrations/buckets",
	  "/integrations/activity",
      "/integrations/mcp",
      "/integrations/sdks",
      "/integrations/settings",
    ].includes(location.pathname);
    const isPublicRoute = !isAuthenticatedStaticRoute && (
      // Single-segment provider detail pages are public (e.g. /integrations/stripe)
      /^\/integrations\/[a-zA-Z0-9_-]+$/.test(location.pathname) ||
      // Two-segment provider/slug pages are public (e.g. /integrations/stripe/charges)
      // App and MCP detail prefixes are classified above before provider URLs.
      /^\/integrations\/[a-zA-Z0-9_-]+\/[a-zA-Z0-9_-]+$/.test(location.pathname)
    );
    // /integrations (index) and all authenticated static routes require login.
    if (!isAuth && !isPublicRoute) {
      navigate(loginPathForLocation(location.pathname, location.search), { replace: true });
    }
  }, [navigate, location.pathname, location.search, isAuth]);

  async function handleSignOut() {
    setSignOutError("");
    try {
      const result = await api.auth.logout();
      completeBrowserLogout(result.logout_url);
    } catch (error) {
      try {
        const session = await api.auth.session();
        // A lost response can arrive after Engine already revoked the local
        // session. In that case there is nothing left to retry.
        if (!session.authenticated) {
          window.location.replace("/login");
          return;
        }
        const result = await api.auth.logout();
        completeBrowserLogout(result.logout_url);
      } catch (retryError) {
        console.error("Failed to sign out", error, retryError);
        setSignOutError("Sign out could not be completed. Refresh the page and try again.");
      }
    }
  }

  function completeBrowserLogout(logoutURL?: string) {
    if (logoutURL) {
      // Provider logout must be a top-level navigation so Logto can clear its
      // own host-only session cookie before returning to this Engine.
      window.location.assign(logoutURL);
      return;
    }
    window.location.replace("/login");
  }

  return (
    <CurrentActorAccessProvider isAuth={isAuth}>
      <FusedAgentProvider authenticated={isAuth}>
      <div data-fused-workspace className="min-h-screen flex flex-col lg:flex-row bg-[var(--brand-paper)]">
        <IntegrationsSidebar isAuth={isAuth} handleSignOut={handleSignOut} />

        {/* Main content */}
        <main className="flex-1 min-w-0 overflow-y-auto">
          {signOutError && <p role="alert" className="m-4 rounded-md bg-red-50 p-3 text-sm text-red-700">{signOutError}</p>}
          <div className="max-w-6xl mx-auto w-full px-4 pt-3 pb-5 sm:px-6 sm:pt-4 sm:pb-8 lg:px-8">
            {/* Back navigation leads the page; global utilities share its row without displacing the title. */}
            <div className="mb-5 flex min-h-8 items-center justify-between gap-4">
              <div id="integrations-back-navigation" className="min-w-0 flex-1 empty:hidden" />
              {/* Workspace utilities remain available on pages without parent navigation. */}
              {isAuth && <div id="integrations-header-actions" className="ml-auto flex items-center justify-end gap-2"><NotificationBell /></div>}
            </div>
            <Outlet />
          </div>
        </main>
        {/* Portaled forms stay outside parent forms while remaining available to workspace page tools. */}
        <div id="fused-workspace-dialogs" className="contents" />
      </div>
    </FusedAgentProvider>
    </CurrentActorAccessProvider>
  );
}
