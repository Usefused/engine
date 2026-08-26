/** Describes data shape without pretending an incomplete sample is the original result. */
export interface ResultPreview {
  type: string;
  complete: false;
  count?: number;
  fields?: Array<{ name: string; preview: ResultPreview }>;
  items?: ResultPreview[];
}

const MAX_PREVIEW_DEPTH = 2;
const MAX_PREVIEW_CHILDREN = 4;
const MAX_PREVIEW_KEY_BYTES = 128;

/** Describes only parsed JSON structure; personal scalar values never enter automatic previews. */
export function previewResult(value: unknown, depth = 0): ResultPreview {
  // String length helps callers choose a slice without revealing any message or file content.
  if (typeof value === "string") {
    return { type: "string", complete: false, count: value.length };
  }
  // JSON primitives are small and have no recursively expanding children.
  if (value === null || typeof value !== "object") {
    return { type: value === null ? "null" : typeof value, complete: false };
  }
  // Arrays retain item order and expose their full count even when only a few items are sampled.
  if (Array.isArray(value)) {
    const items = depth < MAX_PREVIEW_DEPTH ? value.slice(0, MAX_PREVIEW_CHILDREN).map((item) => previewResult(item, depth + 1)) : [];
    return { type: "array", count: value.length, complete: false, items };
  }
  return previewObject(value as Record<string, unknown>, depth);
}

/** Bounds object names independently so a giant property key cannot overflow the navigation envelope. */
function previewObject(value: Record<string, unknown>, depth: number): ResultPreview {
  const keys = Object.keys(value);
  const fields: Array<{ name: string; preview: ResultPreview }> = [];
  // At the depth boundary, counts preserve orientation without expanding another layer.
  if (depth < MAX_PREVIEW_DEPTH) {
    for (const name of keys.slice(0, MAX_PREVIEW_CHILDREN)) {
      // Omit oversized names whole rather than advertise a shortened key that cannot be retrieved.
      if (Buffer.byteLength(name, "utf8") <= MAX_PREVIEW_KEY_BYTES) {
        fields.push({ name, preview: previewResult(value[name], depth + 1) });
      }
    }
  }
  return { type: "object", count: keys.length, complete: false, fields };
}
