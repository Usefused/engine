/** Formats Registry's exact integer string without narrowing large counts. */
export function formatAppDownloadCount(value: string | null | undefined): string {
  if (!value || !/^(0|[1-9][0-9]*)$/.test(value)) return "—";
  try {
    return BigInt(value).toLocaleString();
  } catch {
    return "—";
  }
}
