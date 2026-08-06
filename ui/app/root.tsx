import {
  Links,
  Outlet,
  Scripts,
  ScrollRestoration,
  useLoaderData,
  useLocation,
} from "@remix-run/react";
import { useEffect } from "react";
import { track } from "~/lib/analytics";
import { routeTitle } from "~/lib/route-title";
import type { LinksFunction } from "@remix-run/node";
import "./tailwind.css";
import { ToastProvider } from "~/components/Toast";

export const links: LinksFunction = () => [
  { rel: "icon", href: "/favicon.svg", type: "image/svg+xml" },
];

import { api } from "~/lib/api";
import { purgeLegacyBrowserCredential } from "~/lib/session";

export async function clientLoader() {
	purgeLegacyBrowserCredential();
	const session = await api.auth.session().catch(() => ({ authenticated: false }));
  const runtimeEnv =
    typeof window !== "undefined" ? window.__FUSED_ENV || {} : {};
  return {
		isAuth: session.authenticated,
		apiToken: "",
    ENV: {
      BACKEND_URL: runtimeEnv.BACKEND_URL ?? "", // Relative when embedded in the Go backend
			API_KEY: "",
      WEBHOOK_BASE_URL: "https://run.usefused.com",
      SUPPORT_EMAIL: "hello@usefused.com",
      AGENT_MAX_IMPORT_SELECTIONS: "20",
      GA_MEASUREMENT_ID: "G-2LM4S365BM",
    },
  };
}

export function HydrateFallback() {
  // Engine's static shell owns the document. Hydrating only #app prevents
  // route-specific head/preload changes from invalidating SPA hydration.
  return (
    <>
      <Links />
      <div className="h-full" />
      <Scripts />
    </>
  );
}

export default function App() {
  const data = useLoaderData<typeof clientLoader>();
  const gaId = data.ENV.GA_MEASUREMENT_ID;
  const location = useLocation();

  useEffect(() => {
    document.title = routeTitle(location.pathname, location.search);
  }, [location.pathname, location.search]);

  useEffect(() => {
    if (!gaId) return;

    let injected = false;
    const injectGtag = () => {
      if (injected) return;
      injected = true;
      const script = document.createElement("script");
      script.async = true;
      script.defer = true;
      script.src = `https://www.googletagmanager.com/gtag/js?id=${gaId}`;
      document.head.appendChild(script);

      window.dataLayer = window.dataLayer || [];
      window.gtag = function (...args: unknown[]) {
        window.dataLayer?.push(args);
      };
      window.gtag("js", new Date());
      window.gtag("config", gaId);
    };

    const interactEvents = ["scroll", "mousemove", "touchstart", "keydown", "click"];
    const handleInteract = () => {
      injectGtag();
      interactEvents.forEach(e => window.removeEventListener(e, handleInteract));
    };

    interactEvents.forEach(e => window.addEventListener(e, handleInteract, { passive: true, once: true }));

    return () => {
      interactEvents.forEach(e => window.removeEventListener(e, handleInteract));
    };
  }, [gaId]);

  useEffect(() => {
    function handleClick(e: MouseEvent) {
      const button = (e.target as Element).closest("button, [role='button']");
      if (!button) return;

      const label =
        button.getAttribute("data-track") ||
        button.getAttribute("aria-label") ||
        button.textContent?.trim() ||
        "unknown";

      track("button_click", { label });
    }

    document.addEventListener("click", handleClick);
    return () => document.removeEventListener("click", handleClick);
  }, []);

  return (
    <>
      <Links />
      <div className="h-full">
        <ToastProvider>
          <Outlet />
        </ToastProvider>
      </div>
      <ScrollRestoration />
      <Scripts />
    </>
  );
}

export function ErrorBoundary() {
  return (
    <>
      <Links />
      <div className="h-full">
        <div className="h-full flex items-center justify-center text-center">
          <div>
            <h1 className="text-2xl font-semibold text-slate-800 mb-2">Something went wrong</h1>
            <a href="/" className="text-[var(--brand-violet)] underline">Go home</a>
          </div>
        </div>
      </div>
      <Scripts />
    </>
  );
}
