import { Navigate, useSearchParams, type MetaFunction } from "@remix-run/react";

// meta keeps legacy catalogue URLs titled as the shared Apps destination.
export const meta: MetaFunction = ({ matches }) => {
  const parentMeta = matches.filter((match) => match.id === "root").flatMap((match) => match.meta ?? []);
  return [...parentMeta.filter((item) => !("title" in item)), { title: "Apps - Fused" }];
};

/** Preserves legacy MCP catalogue links by forwarding them into the shared Apps view. */
export default function McpServersRedirect() {
  const [searchParams] = useSearchParams();
  const next = new URLSearchParams(searchParams);
  // The unified catalogue no longer keeps adapter selection in its URL.
  next.delete("tab");
  const suffix = next.toString();
  // Preserve useful search and archive state while avoiding a trailing question mark.
  return <Navigate to={`/integrations/sdks${suffix ? `?${suffix}` : ""}`} replace />;
}
