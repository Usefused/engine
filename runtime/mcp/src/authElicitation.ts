import { UrlElicitationRequiredError } from "@modelcontextprotocol/sdk/types.js";
import type { ExecuteAuthAction } from "./sandbox.js";

/** Builds the standard URL-elicitation error with namespaced replay metadata for capable hosts. */
export function mcpAuthElicitationError(action: ExecuteAuthAction): UrlElicitationRequiredError {
  // Reconnect and first-time connection use distinct copy while sharing the same browser contract.
  const message = action.action === "reconnect" ? "Reconnect your provider account to continue." : "Connect your provider account to continue.";
  return new UrlElicitationRequiredError([{
    mode: "url", message, elicitationId: action.elicitationId, url: action.url,
    _meta: { "com.usefused/auth": { schema_version: 1, action: action.action, expires_at: action.expiresAt, ...action.recovery } },
  }], message);
}
