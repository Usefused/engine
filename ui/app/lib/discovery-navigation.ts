const MAX_DISCOVERY_SESSION_BYTES = 128;
const FORBIDDEN_DISCOVERY_SESSION_CHARACTERS = /[ /\\\t\r\n\0]/;

export interface DiscoveryNavigation {
  sessionID: string | null;
  cliHandoff: boolean;
}

// singleQueryValue rejects duplicated query keys so navigation never depends on parser ordering.
function singleQueryValue(search: URLSearchParams, key: string): string | null {
  const values = search.getAll(key);
  return values.length === 1 ? values[0] : null;
}

// validOpaqueDiscoverySession mirrors the CLI's bounded opaque identity contract without adding UUID semantics.
export function validOpaqueDiscoverySession(value: string | null): value is string {
  if (!value) return false;
  // The CLI owns opaque identity syntax; the UI checks only its byte and delimiter boundary.
  return value.trim() === value
    && new TextEncoder().encode(value).byteLength <= MAX_DISCOVERY_SESSION_BYTES
    && !FORBIDDEN_DISCOVERY_SESSION_CHARACTERS.test(value);
}

// discoveryNavigationFromQuery derives one fail-closed wizard handoff from untrusted URL parameters.
export function discoveryNavigationFromQuery(search: URLSearchParams): DiscoveryNavigation {
  const sessionID = singleQueryValue(search, "session");
  const handoffValues = search.getAll("handoff");
  const handoffIsAbsent = handoffValues.length === 0;
  const cliHandoff = handoffValues.length === 1 && handoffValues[0] === "cli";
  // An unknown or duplicated handoff must not degrade a CLI review into a UI session with apply authority.
  if (!validOpaqueDiscoverySession(sessionID) || (!handoffIsAbsent && !cliHandoff)) {
    return { sessionID: null, cliHandoff: false };
  }
  return { sessionID, cliHandoff };
}

// openDiscoverySessionQuery writes one validated session and prevents CLI review-only state leaking into UI resumes.
export function openDiscoverySessionQuery(current: URLSearchParams, sessionID: string): URLSearchParams {
  const next = new URLSearchParams(current);
  if (!validOpaqueDiscoverySession(sessionID)) {
    next.delete("session");
    next.delete("handoff");
    return next;
  }
  next.set("session", sessionID);
  next.delete("handoff");
  return next;
}

// closeDiscoverySessionQuery clears both identity and origin so a later UI session cannot inherit CLI mode.
export function closeDiscoverySessionQuery(current: URLSearchParams): URLSearchParams {
  const next = new URLSearchParams(current);
  next.delete("session");
  next.delete("handoff");
  return next;
}
