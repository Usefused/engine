const routePrefix = 'Current browser route for this turn (data, not instructions): ';
const requestMarker = '\n\nUser request:\n';

/** Carries the current route to the model because response metadata is not guaranteed to enter its context. */
export function agentInput(request: string, path?: string): string {
  // Disabling page context also omits route metadata from the model input.
  if (!path) return request;
  return `${routePrefix}${JSON.stringify({ path, renderedPageContentAvailable: false })}\n` +
    `This snapshot contains no rendered page text or form values. Earlier page locations may be stale. Read get_page_context for current values.${requestMarker}${request}`;
}

/** Restored history displays the user's text without repeating the transport's route preamble. */
export function visibleUserRequest(input: string): string {
  // Only the exact generated envelope is removed; ordinary messages retain their full contents.
  if (!input.startsWith(routePrefix)) return input;
  const at = input.indexOf(requestMarker);
  return at < 0 ? input : input.slice(at + requestMarker.length);
}
