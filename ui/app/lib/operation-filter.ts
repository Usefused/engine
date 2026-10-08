export const operationMethods = ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE", "CONNECT", "GRAPHQL", "SOAP"];

/** Applies an exact method filter without changing operation ordering or schema identity. */
export function filterOperationMethods<T extends { method?: string }>(operations: T[], method: string): T[] {
  // The unrestricted view preserves the original collection and all provider-specific methods.
  if (method === "all") return operations;
  return operations.filter((operation) => operation.method?.toUpperCase() === method);
}
