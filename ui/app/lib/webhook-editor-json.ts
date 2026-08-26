const precisionMessage = "This browser cannot preserve JSON numbers exactly. Update your browser or use OpenAPI import outside the builder to keep their full precision.";
export const webhookEditorMaxBytes = 4 * 1024 * 1024;

// Source and optional field inputs share one byte budget before trimming or decoding can allocate copies.
export function assertWebhookEditorJSONSize(source: string): void {
  // A bounded source is required even when it contains only whitespace or an unsupported JSON construct.
  if (new TextEncoder().encode(source).length > webhookEditorMaxBytes) throw new Error("The webhook draft exceeds the editor's 4 MiB limit. Use the existing OpenAPI import workflow.");
}

// Native raw JSON keeps schema/example numeric tokens opaque without adding another JSON parser.
export function parseWebhookEditorJSON(source: string): unknown {
  assertWebhookEditorJSONSize(source);
  const rawJSON = (JSON as unknown as { rawJSON?: (text: string) => unknown }).rawJSON;
  return JSON.parse(source, (_key: string, value: unknown, context?: { source?: string }) => {
    // Objects, strings and booleans are already lossless in the browser's JSON representation.
    if (typeof value !== "number") return value;
    // Older browsers without source-context support cannot prove numeric fidelity and must fail closed.
    if (!context?.source || !rawJSON) throw new Error(precisionMessage);
    // Native stringify emits the exact original token, including unsafe integers, fractions and huge exponents.
    return rawJSON(context.source);
  });
}
