/**
 * Small, self-contained fuzzy scorer for search_docs's query mode. Written by
 * hand rather than pulling in a matching library (e.g. fuse.js) -- the spike
 * only needs "does this query loosely match this operation" over a handful
 * of short fields (name, description, path), which doesn't justify an extra
 * dependency in code whose whole point is to be small and auditable.
 *
 * Score is 0 (no match) to 1 (best match): an exact substring hit scores
 * highest, a prefix match scores second, and partial word-token overlap
 * scores lowest but still above zero -- good enough to rank "list repos" vs
 * "get repo" for a query like "list", without needing real NLP.
 */
export function fuzzyScore(query: string, target: string): number {
  const q = query.trim().toLowerCase();
  const t = target.toLowerCase();
  if (q === "" || t === "") {
    return 0;
  }

  if (t.includes(q)) {
    // Reward matches near the start of the target slightly more than a
    // match buried deep in a long description, without needing a full
    // edit-distance calculation.
    const position = t.indexOf(q);
    return position === 0 ? 1 : 0.9;
  }

  return tokenOverlapScore(q, t);
}

/** tokenOverlapScore rewards queries whose words appear in the target, even
 * out of order or partially -- e.g. "user repos" against "List user
 * repositories" won't substring-match but should still rank above unrelated
 * operations. */
function tokenOverlapScore(query: string, target: string): number {
  const queryTokens = query.split(/\s+/).filter(Boolean);
  if (queryTokens.length === 0) {
    return 0;
  }

  const targetTokens = target.split(/\s+/).filter(Boolean);
  let matched = 0;
  for (const qt of queryTokens) {
    if (targetTokens.some((tt) => tt.startsWith(qt) || qt.startsWith(tt))) {
      matched++;
    }
  }

  // Scaled below the substring-match range (0, 0.7] so an exact/prefix hit
  // on any field always outranks a partial token-overlap hit.
  return (matched / queryTokens.length) * 0.7;
}

/**
 * bestScore takes the max fuzzyScore across several fields (name,
 * description, path) -- a query can match any one of them and the operation
 * should still surface, rather than requiring the match to be in one
 * specific field.
 */
export function bestScore(query: string, fields: (string | undefined)[]): number {
  let best = 0;
  for (const field of fields) {
    if (!field) continue;
    best = Math.max(best, fuzzyScore(query, field));
  }
  return best;
}
