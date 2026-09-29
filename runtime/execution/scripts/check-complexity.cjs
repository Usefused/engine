const fs = require("node:fs");
const path = require("node:path");
const ts = require("typescript");

const MAX_COMPLEXITY = 10;
const FILES = ["src/index.ts", "src/bundle.ts", "src/bindings.ts", "src/cli.ts", "src/client_generator.ts", "src/client_schema.ts", "src/manifest-worker.ts", "test/fixture.ts", "test/execution.test.ts", "examples/live-greeting.ts"];
const FUNCTION_KINDS = new Set([
  ts.SyntaxKind.FunctionDeclaration, ts.SyntaxKind.FunctionExpression,
  ts.SyntaxKind.ArrowFunction, ts.SyntaxKind.MethodDeclaration,
]);
const DECISION_KINDS = new Set([
  ts.SyntaxKind.IfStatement, ts.SyntaxKind.ConditionalExpression,
  ts.SyntaxKind.ForStatement, ts.SyntaxKind.ForInStatement,
  ts.SyntaxKind.ForOfStatement, ts.SyntaxKind.WhileStatement,
  ts.SyntaxKind.DoStatement, ts.SyntaxKind.CatchClause, ts.SyntaxKind.CaseClause,
]);
const LOGICAL_KINDS = new Set([
  ts.SyntaxKind.AmpersandAmpersandToken, ts.SyntaxKind.BarBarToken,
  ts.SyntaxKind.QuestionQuestionToken,
]);

// Count decisions within one function without charging nested callbacks twice.
function complexityOf(node) {
  let complexity = 1;
  // Each nested function owns its own decision count.
  function visit(child) {
    // Nested callbacks are checked separately by the outer tree walk.
    if (child !== node.body && FUNCTION_KINDS.has(child.kind)) return;
    // Logical branches affect complexity just like control statements.
    if (DECISION_KINDS.has(child.kind) || ts.isBinaryExpression(child) && LOGICAL_KINDS.has(child.operatorToken.kind)) complexity += 1;
    ts.forEachChild(child, visit);
  }
  // A declaration without a body has no executable decisions.
  if (node.body) visit(node.body);
  return complexity;
}

// Report every function that exceeds the package's release ceiling.
function inspectFile(file) {
  const source = ts.createSourceFile(file, fs.readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true);
  let failed = false;
  // Traverse all declarations so anonymous test callbacks are included.
  function visit(node) {
    // Only executable function bodies have a meaningful complexity score.
    if (FUNCTION_KINDS.has(node.kind)) {
      const complexity = complexityOf(node);
      // A bounded function should remain easy to review and test.
      if (complexity > MAX_COMPLEXITY) {
        const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
        process.stderr.write(`${file}:${line} complexity=${complexity}\n`);
        failed = true;
      }
    }
    ts.forEachChild(node, visit);
  }
  visit(source);
  return failed;
}

// Keep the gate deterministic across local and CI runs.
const failed = FILES.map((file) => inspectFile(path.normalize(file))).some(Boolean);
// A failing gate prevents a complex execution path from shipping unnoticed.
if (failed) process.exitCode = 1;
else process.stdout.write(`Complexity <= ${MAX_COMPLEXITY} for Unified App TypeScript.\n`);
