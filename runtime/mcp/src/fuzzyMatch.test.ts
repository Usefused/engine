import { describe, expect, it } from "vitest";
import { bestScore, fuzzyScore } from "./fuzzyMatch.js";

describe("fuzzyScore", () => {
  it("scores an exact substring match highest", () => {
    expect(fuzzyScore("repos", "List user repositories")).toBeGreaterThan(0.8);
  });

  it("scores a prefix match at the max", () => {
    expect(fuzzyScore("list", "List user repositories")).toBe(1);
  });

  it("scores partial token overlap lower than a substring match", () => {
    const substringScore = fuzzyScore("repos", "List user repositories");
    // Reversed word order is not a literal substring of the target (unlike
    // "user repos", which is -- "repos" is a literal prefix of
    // "repositories"), so this genuinely exercises the token-overlap path,
    // which is intentionally scored below any substring/prefix hit.
    const tokenScore = fuzzyScore("repositories user", "List user repositories");
    expect(tokenScore).toBeLessThan(substringScore);
    expect(tokenScore).toBeGreaterThan(0);
  });

  it("returns 0 for no overlap at all", () => {
    expect(fuzzyScore("banana", "List user repositories")).toBe(0);
  });

  it("returns 0 for an empty query or target", () => {
    expect(fuzzyScore("", "anything")).toBe(0);
    expect(fuzzyScore("anything", "")).toBe(0);
  });

  it("is case-insensitive", () => {
    expect(fuzzyScore("REPOS", "list user repositories")).toBeGreaterThan(0);
  });
});

describe("bestScore", () => {
  it("takes the max score across multiple fields", () => {
    const score = bestScore("getRepo", ["List user repositories", undefined, "getRepo"]);
    expect(score).toBe(1);
  });

  it("returns 0 when no field matches", () => {
    expect(bestScore("nomatch", ["a", "b", undefined])).toBe(0);
  });
});
