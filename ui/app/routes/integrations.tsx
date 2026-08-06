import { useEffect, useState } from "react";
import { Outlet, useNavigate, useLocation, useRouteLoaderData } from "@remix-run/react";
import { NotificationBell } from "~/components/notifications/NotificationBell";
import { api } from "~/lib/api";
import { IntegrationsSidebar } from "~/components/layout/IntegrationsSidebar";
import { CurrentActorAccessProvider } from "~/components/access/CurrentActorAccess";

export default function IntegrationsLayout() {
  const navigate = useNavigate();
  const location = useLocation();
  const rootData = useRouteLoaderData<{ isAuth: boolean }>("root");
  const isAuth = rootData?.isAuth ?? false;
  const [signOutError, setSignOutError] = useState("");

  useEffect(() => {
    const isAuthenticatedStaticRoute = location.pathname.startsWith("/integrations/access/") || [
      "/integrations/buckets",
	  "/integrations/activity",
      "/integrations/mcp",
      "/integrations/sdks",
      "/integrations/settings",
    ].includes(location.pathname);
    const isPublicRoute =
      // Single-segment provider detail pages are public (e.g. /integrations/stripe)
      (!isAuthenticatedStaticRoute && /^\/integrations\/[a-zA-Z0-9_-]+$/.test(location.pathname)) ||
      // Two-segment provider/slug pages are public (e.g. /integrations/stripe/charges)
      // but not /integrations/sdks/:id which needs auth
      /^\/integrations\/(?!sdks\/)[a-zA-Z0-9_-]+\/[a-zA-Z0-9_-]+$/.test(location.pathname);
    // /integrations (index) and all authenticated static routes require login.
    if (!isAuth && !isPublicRoute) {
      navigate("/login", { replace: true });
    }
  }, [navigate, location.pathname, isAuth]);

	async function handleSignOut() {
		setSignOutError("");
		try {
			await api.auth.logout();
			window.location.replace("/login");
		} catch (error) {
			try {
				await api.auth.session();
				await api.auth.logout();
				window.location.replace("/login");
			} catch (retryError) {
				console.error("Failed to sign out", error, retryError);
				setSignOutError("Sign out could not be completed. Refresh the page and try again.");
			}
		}
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
