import { useEffect, useState } from "react";
import { CopyValue } from "~/components/CopyValue";
import { appExecutionURL } from "~/lib/app-execution-url";

/** Exposes the exact Engine route, copying its address without the HTTP method label. */
export function AppExecutionEndpoint({ appID }: { appID: string }) {
  const [publicURL, setPublicURL] = useState("");
  useEffect(() => { setPublicURL(window.__FUSED_ENV?.ENGINE_PUBLIC_URL || ""); }, []);
  const endpoint = appExecutionURL(appID, publicURL);
  const isPath = endpoint.startsWith("/");
  return <div className="mt-3 space-y-2"><CopyValue value={endpoint} label={isPath ? "execution path" : "execution URL"} prefix="POST" />
    {/* A missing public origin is explicit rather than silently using the website's host. */}
    {isPath && <p className="text-xs text-slate-500">Use this path with your Fused URL.</p>}
  </div>;
}
