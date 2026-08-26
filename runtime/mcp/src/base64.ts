import { atob as nodeAtob } from "node:buffer";

/** Keep conversion allocations bounded independently of the smaller model-facing output budget. */
export const BASE64_MAX_BYTES = 1 << 20;
const MAX_ENCODED_CHARS = 4 * Math.ceil(BASE64_MAX_BYTES / 3);

/** Rejects coercible objects and oversized strings before allocating conversion buffers. */
function boundedString(value: string, maxLength: number): void {
  // Implicit string conversion can execute user hooks and allocate outside our size admission.
  if (typeof value !== "string") throw new Error("MCP_BASE64_INVALID: expected a string");
  // Check code units first so byte validation never scans an unbounded caller input.
  if (value.length > maxLength) throw new Error("MCP_BASE64_LIMIT: conversion exceeds the 1 MiB byte limit");
}

/** Accepts padded/unpadded standard and URL-safe base64 without silently discarding invalid characters. */
function normalizedBase64(value: string): string {
  boundedString(value, MAX_ENCODED_CHARS);
  const normalized = value.replace(/[\t\n\f\r ]/g, "").replace(/-/g, "+").replace(/_/g, "/");
  const unpadded = normalized.replace(/=+$/, "");
  // Node's permissive Buffer decoder otherwise accepts truncated or malformed provider data.
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(normalized) || unpadded.length % 4 === 1) {
    throw new Error("MCP_BASE64_INVALID: malformed base64");
  }
  // Padding, when present, must describe a complete final quartet.
  if (normalized.includes("=") && normalized.length % 4 !== 0) throw new Error("MCP_BASE64_INVALID: malformed padding");
  // Preflight decoded length rather than allocating a buffer and only then rejecting it.
  if (Math.floor(unpadded.length * 3 / 4) > BASE64_MAX_BYTES) throw new Error("MCP_BASE64_LIMIT: conversion exceeds the 1 MiB byte limit");
  return normalized;
}

/** Decodes provider base64/base64url as UTF-8 text without exposing a Buffer or backing allocation. */
export function decodeBase64(value: string): string {
  const bytes = Buffer.from(normalizedBase64(value), "base64");
  try {
    // Silent replacement of malformed UTF-8 can corrupt identifiers and structured provider data.
    return new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
  } catch {
    throw new Error("MCP_BASE64_INVALID_UTF8: use atob for binary data");
  }
}

/** Encodes UTF-8 text; URL-safe output omits padding for provider APIs that require that alphabet. */
export function encodeBase64(value: string, urlSafe = false): string {
  boundedString(value, BASE64_MAX_BYTES);
  // Code-unit admission alone does not bound multi-byte UTF-8 allocations.
  if (Buffer.byteLength(value, "utf8") > BASE64_MAX_BYTES) throw new Error("MCP_BASE64_LIMIT: conversion exceeds the 1 MiB byte limit");
  // Do not coerce arbitrary objects or truthy values into an encoding choice.
  if (typeof urlSafe !== "boolean") throw new Error("MCP_BASE64_INVALID: urlSafe must be a boolean");
  return Buffer.from(value, "utf8").toString(urlSafe ? "base64url" : "base64");
}

/** Preserves browser atob binary-string semantics; unlike decodeBase64 this rejects URL-safe input. */
export function boundedAtob(value: string): string {
  boundedString(value, MAX_ENCODED_CHARS);
  // Browser compatibility requires the standard alphabet even though the UTF-8 helper accepts both.
  if (/[-_]/.test(value)) throw new Error("MCP_BASE64_INVALID: atob requires standard base64; use decodeBase64 for base64url");
  const normalized = normalizedBase64(value);
  return nodeAtob(normalized);
}
