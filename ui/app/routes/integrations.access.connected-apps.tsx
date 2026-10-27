import { Navigate, useLocation } from "@remix-run/react";

/** Preserve bookmarked links while keeping the new navigation location canonical. */
export default function LegacyAccessRedirect() {
  const { search, hash } = useLocation();
  return <Navigate to={"/integrations/access/authorized-apps" + search + hash} replace />;
}
