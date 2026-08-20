export interface GraphQLResponseError {
  message: string;
}

export interface GraphQLResponse<T> {
  data: T;
  errors?: GraphQLResponseError[];
}

// unwrapGraphQLResponse returns successful data and preserves every server
// validation failure when a document has more than one incompatible field.
export function unwrapGraphQLResponse<T>(response: GraphQLResponse<T>): T {
  const messages = response.errors
    ?.map((error) => error.message.trim())
    .filter(Boolean);
  if (messages && messages.length > 0) {
    // Joining all messages avoids iterative one-field-at-a-time diagnosis when
    // a deployed UI and Registry schema have drifted together.
    throw new Error(messages.join("\n"));
  }
  return response.data;
}
