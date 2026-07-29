import { useEffect } from "react";
import { Outlet, useNavigate, useLocation, useRouteLoaderData } from "@remix-run/react";
import { CreditBanner } from "~/components/CreditBanner";
import { FloatingCredits } from "~/components/FloatingCredits";
import { NotificationBell } from "~/components/notifications/NotificationBell";
import { clearApiKey } from "~/lib/session";
import { IntegrationsSidebar } from "~/components/layout/IntegrationsSidebar";

export default function IntegrationsLayout() {
  const navigate = useNavigate();
  const location = useLocation();
  const rootData = useRouteLoaderData<{ isAuth: boolean }>("root");
  const isAuth = rootData?.isAuth ?? false;

  useEffect(() => {
    const isAuthenticatedStaticRoute = [
      "/integrations/buckets",
      "/integrations/credits",
      "/integrations/mcp",
      "/integrations/notifications",
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

  function handleSignOut() {
    clearApiKey();
    navigate("/login", { replace: true });
  }

  return (
    <div className="min-h-screen flex flex-col md:flex-row bg-slate-50">
      <IntegrationsSidebar isAuth={isAuth} handleSignOut={handleSignOut} />

      {/* Main content */}
      <main className="flex-1 min-w-0 overflow-y-auto">
        <CreditBanner />
        <div className="max-w-6xl mx-auto px-4 md:px-8 py-8 w-full">
          <Outlet />
        </div>
      </main>
      {/* {isAuth && <FloatingCredits />} */}
      {isAuth && <NotificationBell />}
    </div>
  );
}
