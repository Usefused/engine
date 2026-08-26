/** One public catalogue field and its relative intent-ranking importance. */
export interface WeightedSearchField {
  value?: string;
  weight: number;
}

/** Splits prose, identifiers, and paths into the same deterministic lexical form. */
function tokenize(value: string): string[] {
  const words = value
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .toLocaleLowerCase("en-US")
    .match(/[\p{L}\p{N}]+/gu);
  // An empty lexical form cannot provide evidence for a catalogue match.
  return words ?? [];
}

/** Treats exact and prefix-related terms as intent matches without guessing synonyms. */
function tokenMatchScore(queryToken: string, targetToken: string): number {
  // Exact words are stronger evidence than identifier-prefix similarity.
  if (queryToken === targetToken) {
    return 4;
  }
  // Prefix matching keeps concise intent useful for common API word variants.
  if (queryToken.length >= 3 && (targetToken.startsWith(queryToken) || queryToken.startsWith(targetToken))) {
    return 2;
  }
  return 0;
}

/** Scores one query token against a field while counting it at most once. */
function fieldTokenScore(queryToken: string, targetTokens: string[]): number {
  let best = 0;
  for (const targetToken of targetTokens) {
    best = Math.max(best, tokenMatchScore(queryToken, targetToken));
  }
  return best;
}

/** Scores a field from exact phrase and per-token evidence. */
function weightedFieldScore(queryTokens: string[], field: WeightedSearchField): number {
  // Missing fields remain neutral so optional metadata never penalizes an operation.
  if (!field.value) {
    return 0;
  }
  const targetTokens = tokenize(field.value);
  // A field with no searchable words cannot contribute lexical evidence.
  if (targetTokens.length === 0) {
    return 0;
  }
  let score = 0;
  for (const queryToken of queryTokens) {
    score += fieldTokenScore(queryToken, targetTokens) * field.weight;
  }
  // Consecutive intent terms distinguish a coherent phrase from scattered words.
  if (targetTokens.join(" ").includes(queryTokens.join(" "))) {
    score += 3 * field.weight;
  }
  return score;
}

/** Produces a deterministic weighted lexical rank without network or model calls. */
export function weightedIntentScore(query: string, fields: WeightedSearchField[]): number {
  const queryTokens = tokenize(query);
  // Whitespace-only intent is list mode, not a universal fuzzy match.
  if (queryTokens.length === 0) {
    return 0;
  }
  let score = 0;
  for (const field of fields) {
    score += weightedFieldScore(queryTokens, field);
  }
  return score;
}

/** Preserves the small public scorer for callers that need one unweighted field. */
export function fuzzyScore(query: string, target: string): number {
  const queryTokens = tokenize(query);
  const targetTokens = tokenize(target);
  // Empty lexical forms cannot match and retain the historical zero contract.
  if (queryTokens.length === 0 || targetTokens.length === 0) {
    return 0;
  }
  const normalizedQuery = queryTokens.join(" ");
  const normalizedTarget = targetTokens.join(" ");
  // An exact normalized prefix remains the strongest single-field result.
  if (normalizedTarget.startsWith(normalizedQuery)) {
    return 1;
  }
  // A later phrase hit stays above loose token overlap.
  if (normalizedTarget.includes(normalizedQuery)) {
    return 0.9;
  }
  let matched = 0;
  for (const queryToken of queryTokens) {
    // Each query term counts once even when repeated in the target.
    if (targetTokens.some((targetToken) => tokenMatchScore(queryToken, targetToken) > 0)) {
      matched++;
    }
  }
  return (matched / queryTokens.length) * 0.7;
}

/** Preserves max-field matching while the catalogue uses richer field weights. */
export function bestScore(query: string, fields: (string | undefined)[]): number {
  let best = 0;
  for (const field of fields) {
    best = Math.max(best, fuzzyScore(query, field ?? ""));
  }
  return best;
}
