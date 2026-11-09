export const operationMethods = ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE", "CONNECT", "GRAPHQL"];

/** Cached catalogue pages can still contain operations retired by the Registry. */
export function isVisibleOperationMethod(method?: string) {
  return method?.toUpperCase() !== "SOAP";
}

/** Applies the same visibility rules to search results and resource pages. */
export function filterOperationMethods<T extends { method?: string }>(operations: T[], method: string): T[] {
  // An unrestricted view may contain legacy rows from before the Registry upgrade.
  return operations.filter((operation) => isVisibleOperationMethod(operation.method) &&
    (method === "all" || operation.method?.toUpperCase() === method));
}
