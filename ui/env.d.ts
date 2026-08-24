/// <reference types="vite/client" />

import "react";

declare module "react" {
  interface HTMLAttributes<T> extends AriaAttributes, DOMAttributes<T> {
    toolname?: string;
    tooldescription?: string;
    toolparam?: string;
    toolparamdescription?: string;
  }
}

declare global {
  interface Window {
    // Injected by the Go backend before hydration (see root.tsx clientLoader).
    __FUSED_ENV?: {
      BACKEND_URL?: string;
      ENGINE_PUBLIC_URL?: string;
      ENGINE_PUBLIC_GRPC_URL?: string;
      [key: string]: unknown;
    };
    // Re-exposed subset of __FUSED_ENV plus loader-derived values, read by
    // client components that need it outside the route loader tree.
    ENV?: {
      BACKEND_URL: string;
      API_KEY: string;
      WEBHOOK_BASE_URL: string;
      SUPPORT_EMAIL: string;
      GA_MEASUREMENT_ID: string;
    };
    // Standard gtag.js globals.
    dataLayer?: unknown[];
    gtag?: (...args: unknown[]) => void;
  }
}
