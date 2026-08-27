import { resolveJsonPointer } from "./jsonPointer.js";
import { isResultRecord, MAX_RESULT_FIELDS, MAX_RESULT_FIELD_BYTES } from "./resultCollections.js";
import { executeRequest, pageScript, SessionToolRequest } from "./sessionContract.js";

/** Fields are literal immediate row keys; only the collection path uses RFC 6901. */
export interface ResultPageOptions {
  path?: string;
  fields?: string[];
  offset?: number;
}

/** Explicit ranges prevent a terminal page from being confused with the entire original collection. */
export interface ResultPage {
  recovery_action?: "continue_stored_result";
  execute_request?: "use_next_request";
  provider_execution: "complete";
  automatic_replay: false;
  result_ref: string;
  path: string;
  offset: number;
  total: number;
  returned: number;
  nextOffset: number | null;
  complete: boolean;
  items: unknown[];
  next_request?: SessionToolRequest;
}

/** Projects and sizes whole rows from an already admitted snapshot without I/O or provider execution. */
export function pageResult(root: unknown, reference: string, rawOptions: unknown, maxBytes: number): ResultPage {
  const options = pageOptions(rawOptions);
  const resolved = resolveJsonPointer(root, options.path);
  // One explicit array is required; missing paths and object maps cannot silently turn into an empty page.
  if (resolved.error || !Array.isArray(resolved.value)) {
    throw new Error("MCP_RESULT_PAGE_PATH_INVALID: path must be an existing array's RFC 6901 JSON Pointer; use an empty path for a root array.");
  }
  const rows = resolved.value;
  // Out-of-range offsets are mistakes, while exactly total is a valid terminal empty page.
  if (options.offset > rows.length) throw new Error("MCP_RESULT_PAGE_OFFSET_INVALID: offset exceeds the collection count.");
  validateAvailableFields(rows, options.fields);
  return packPage(rows, reference, options, maxBytes);
}

/** Proves every selected row can advance through the same exact continuation before it is advertised. */
export function pageResultCanProgress(root: unknown, reference: string, rawOptions: unknown, maxBytes: number): boolean {
  try {
    const options = pageOptions(rawOptions);
    const resolved = resolveJsonPointer(root, options.path);
    // Missing arrays cannot become a retained continuation candidate.
    if (resolved.error || !Array.isArray(resolved.value)) return false;
    validateAvailableFields(resolved.value, options.fields);
    for (let index = 0; index < resolved.value.length; index++) {
      const page = pageEnvelope(reference, options, resolved.value.length, index, 1, maxBytes);
      page.items = [projectRow(resolved.value[index], options.fields)];
      // One whole selected row plus its exact next request must fit at every cursor.
      if (Buffer.byteLength(JSON.stringify(page), "utf8") > maxBytes) return false;
    }
    const empty = pageEnvelope(reference, options, 0, 0, 0, maxBytes);
    return resolved.value.length > 0 || Buffer.byteLength(JSON.stringify(empty), "utf8") <= maxBytes;
  } catch {
    // Invalid selectors and hostile values simply fall back to bounded structural inspection.
    return false;
  }
}

/** Copies and validates caller options once so getters cannot change a checked projection later. */
function pageOptions(value: unknown): Required<Pick<ResultPageOptions, "path" | "offset">> & Pick<ResultPageOptions, "fields"> {
  // Reject unknown options rather than silently ignoring a misspelled selector or unsupported filter.
  if (!isResultRecord(value) || Object.keys(value).some((key) => !["path", "fields", "offset"].includes(key))) {
    throw new Error("MCP_RESULT_PAGE_OPTIONS_INVALID: use only path, fields, and offset.");
  }
  const { path = "", fields, offset = 0 } = value;
  // Offsets identify exact stored positions and must never be coerced or rounded.
  if (typeof path !== "string" || typeof offset !== "number" || !Number.isSafeInteger(offset) || offset < 0) {
    throw new Error("MCP_RESULT_PAGE_OPTIONS_INVALID: path must be a string and offset a non-negative safe integer.");
  }
  return { path, offset, fields: pageFields(fields) };
}

/** Preserves literal field names, including slash/dot keys, without introducing another mapping language. */
function pageFields(value: unknown): string[] | undefined {
  // Omission requests complete rows; an empty list is rejected to prevent accidental arrays of empty objects.
  if (value === undefined) return undefined;
  // Cardinality and key bounds also bound projection work on adversarial inputs.
  if (!Array.isArray(value) || value.length === 0 || value.length > MAX_RESULT_FIELDS) {
    throw new Error("MCP_RESULT_PAGE_FIELDS_INVALID: fields must contain 1 to 32 distinct immediate field names.");
  }
  const fields = [...value];
  for (const field of fields) {
    // Exact keys may be empty, but non-string or oversized values must not enter projection or diagnostics.
    if (typeof field !== "string" || Buffer.byteLength(field) > MAX_RESULT_FIELD_BYTES) {
      throw new Error("MCP_RESULT_PAGE_FIELDS_INVALID: each field must be a string of at most 128 UTF-8 bytes.");
    }
  }
  // Duplicates commonly signal a bad selector and cannot produce additional useful output.
  if (new Set(fields).size !== fields.length) throw new Error("MCP_RESULT_PAGE_FIELDS_INVALID: fields must be distinct.");
  return fields;
}

/** Recognizes sparse fields anywhere in the collection while rejecting selectors that exist in no row. */
function validateAvailableFields(rows: unknown[], fields?: string[]): void {
  // Empty collections have no observed schema; any syntactically valid projection returns their empty page.
  if (!fields || rows.length === 0) return;
  const missing = new Set(fields);
  for (const row of rows) {
    // Mixed primitive rows cannot satisfy an object projection; whole-row reads are still supported.
    if (!isResultRecord(row)) continue;
    for (const field of missing) {
      // Own keys avoid treating constructor/prototype members as provider fields.
      if (Object.hasOwn(row, field)) missing.delete(field);
    }
    // Most known projections are established by the first row; stop once every key is proven present somewhere.
    if (missing.size === 0) return;
  }
  throw new Error("MCP_RESULT_PAGE_FIELDS_UNKNOWN: at least one selected field does not occur in the collection; inspect stored keys without repeating the operation.");
}

/** Copies selected values without prototype setters and leaves absent sparse properties absent. */
function projectRow(row: unknown, fields?: string[]): unknown {
  // Omitted fields means preserve the entire JSON row, including primitive and array rows.
  if (!fields) return row;
  // A primitive must not disappear or turn into an empty object under an object-field projection.
  if (!isResultRecord(row)) throw new Error("MCP_RESULT_PAGE_ROW_TYPE: selected fields require object rows; omit fields to retrieve mixed rows.");
  const projected: Record<string, unknown> = Object.create(null);
  for (const field of fields) {
    // Missing sparse fields stay absent, preserving the distinction from an explicitly null value.
    if (Object.hasOwn(row, field)) projected[field] = row[field];
  }
  return projected;
}

/** Charges each row once plus the exact changing envelope, avoiding quadratic whole-page serialization. */
function packPage(rows: unknown[], reference: string, options: ReturnType<typeof pageOptions>, maxBytes: number): ResultPage {
  const items: unknown[] = [];
  let itemBytes = 0;
  let page = pageEnvelope(reference, options, rows.length, options.offset, 0, maxBytes);
  const terminalBytes = Buffer.byteLength(JSON.stringify(pageEnvelope(reference, options, rows.length, options.offset, rows.length - options.offset, maxBytes)));
  // Very long paths can consume a small configured budget before any row is selected.
  if (Buffer.byteLength(JSON.stringify(page)) > maxBytes) throw new Error("MCP_RESULT_PAGE_METADATA_TOO_LARGE: page metadata exceeds outputBudgetBytes; increase the budget or use session.get for a smaller projection.");
  for (let index = options.offset; index < rows.length; index++) {
    const row = projectRow(rows[index], options.fields);
    // Commas are charged only between rows; the empty array brackets are already in the envelope.
    const addedBytes = Buffer.byteLength(JSON.stringify(row)) + (items.length > 0 ? 1 : 0);
    const candidate = pageEnvelope(reference, options, rows.length, options.offset, items.length + 1, maxBytes);
    // Exact UTF-8 plus JSON escaping and changing count digits decide whether a complete next row fits.
    if (Buffer.byteLength(JSON.stringify(candidate)) + itemBytes + addedBytes <= maxBytes) {
      page = candidate;
    // Terminal null/true metadata can be shorter than a numeric continuation. Inspect that bounded tail only
    // when even its minimum one-byte JSON rows plus commas could still fit, avoiding premature short pages.
    } else if (terminalBytes + itemBytes + addedBytes + 2 * (rows.length - index - 1) > maxBytes) {
      break;
    }
    items.push(row);
    itemBytes += addedBytes;
  }
  // An oversized first row must fail explicitly instead of returning a non-progressing empty page.
  if (page.returned === 0 && options.offset < rows.length) throw new Error("MCP_RESULT_ROW_TOO_LARGE: one selected row exceeds outputBudgetBytes; choose fewer fields or use session.get to inspect a field or string slice. Do not repeat the provider call.");
  page.items = items.slice(0, page.returned);
  return page;
}

/** Keeps continuation positions independent of field projection and never invents a cursor beyond the snapshot. */
function pageEnvelope(reference: string, options: ReturnType<typeof pageOptions>, total: number, offset: number, returned: number, maxBytes: number): ResultPage {
  const end = offset + returned;
  // Completion means no rows remain after this range; offset still shows whether earlier pages were omitted.
  const complete = end === total;
  const page: ResultPage = {
    provider_execution: "complete",
    automatic_replay: false,
    result_ref: reference, path: options.path, offset, total, returned,
    nextOffset: complete ? null : end, complete, items: [],
  };
  // Incomplete pages carry the exact same selector and budget so continuation requires no reconstruction.
  if (!complete) {
    page.recovery_action = "continue_stored_result";
    page.execute_request = "use_next_request";
    page.next_request = executeRequest(pageScript(reference, options.path, options.fields, end), maxBytes);
  }
  return page;
}
