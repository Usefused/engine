import type { CurrentActorAccess } from "./current-actor-permissions";
import { hasWorkspacePermission } from "./current-actor-permissions.ts";
import { assertWebhookEditorJSONSize, parseWebhookEditorJSON, webhookEditorMaxBytes } from "./webhook-editor-json.ts";
export { webhookEditorMaxBytes } from "./webhook-editor-json.ts";

export type WebhookDocument = Record<string, unknown>;
// Decode every OpenAPI 3.1 Path Item method so imports remain lossless even when Engine ingress cannot execute a verb.
export const webhookMethods = ["post", "get", "put", "patch", "delete", "head", "options", "trace"];
export interface WebhookDraftEvent {
  id: string;
  name: string;
  method: string;
  originalMethod: string;
  description: string;
  originalDescription: string;
  schemaText?: string;
  exampleText?: string;
  operation: WebhookDocument;
  pathItem: WebhookDocument;
}
export interface WebhookEditorDraft {
  document: WebhookDocument;
  events: WebhookDraftEvent[];
}

// Ownership and an explicit import grant are independent requirements; route shape grants nothing.
export function canEditWebhook(owner: unknown, access: CurrentActorAccess | null): boolean {
  return owner === true && hasWorkspacePermission(access, "catalogue.import");
}

// JSON object checks protect the thin adapter without interpreting OpenAPI semantics in React.
export function webhookRecord(value: unknown): WebhookDocument {
  // A missing optional object is represented as empty; server validation still owns its semantics.
  if (value === undefined) return {};
  // Arrays and primitives cannot safely carry preserved object properties.
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("This contract cannot be edited safely: an object was expected.");
  return value as WebhookDocument;
}

// Preserve unsupported contract metadata rather than reconstructing from the read-only catalogue DTO.
export function readWebhookDraft(source: string): WebhookEditorDraft {
  const document = webhookRecord(parseWebhookEditorJSON(source));
  const webhooks = webhookRecord(document.webhooks);
  const events = Object.entries(webhooks).flatMap(([name, value]) => readPathEvents(name, webhookRecord(value)));
  return { document, events };
}

// Retain the original method for intentional undo; referenced path items cannot be safely rebuilt locally.
function readPathEvents(name: string, pathItem: WebhookDocument): WebhookDraftEvent[] {
  // Fail closed instead of replacing a reference with an incomplete inferred operation.
  if (pathItem.$ref) throw new Error("Referenced webhook path items require OpenAPI editing; this editor will not flatten them.");
  return webhookMethods.filter((method) => pathItem[method] !== undefined).map((method) => {
    const operation = webhookRecord(pathItem[method]);
    // Reference siblings have version-sensitive semantics and must not be overwritten by a partial form.
    if (operation.$ref) throw new Error("Referenced webhook operations require OpenAPI editing.");
    const description = String(operation.summary ?? operation.description ?? "");
    return { id: `${name}:${method}`, name, method, originalMethod: method, description, originalDescription: description, operation, pathItem };
  });
}

// New events start with POST without guessed payload fields; the original method supports undo within the draft.
export function newWebhookEvent(): WebhookDraftEvent {
  return { id: crypto.randomUUID(), name: "", method: "post", originalMethod: "post", description: "", originalDescription: "", operation: { responses: { "200": { description: "Webhook accepted" } } }, pathItem: {} };
}

// Rebuild only the event map while retaining schemas, references, security, and all unrelated metadata.
export function serializeWebhookDraft(draft: WebhookEditorDraft): string {
  const webhooks: WebhookDocument = Object.create(null);
  const identities = new Set<string>();
  for (const event of draft.events) {
    // Empty identifiers are invalid authoring state, not a signal to silently remove an event.
    if (!event.name.trim()) throw new Error("Every event needs a name.");
    const identity = `${event.name}\u0000${event.method}`;
    // Canonical event identity includes the method; duplicate map keys would otherwise overwrite silently.
    if (identities.has(identity)) throw new Error(`Duplicate event: ${event.method.toUpperCase()} ${event.name}`);
    identities.add(identity);
    const item = webhookRecord(webhooks[event.name]);
    // Combining renamed events with distinct path metadata would silently lose one source's contract.
    if (webhooks[event.name] && JSON.stringify(pathMetadata(item)) !== JSON.stringify(pathMetadata(event.pathItem))) throw new Error("Events with different shared path metadata cannot be combined under one name in the builder.");
    webhooks[event.name] = { ...pathMetadata(event.pathItem), ...item, [event.method]: eventOperation(event) };
  }
  const source = JSON.stringify({ ...draft.document, webhooks });
  // Apply the same byte limit to authored and imported source before transport.
  if (new TextEncoder().encode(source).length > webhookEditorMaxBytes) throw new Error("The webhook draft exceeds the editor's 4 MiB limit.");
  return source;
}

// Non-operation path metadata travels with an edited event without copying sibling methods.
function pathMetadata(item: WebhookDocument): WebhookDocument {
  return Object.fromEntries(Object.entries(item).filter(([key]) => !webhookMethods.includes(key)));
}

// An untouched optional description must stay absent rather than causing a spurious contract change.
function eventOperation(event: WebhookDraftEvent): WebhookDocument {
  let operation = event.operation;
  // Preserve exact original JSON unless the user changed this explicit field.
  if (event.description !== event.originalDescription) operation = { ...operation, summary: event.description };
  return applyPayloadEdits(operation, event);
}

// Optional payload edits affect only JSON representation fields explicitly touched by the owner.
function applyPayloadEdits(operation: WebhookDocument, event: WebhookDraftEvent): WebhookDocument {
  // Untouched representations and shared references must remain exactly as supplied.
  if (event.schemaText === undefined && event.exampleText === undefined) return operation;
  const body = webhookRecord(operation.requestBody);
  // Shared request-body references cannot safely accept local sibling overrides.
  if (body.$ref) throw new Error("Edit the referenced request body in OpenAPI instead of replacing it here.");
  const content = webhookRecord(body.content);
  const json = { ...webhookRecord(content["application/json"]) };
  setOptionalJSON(json, "schema", event.schemaText);
  setOptionalJSON(json, "example", event.exampleText);
  return { ...operation, requestBody: { ...body, content: { ...content, "application/json": json } } };
}

// Empty explicit input removes one optional field; unchanged input does not touch it.
function setOptionalJSON(target: WebhookDocument, key: string, text: string | undefined): void {
  // Undefined means that the form never modified this field.
  if (text === undefined) return;
  assertWebhookEditorJSONSize(text);
  // Clearing a field is an explicit edit, not schema inference from the example.
  if (!text.trim()) { delete target[key]; return; }
  target[key] = parseWebhookEditorJSON(text);
}

// Payload controls inspect only the existing JSON representation and never resolve references.
export function webhookPayloadField(event: WebhookDraftEvent, field: "schema" | "example"): string {
  const json = webhookRecord(webhookRecord(webhookRecord(event.operation.requestBody).content)["application/json"]);
  // Absence remains optional and does not invent a generic object contract.
  if (json[field] === undefined) return "";
  return JSON.stringify(json[field], null, 2);
}

// Document settings edits replace only one reviewed extension and keep every other contract intact.
export function updateWebhookSetting(draft: WebhookEditorDraft, key: string, value: unknown): WebhookEditorDraft {
  const document = { ...draft.document };
  // Explicit clearing removes only the selected extension, not events or other policies.
  if (value === undefined) delete document[key];
  else document[key] = value;
  return { ...draft, document };
}

// Show the shared routing grammar as text, including existing composite and fallback forms.
export function webhookDiscriminatorText(document: WebhookDocument): string {
  const value = document["x-fused-event-discriminator"];
  // JSON preserves composite arrays/objects without inventing another routing parser.
  if (typeof value === "object" && value !== null) return JSON.stringify(value);
  return String(value ?? "");
}

// Accept scalar routing paths or explicit JSON composites; Registry remains authoritative on syntax.
export function webhookDiscriminatorValue(value: string): unknown {
  // Absence is intentional and is reviewed by the same server planner.
  if (!value.trim()) return undefined;
  // Composite authoring retains the canonical extension shape rather than flattening it.
  if (value.trim().startsWith("[") || value.trim().startsWith("{")) return parseWebhookEditorJSON(value);
  return value;
}
