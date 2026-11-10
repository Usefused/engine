import { createRoot } from "react-dom/client";
import { createBrowserRouter, RouterProvider, Outlet } from "react-router-dom";
import { CurrentActorAccessProvider } from "../../app/components/access/CurrentActorAccess";
import WebhookDetails from "../../app/routes/integrations.webhooks.$serviceId.$slug";
import Webhooks from "../../app/routes/integrations.webhooks._index";

/** Keeps production route components and access hydration intact while all API data remains synthetic. */
function FixtureLayout() {
  return <CurrentActorAccessProvider isAuth><main className="mx-auto min-w-0 max-w-6xl space-y-6 p-4 sm:p-8"><Outlet /></main></CurrentActorAccessProvider>;
}

const router = createBrowserRouter([{ element: <FixtureLayout />, children: [
  { path: "/integrations/webhooks", element: <Webhooks /> },
  { path: "/integrations/webhooks/:serviceId/:slug", element: <WebhookDetails /> },
] }]);
createRoot(document.getElementById("root")!).render(<RouterProvider router={router} />);
