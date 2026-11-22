export function formatServiceName(name: string): string {
  return name.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

// Versions are provider-defined identifiers, not numbers. Preserve them
// exactly instead of assuming they need a leading "v".
export function formatVersion(version: string): string {
  return version;
}

export function stripLinks(html: string | undefined | null): string {
  if (!html) return "";
  return html.replace(/<a\b[^>]*>/gi, "").replace(/<\/a>/gi, "");
}

// stripMarkdown converts provider-authored markdown into clean plain prose for
// compact card previews: markdown links ([text](url)) collapse to their label,
// list bullets are dropped, and whitespace runs flatten to single spaces. This
// keeps raw URLs out of the card so a short description reads as a description.
export function stripMarkdown(text: string): string {
  return text
    .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/^\s*[-*+]\s+/gm, "")
    .replace(/\s+/g, " ")
    .trim();
}

// truncateWords bounds prose to a word budget for compact previews such as
// service cards, while detail pages keep the provider's full text. Cutting on
// whitespace boundaries guarantees the excerpt never ends mid-word.
export function truncateWords(text: string, maxWords: number): string {
  const trimmed = text.trim();
  const words = trimmed.split(/\s+/);
  // Short prose renders verbatim so brief descriptions never gain a needless ellipsis.
  if (words.length <= maxWords) return trimmed;
  // The ellipsis signals omitted content without obscuring the excerpt's meaning.
  return words.slice(0, maxWords).join(" ") + "\u2026";
}

// formatCompoundUnit splits a rounded total into a major/minor pair, omitting zero remainders.
function formatCompoundUnit(total: number, divisor: number, majorUnit: string, minorUnit: string): string {
  const major = Math.floor(total / divisor);
  const minor = total % divisor;
  // Drop redundant zero remainders (e.g., "1h" instead of "1h 0m").
  return minor > 0 ? `${major}${majorUnit} ${minor}${minorUnit}` : `${major}${majorUnit}`;
}

// formatSubMinute formats durations below 60 seconds, returning null when rounding promotes into minutes.
function formatSubMinute(ms: number): string | null {
  // Preserve sub-millisecond precision for fast trace phases without collapsing non-zero timings to zero.
  if (ms > 0 && ms < 1) {
    const subMs = Number(ms.toFixed(2));
    if (subMs < 1) return `${subMs} ms`;
  }
  // Sub-second durations stay in milliseconds unless rounding promotes 999.5+ ms into 1 s.
  if (ms < 1000) {
    const roundedMs = Math.round(ms);
    if (roundedMs < 1000) return `${roundedMs} ms`;
  }
  // Under a minute, show up to two decimals in seconds while trimming redundant trailing zeros.
  const seconds = Number((ms / 1000).toFixed(2));
  return seconds < 60 ? `${seconds} s` : null;
}

// formatDuration converts a millisecond duration into the most readable unit
// scale (ms, s, m/s, h/m, or d/h) so long spans never render as huge raw ms.
export function formatDuration(ms: number | null | undefined): string {
  // Missing, non-finite, or negative inputs have no valid elapsed duration to render.
  if (ms == null || !Number.isFinite(ms) || ms < 0) return "";
  // Delegate sub-minute scales first; values rounding up to 60 s fall through to the minute tier.
  if (ms < 60_000) {
    const subMinute = formatSubMinute(ms);
    if (subMinute !== null) return subMinute;
  }
  // Under an hour, pair minutes with whole remaining seconds after rounding to avoid "59m 60s".
  const totalSeconds = Math.round(ms / 1000);
  if (totalSeconds < 3600) return formatCompoundUnit(totalSeconds, 60, "m", "s");
  // Under a day, pair hours with rounded remaining minutes so values never display "23h 60m".
  const totalMinutes = Math.round(ms / 60_000);
  if (totalMinutes < 1440) return formatCompoundUnit(totalMinutes, 60, "h", "m");
  // Multi-day spans pair days with rounded remaining hours to stay compact in receipts and tables.
  return formatCompoundUnit(Math.round(ms / 3_600_000), 24, "d", "h");
}
