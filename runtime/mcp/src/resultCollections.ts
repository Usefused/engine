import { appendPointer, MAX_JSON_POINTER_BYTES } from "./jsonPointer.js";

/** Describes observed immediate row keys, not a provider schema or scalar sample. */
export interface ResultCollection {
  path: string;
  count: number;
  fields: string[];
  fields_complete: boolean;
}

/** Both traversal and field discovery disclose when their bounded inspection omitted information. */
export interface CollectionDiscovery {
  collections: ResultCollection[];
  collections_complete: boolean;
}

const MAX_COLLECTIONS = 8;
const MAX_NODES = 256;
const MAX_DEPTH = 8;
const MAX_CHILDREN = 32;
const MAX_ROWS = 512;
export const MAX_RESULT_FIELDS = 32;
export const MAX_RESULT_FIELD_BYTES = 128;

/** Shares the same record distinction between discovery and field projection, including JSON null. */
export function isResultRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

/** Finds nested arrays breadth-first so shallow useful collections precede row-specific child arrays. */
export function discoverCollections(root: unknown): CollectionDiscovery {
  const result: CollectionDiscovery = { collections: [], collections_complete: true };
  const queue = [{ value: root, path: "", depth: 0 }];
  for (let index = 0; index < queue.length; index++) {
    const node = queue[index];
    // Arrays can be paged even when empty or composed of primitive rows.
    if (Array.isArray(node.value)) {
      // Stop instead of publishing an apparently exhaustive catalogue after its admission cap.
      if (result.collections.length === MAX_COLLECTIONS) {
        result.collections_complete = false;
        break;
      }
      result.collections.push(describeCollection(node.value, node.path));
    }
    enqueueChildren(node, queue, result);
  }
  return result;
}

/** Bounds traversal memory and path growth without following prototypes or executing provider data. */
function enqueueChildren(node: { value: unknown; path: string; depth: number }, queue: typeof node[], result: CollectionDiscovery): void {
  // Scalars cannot contain another collection and need no traversal budget.
  if (node.value === null || typeof node.value !== "object") return;
  const keys = Object.keys(node.value);
  // Report skipped descendants rather than guessing that their shape resembles the inspected prefix.
  if (node.depth >= MAX_DEPTH || queue.length >= MAX_NODES) {
    result.collections_complete &&= keys.length === 0;
    return;
  }
  // A truncated sibling list may hide another array even when the inspected children contain none.
  result.collections_complete &&= keys.length <= MAX_CHILDREN;
  for (const key of keys.slice(0, MAX_CHILDREN)) {
    const path = appendPointer(node.path, key);
    // Advertised pointers must remain valid for the shared bounded pointer resolver.
    if (Buffer.byteLength(path) > MAX_JSON_POINTER_BYTES || queue.length >= MAX_NODES) {
      result.collections_complete = false;
      continue;
    }
    queue.push({ value: (node.value as Record<string, unknown>)[key], path, depth: node.depth + 1 });
  }
}

/** Unions sparse row keys across a bounded prefix, declaring completeness only after inspecting every row. */
function describeCollection(rows: unknown[], path: string): ResultCollection {
  const fields = new Set<string>();
  const collection: ResultCollection = { path, count: rows.length, fields: [], fields_complete: rows.length <= MAX_ROWS };
  for (const row of rows.slice(0, MAX_ROWS)) {
    // Primitive and array rows have no selectable immediate object fields; whole-row paging remains available.
    if (!isResultRecord(row)) continue;
    const keys = Object.keys(row);
    // A wide row may contain unique keys beyond the inspected prefix.
    collection.fields_complete &&= keys.length <= MAX_RESULT_FIELDS;
    for (const key of keys.slice(0, MAX_RESULT_FIELDS)) {
      // Never advertise shortened keys; the exact authored name is required for projection.
      if (Buffer.byteLength(key) > MAX_RESULT_FIELD_BYTES || fields.size >= MAX_RESULT_FIELDS && !fields.has(key)) {
        collection.fields_complete = false;
        continue;
      }
      fields.add(key);
    }
  }
  collection.fields = [...fields];
  return collection;
}
