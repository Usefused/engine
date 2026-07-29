import {
  Links,
  Meta,
  Outlet,
  Scripts,
  ScrollRestoration,
  useLoaderData,
  type MetaFunction,
} from "@remix-run/react";
import { useEffect } from "react";
import { track } from "~/lib/analytics";
import type { LinksFunction } from "@remix-run/node";
import "./tailwind.css";
import { ToastProvider } from "~/components/Toast";

export const meta: MetaFunction = () => {
  return [
    { title: "Fused" },
    { name: "description", content: "Fused helps teams manage API integrations at usefused.com — generate typed SDKs, webhook receivers, and MCP servers from any API spec in minutes." },
    { name: "robots", content: "index, follow, max-image-preview:large, max-snippet:-1, max-video-preview:-1" },
    { name: "author", content: "Fused" },
    { name: "theme-color", content: "#ffffff" },
  ];
};

export const links: LinksFunction = () => [
  { rel: "icon", href: "/favicon.svg", type: "image/svg+xml" },
];

import { getApiKey } from "~/lib/session";

export async function clientLoader() {
  const token = getApiKey();
  const runtimeEnv =
    typeof window !== "undefined" ? (window as any).__FUSED_ENV || {} : {};
  return {
    isAuth: !!token,
    apiToken: token,
    ENV: {
      BACKEND_URL: runtimeEnv.BACKEND_URL ?? "", // Relative when embedded in the Go backend
      API_KEY: token || "",
      WEBHOOK_BASE_URL: "https://run.usefused.com",
      SUPPORT_EMAIL: "hello@usefused.com",
      AGENT_MAX_IMPORT_SELECTIONS: "20",
      GA_MEASUREMENT_ID: "G-2LM4S365BM",
    },
  };
}

export function HydrateFallback() {
  return (
    <html lang="en" className="h-full bg-slate-50">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <Meta />
        <Links />
      </head>
      <body className="h-full flex items-center justify-center">
        <div className="text-slate-500">Loading...</div>
        <script src="/env.js" suppressHydrationWarning />
        <Scripts />
      </body>
    </html>
  );
}

export default function App() {
  const data = useLoaderData<typeof clientLoader>();
  const gaId = data.ENV.GA_MEASUREMENT_ID;

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

      (window as any).dataLayer = (window as any).dataLayer || [];
      (window as any).gtag = function (...args: any[]) {
        (window as any).dataLayer.push(args);
      };
      (window as any).gtag("js", new Date());
      (window as any).gtag("config", gaId);
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
    <html lang="en" className="h-full bg-slate-50">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <Meta />
        <Links />
      </head>
      <body className="h-full">
        <ToastProvider>
          <Outlet />
        </ToastProvider>
        <ScrollRestoration />
        <script
          dangerouslySetInnerHTML={{
            __html: `window.ENV = ${JSON.stringify(data.ENV)}`,
          }}
        />
        <Scripts />
      </body>
    </html>
  );
}

export function ErrorBoundary() {
  return (
    <html lang="en" className="h-full bg-slate-50">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <Meta />
        <Links />
      </head>
      <body className="h-full flex items-center justify-center">
        <div className="text-center">
          <h1 className="text-2xl font-semibold text-slate-800 mb-2">Something went wrong</h1>
          <a href="/" className="text-blue-600 underline">Go home</a>
        </div>
        <Scripts />
      </body>
    </html>
  );
}
