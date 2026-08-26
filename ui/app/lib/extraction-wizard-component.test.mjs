import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { URL } from "node:url";
import test from "node:test";

const wizardSource = readFileSync(new URL("../components/ExtractionWizard.tsx", import.meta.url), "utf8");
const apiSource = readFileSync(new URL("./api.ts", import.meta.url), "utf8");
const routeSource = readFileSync(new URL("../routes/integrations._index.tsx", import.meta.url), "utf8");
const summarySource = readFileSync(new URL("../components/DiscoveryReviewSummaryPanel.tsx", import.meta.url), "utf8");
const protocolSource = readFileSync(new URL("./extraction-wizard-protocol.ts", import.meta.url), "utf8");

// Apply owns workspace activation; browser navigation must not introduce a second mutation.
test("reviewed import delegates activation exactly once to Engine apply", () => {
  const applySource = wizardSource.slice(wizardSource.indexOf("async function applyPlan"), wizardSource.indexOf("async function cancelDiscovery"));
  assert.equal((applySource.match(/api\.integrations\.applyImport\(/g) || []).length, 1);
  assert.doesNotMatch(applySource, /api\.workspace\.addService/);
  assert.match(applySource, /error\.message/);
});

test("models only the bounded public snapshot payload", () => {
  const payloadSource = apiSource.slice(apiSource.indexOf("export interface DiscoveryPayload"), apiSource.indexOf("export interface DiscoverySnapshot"));
  for (const field of ["effective_workers", "max_pages", "max_depth", "max_selections", "operations", "proposals", "diagnostics", "contract", "plan", "failure_code"]) {
    assert.match(payloadSource, new RegExp(`\\b${field}\\b`));
  }
  for (const internal of ["account_id", "bundle_id", "metadata", "effective:", "draft?"]) {
    assert.equal(payloadSource.includes(internal), false, `internal discovery field exposed: ${internal}`);
  }
  assert.doesNotMatch(payloadSource, /\bselection\??:/);
  assert.match(apiSource, /interface DiscoveryPlanReceipt[\s\S]*plan_id:\s*string/);
});

test("uses SSE envelopes only to reload the authoritative snapshot", () => {
  assert.match(apiSource, /function discoveryStreamURL\(sessionID: string\)[\s\S]*getBaseURL\(\)/);
  assert.match(wizardSource, /fetchEventSource\(discoveryStreamURL\(sessionId\)/);
  assert.doesNotMatch(wizardSource, /\bBASE\b/);
  assert.match(wizardSource, /parseDiscoveryEnvelope\(message\.data, sessionId, revisionRef\.current\)/);
  assert.match(wizardSource, /async onmessage[\s\S]*await loadSnapshot\(\)/);
  assert.match(wizardSource, /async onopen[\s\S]*await loadSnapshot\(\)/);
  assert.doesNotMatch(wizardSource, /event\.message|message\.payload as/);
});

test("implements selection, draft review, proposals, plan, and separate apply stages", () => {
  assert.match(wizardSource, /case|awaiting_selection/);
  assert.match(wizardSource, /awaiting_review/);
  assert.match(wizardSource, /accept_enrichment/);
  assert.match(wizardSource, /reject_enrichment/);
  assert.match(wizardSource, /updateOverlayAction/);
  assert.match(wizardSource, /requestPlanAction/);
  assert.match(wizardSource, /applyImport\(plan\.plan_id, plan\.review_hash\)/);
  assert.match(wizardSource, /Apply reviewed plan/);
  assert.match(apiSource, /getDiscoveryReviewSummary[\s\S]*draft_id[\s\S]*draft_revision[\s\S]*review_hash/);
  assert.match(wizardSource, /error instanceof APIRequestError && error\.status === 409[\s\S]*loadSnapshot/);
  assert.match(wizardSource, /DiscoveryReviewSummaryPanel/);
  assert.match(summarySource, /operation_counts/);
  assert.doesNotMatch(summarySource, /schema\.raw|examples|source_url|account_id|bundle_id|object_key/);
});

// Discovery review rows must match the canonical service operation identity
// while keeping bounded contract facts closed until the user asks for them.
test("renders service-style operation disclosures collapsed by default", () => {
  const operationSource = summarySource.slice(
    summarySource.indexOf("function ReviewOperation"),
    summarySource.indexOf("function reviewSecurityLabel"),
  );

  assert.match(summarySource, /import \{ METHOD_COLORS \} from "~\/components\/EndpointRow"/);
  assert.match(operationSource, /<details className=/);
  assert.doesNotMatch(operationSource, /<details[^>]*\sopen(?:=|\s|>)/);
  assert.match(operationSource, /<summary className=/);
  assert.match(operationSource, /METHOD_COLORS\[method\]/);
  assert.match(operationSource, /operation\.path/);
  assert.match(operationSource, /operation\.operation_id/);
});

test("hard-cuts every removed discovery client route and opaque state", () => {
  const combined = `${apiSource}\n${wizardSource}\n${routeSource}`;
  for (const removed of [
    "/integrations/respond",
    "/integrations/preview_openapi",
    "/integrations/session/${sessionId}/recover",
    "/integrations/session/${sessionId}/cancel",
    "pending_question",
  ]) {
    assert.equal(combined.includes(removed), false, `removed discovery contract still present: ${removed}`);
  }
  assert.doesNotMatch(wizardSource, /case "thinking"|case "extracted"|case "complete"/);
  for (const removed of ['"applying"', '"done"', '"completed"', "deleteDiscoverySession"]) {
    assert.equal(`${apiSource}\n${wizardSource}\n${protocolSource}`.includes(removed), false, `removed v1 vocabulary still present: ${removed}`);
  }
});

test("closing the wizard preserves a resumable session", () => {
  const closeHandler = routeSource.slice(routeSource.indexOf("<ExtractionSessionPanel"));
  assert.match(closeHandler, /Closing preserves the durable session/);
  assert.doesNotMatch(closeHandler, /deleteDiscoverySession|cancelDiscoveryAction/);
});

test("derives the active wizard and CLI review mode from validated URL navigation", () => {
  assert.match(routeSource, /discoveryNavigationFromQuery\(searchParams\)/);
  assert.match(routeSource, /sessionID=\{activeDiscoverySessionID\}/);
  assert.match(routeSource, /reviewOnly=\{discoveryNavigation\.cliHandoff\}/);
  assert.match(routeSource, /openDiscoverySessionQuery\(current, sessionID\)/);
  assert.match(routeSource, /closeDiscoverySessionQuery\(current\)/);
  assert.doesNotMatch(routeSource, /useState<string \| null>\(null\).*Session/i);
});

test("CLI plan review returns to the terminal without rendering or invoking Apply", () => {
  const actionSource = wizardSource.slice(
    wizardSource.indexOf("function DiscoveryPlanAction"),
    wizardSource.indexOf("function DiscoveryTerminal"),
  );
  const reviewBranch = actionSource.slice(actionSource.indexOf("if (reviewOnly)"), actionSource.indexOf("return <button"));
  assert.match(reviewBranch, /Return to your terminal/);
  assert.doesNotMatch(reviewBranch, /Apply reviewed plan/);
  assert.match(actionSource, /return <button[^;]*Apply reviewed plan/);
  assert.match(wizardSource, /async function applyPlan\(\)[\s\S]*if \(reviewOnly\) return;/);
});
