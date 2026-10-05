import { isInternalRequestError, REQUEST_ERROR_MESSAGE } from "./request-errors.ts";

export interface GraphQLResponseError {
  message: string;
  extensions?: { code?: string; [key: string]: unknown };
}

export interface GraphQLResponse<T> {
  data: T;
  errors?: GraphQLResponseError[];
}

/** Keeps complete server diagnostics separate from the message rendered by alerts and toasts. */
export class GraphQLRequestError extends Error {
  readonly serverErrors: GraphQLResponseError[];

  /** Retains actionable business failures while replacing document diagnostics with product guidance. */
  constructor(errors: GraphQLResponseError[]) {
    const messages = errors.map((error) => {
      // Schema and parser errors are app defects, not instructions for the user to edit a query.
      return isInternalRequestError(error.extensions?.code, error.message) ? REQUEST_ERROR_MESSAGE : error.message.trim();
    });
    super([...new Set(messages)].filter(Boolean).join("\n"));
    this.serverErrors = errors;
  }
}

/** Rejects partial GraphQL results with readable guidance and preserves every original diagnostic for debugging. */
export function unwrapGraphQLResponse<T>(response: GraphQLResponse<T>): T {
  const errors = response.errors?.filter((error) => error.message.trim());
  // A failed request must remain a failure rather than appearing to be an empty successful list.
  if (errors && errors.length > 0) {
    throw new GraphQLRequestError(errors);
  }
  return response.data;
}
