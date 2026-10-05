import type { UnifiedServicePin } from "./unified-app-contract";

export type DescribeKind = "app" | "sdk" | "mcp" | "api" | "unified_app";
export type DescribeStage = "intent" | "services" | "operations" | "source" | "review";
export type DescribeProgress = (message: string, stage?: DescribeStage) => void;
export type DescribeSelectionReady = (proposal: AppDescription) => void;
export type DescribeSchedule = <T>(work: () => Promise<T>) => Promise<T>;

export interface DescribeAnswer { question: string; answer: string }

/** Keeps the original goal and explicit answers together across intent parsing and source drafting. */
export function describeGoalWithAnswers(goal: string, answers: DescribeAnswer[]): string {
  // Ordinary descriptions retain their existing wire representation when no clarification was needed.
  const combined = answers.length
    ? `${goal.trim()}\n\nAnswers to follow-up questions:\n${JSON.stringify(answers)}`
    : goal.trim();
  // Match the Registry's byte bound before spending another model request, including non-ASCII answers.
  if (new TextEncoder().encode(combined).length > 16384) throw new Error("Your description and answers are too long. Shorten the description and try again.");
  return combined;
}

/** Shares four network slots across services and operations, stopping queued work after a failure. */
export function createDescribeScheduler(): DescribeSchedule {
  let active = 0;
  const waiting: Array<() => void> = [];
  let failed = false;
  let failure: unknown;
  /** Each waiter inherits the released slot, keeping the request-wide concurrency limit stable. */
  return async function schedule<T>(work: () => Promise<T>): Promise<T> {
    // New requests wait rather than multiplying concurrency for every service.
    if (active >= 4) await new Promise<void>((resolve) => waiting.push(resolve));
    else active++;
    try {
      // A failed dependency invalidates the whole proposal; do not start more paid requests.
      if (failed) throw failure;
      const result = await work();
      // In-flight siblings cannot publish progress after another request has failed.
      if (failed) throw failure;
      return result;
    } catch (cause) { failed = true; failure = cause; throw cause; }
    finally {
      const next = waiting.shift();
      // Transfer an occupied slot directly to the next waiter, or release it when the queue empties.
      if (next) next();
      else active--;
    }
  };
}

/** Drains in-flight requests before surfacing an error so a later attempt cannot receive stale progress. */
export async function settleDescribe<T>(requests: Promise<T>[]): Promise<T[]> {
  const results = await Promise.allSettled(requests);
  return results.map((result) => {
    // Preserve input order and the original failure instead of committing a partial proposal.
    if (result.status === "rejected") throw result.reason;
    return result.value;
  });
}

export interface DescribeServiceCandidate { id: string; name: string; slug: string; provider?: { handle: string } }
export type ChooseDescribeService = (reference: string, candidates: DescribeServiceCandidate[]) => Promise<string>;
export interface AppServicePin extends UnifiedServicePin { select_all?: boolean; webhooks?: string[] }
export interface AppDescription {
  name: string;
  description: string;
  language?: string;
  source?: string;
  services: Record<string, AppServicePin>;
}

/** Combines repeated provider mentions without dropping capabilities or changing immutable versions. */
export function mergeDescribePin(previous: AppServicePin | undefined, next: AppServicePin): AppServicePin {
  // The first reference establishes the canonical immutable contract.
  if (!previous) return next;
  // Aliases cannot merge capabilities from different snapshots.
  if (previous.service_id !== next.service_id || previous.service_version_id !== next.service_version_id) throw new Error("Conflicting service versions in the description.");
  return { ...previous, ...next, select_all: previous.select_all || next.select_all, operations: [...new Set([...previous.operations, ...next.operations])], webhooks: [...new Set([...(previous.webhooks ?? []), ...(next.webhooks ?? [])])] };
}

/** Normalizes event concepts with the same small vocabulary used by CLI describe. */
function eventTokens(value: string): Set<string> {
  const synonyms: Record<string, string> = { succeeded: "success", successful: "success", successfully: "success", failures: "fail", failed: "fail", failure: "fail", created: "create", creation: "create", updated: "update", deleted: "delete", deletion: "delete", completed: "complete", completion: "complete" };
  return new Set(value.toLowerCase().split(/[^\p{L}\p{N}]+/u).map((token) => {
    // Known event-state variants take precedence over simple plural folding.
    if (synonyms[token]) return synonyms[token];
    return token.length > 3 && token.endsWith("s") ? token.slice(0, -1) : token;
  }).filter((token) => token && token !== "event" && token !== "webhook"));
}

/** Resolves one inbound intent conservatively, rejecting ties instead of widening subscriptions. */
function matchEvent(query: string, available: Array<{ name: string; description: string }>): string {
  const exact = available.find((event) => event.name.toLowerCase() === query.trim().toLowerCase());
  // Exact provider event identifiers outrank similarity in descriptive prose.
  if (exact) return exact.name;
  const tokens = eventTokens(query);
  const scores = available.map((event) => {
    const names = eventTokens(event.name);
    const descriptions = eventTokens(event.description ?? "");
    return { name: event.name, score: [...tokens].filter((token) => names.has(token)).length * 2 + [...tokens].filter((token) => descriptions.has(token)).length };
  });
  const best = Math.max(0, ...scores.map((event) => event.score));
  const matches = scores.filter((event) => event.score === best);
  // A zero score or tie needs more specific user intent.
  if (!best || matches.length !== 1) throw new Error(`Specify an exact webhook event for “${query}”.`);
  return matches[0].name;
}

/** A general event request selects the declared catalogue; specific requests must each resolve uniquely. */
export function matchDescribeEvents(queries: string[], available: Array<{ name: string; description: string }>): string[] {
  // Missing inbound contracts cannot produce a successful empty subscription.
  if (!available.length) throw new Error("This service version has no webhook events.");
  // CLI treats an unqualified inbound request as the full advertised event set.
  if (!queries.length) return [...new Set(available.map((event) => event.name))];
  return [...new Set(queries.map((query) => matchEvent(query, available)))];
}

/** Compares capability state independently of callback identity, property order, and catalogue refreshes. */
export function describeSelectionKey(services: Record<string, AppServicePin>): string {
  return JSON.stringify(Object.entries(services).sort(([left], [right]) => left.localeCompare(right)).map(([key, pin]) => [key, pin.service_id, pin.service_version_id, pin.version, [...pin.operations].sort(), Boolean(pin.select_all), [...(pin.webhooks ?? [])].sort()]));
}
