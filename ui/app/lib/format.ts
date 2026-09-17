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
