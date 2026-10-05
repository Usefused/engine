export const REQUEST_ERROR_MESSAGE = "Fused couldn’t complete this action. Refresh the page and try again. If it continues, contact your workspace administrator.";

const documentErrorCodes = new Set([
  "invalid_graphql_request", "invalid_graphql_request_body", "invalid_graphql_operation",
  "GRAPHQL_PARSE_FAILED", "GRAPHQL_VALIDATION_FAILED",
]);

/** Recognizes document failures owned by the app without hiding ordinary business or permission errors. */
export function isInternalRequestError(code?: string, message = ""): boolean {
  // Stable server codes take precedence over wording that may change between releases.
  if (code && documentErrorCodes.has(code)) return true;
  // Legacy GraphQL responses expose only validator text, so match its specific diagnostic forms.
  return /^(?:The GraphQL (?:request|operation)\b|Correct the GraphQL document\b|Cannot query field\b|Unknown (?:argument|type|directive|fragment|operation)\b|Syntax Error\b|Variable "\$[^"\n]+" (?:got invalid value|of required type|of type|is not defined|is never used)\b|Field "[^"\n]+" (?:argument|must not have a selection|of type|is not defined)\b)/i.test(message.trim());
}
