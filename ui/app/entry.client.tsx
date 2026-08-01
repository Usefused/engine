import { RemixBrowser } from "@remix-run/react";
import { startTransition, StrictMode } from "react";
import { hydrateRoot } from "react-dom/client";

const app = document.querySelector("#app");
if (!app) throw new Error("missing Engine UI application root");

startTransition(() => {
  hydrateRoot(
    app,
    <StrictMode>
      <RemixBrowser />
    </StrictMode>
  );
});
