import { useEffect, useState } from "react";
import { Outlet, useNavigate, useLocation, useRouteLoaderData } from "@remix-run/react";
import { NotificationBell } from "~/components/notifications/NotificationBell";
import { api } from "~/lib/api";
import { IntegrationsSidebar } from "~/components/layout/IntegrationsSidebar";
import { CurrentActorAccessProvider } from "~/components/access/CurrentActorAccess";
import { loginPathForLocation } from "~/lib/safe-navigation";

// IntegrationsLayout protects private integration routes while retaining their complete post-login destination.
export default function IntegrationsLayout() {
  const navigate = useNavigate();
  const location = useLocation();
  const rootData = useRouteLoaderData<{ isAuth: boolean }>("root");
  const isAuth = rootData?.isAuth ?? false;
  const [signOutError, setSignOutError] = useState("");

  useEffect(() => {
    const isAuthenticatedStaticRoute = location.pathname.startsWith("/integrations/access/") ||
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
      <div className="min-h-screen flex flex-col lg:flex-row bg-[var(--brand-paper)]">
        <IntegrationsSidebar isAuth={isAuth} handleSignOut={handleSignOut} />

        {/* Main content */}
        <main className="flex-1 min-w-0 overflow-y-auto">
          {signOutError && <p role="alert" className="m-4 rounded-md bg-red-50 p-3 text-sm text-red-700">{signOutError}</p>}
          <div className="max-w-6xl mx-auto w-full px-4 py-5 sm:px-6 sm:py-8 lg:px-8">
            {isAuth && (
              <div className="flex justify-end mb-3">
                <NotificationBell />
              </div>
            )}
            <Outlet />
          </div>
        </main>
      </div>
    </CurrentActorAccessProvider>
  );
}
