import { useEffect, useState } from "react";
import { CopyValue } from "~/components/CopyValue";
import { appExecutionURL } from "~/lib/app-execution-url";

/** Advertises named Unified App routes or exact SDK routes, copying only their address. */
export function AppExecutionEndpoint({ appID, appName }: { appID: string; appName?: string }) {
  const [publicURL, setPublicURL] = useState("");
  useEffect(() => { setPublicURL(window.__FUSED_ENV?.ENGINE_PUBLIC_URL || ""); }, []);
  const endpoint = appExecutionURL(appID, publicURL, appName);
  const isPath = endpoint.startsWith("/");
  return <div className="mt-3 space-y-2"><CopyValue value={endpoint} label={isPath ? "execution path" : "execution URL"} prefix="POST" />
    {/* A missing public origin is explicit rather than silently using the website's host. */}
    {isPath && <p className="text-xs text-slate-500">Use this path with your Fused URL.</p>}
  </div>;
}
