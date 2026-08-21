import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import ts from "typescript";

const uiRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const defaultAppRoot = path.join(uiRoot, "app");
const manifestPath = path.join(uiRoot, "testdata/graphql-documents.json");
const registryManifestPath = path.resolve(uiRoot, "../../backend/internal/registry/graph/testdata/ui-graphql-documents.json");
const dynamicValue = "UI_DYNAMIC_VALUE";

// fail reports a bounded scanner error without a stack trace that would make manifest failures noisy.
function fail(message) {
  process.stderr.write(`${message}\n`);
  process.exitCode = 1;
}

// parseOptions validates the deliberately small command-line surface used by tests and manifest generation.
function parseOptions(args) {
  const options = { endpoint: "", writeManifest: false, allowRuntimeScalarInterpolation: false };
  for (const arg of args) {
    if (arg === "--write-manifest") options.writeManifest = true;
    else if (arg === "--allow-runtime-scalar-interpolation") options.allowRuntimeScalarInterpolation = true;
    else if (arg.startsWith("--endpoint=")) options.endpoint = arg.slice("--endpoint=".length);
    else throw new Error(`unknown option: ${arg}`);
  }
  if (options.endpoint && !["engine", "registry"].includes(options.endpoint)) {
    throw new Error(`unsupported endpoint: ${options.endpoint}`);
  }
  return options;
}

// collectSourceFiles returns a stable inventory so generated manifests never depend on filesystem iteration order.
function collectSourceFiles(root) {
  const files = [];
  for (const entry of fs.readdirSync(root, { withFileTypes: true }).sort((left, right) => left.name.localeCompare(right.name))) {
    const current = path.join(root, entry.name);
    if (entry.isDirectory()) files.push(...collectSourceFiles(current));
    else if (/\.tsx?$/.test(entry.name) && !entry.name.endsWith(".d.ts")) files.push(current);
  }
  return files;
}

// createGraphQLProgram builds one TypeScript program so imported fragments resolve through compiler symbols.
function createGraphQLProgram(files) {
  return ts.createProgram({
    rootNames: files,
    options: {
      allowJs: false,
      jsx: ts.JsxEmit.Preserve,
      module: ts.ModuleKind.ESNext,
      moduleResolution: ts.ModuleResolutionKind.Bundler,
      noEmit: true,
      baseUrl: uiRoot,
      paths: { "~/*": ["app/*"] },
      skipLibCheck: true,
      target: ts.ScriptTarget.ESNext,
    },
  });
}

// uniqueStrings removes duplicate branches while preserving their source-defined order.
function uniqueStrings(values) {
  return [...new Set(values)];
}

// symbolFor follows import aliases to the declaration that owns a static GraphQL fragment.
function symbolFor(node, checker) {
  let symbol = checker.getSymbolAtLocation(node);
  if (symbol && (symbol.flags & ts.SymbolFlags.Alias) !== 0) symbol = checker.getAliasedSymbol(symbol);
  return symbol;
}

// unwrapExpression removes TypeScript-only wrappers that do not affect a static string value.
function unwrapExpression(node) {
  let current = node;
  while (current && (ts.isAsExpression(current) || ts.isSatisfiesExpression(current) || ts.isTypeAssertionExpression(current) || ts.isNonNullExpression(current) || ts.isParenthesizedExpression(current))) {
    current = current.expression;
  }
  return current;
}

// literalString returns source literals without treating runtime identifiers as constants.
function literalString(node) {
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) return node.text;
  return null;
}

// isGraphQLStringPosition distinguishes unsafe structural interpolation from legacy scalar interpolation inside quotes.
function isGraphQLStringPosition(prefix) {
  let quoted = false;
  let escaped = false;
  for (const character of prefix) {
    if (escaped) escaped = false;
    else if (character === "\\") escaped = true;
    else if (character === '"') quoted = !quoted;
  }
  return quoted;
}

// appendVariants computes the bounded Cartesian product needed when a fragment has static alternatives.
function appendVariants(prefixes, values, suffix = "") {
  const expanded = [];
  for (const prefix of prefixes) {
    for (const value of values) expanded.push(prefix + value + suffix);
  }
  return expanded;
}

// evaluateTemplate resolves imported fragments and rejects runtime structure rather than silently validating a fake query.
function evaluateTemplate(node, state) {
  let variants = [node.head.text];
  for (const span of node.templateSpans) {
    let values = evaluateExpression(span.expression, state);
    if (!values) {
      const scalar = variants.every(isGraphQLStringPosition);
      if (!scalar) throw new Error(`unresolved structural template interpolation: ${span.expression.getText()}`);
      if (!state.allowRuntimeScalarInterpolation) throw new Error("runtime GraphQL interpolation must use variables");
      // Why: this opt-in exists only for auditing legacy scalar interpolation; schema tests must still see parseable text.
      values = [dynamicValue];
    }
    variants = appendVariants(variants, values, span.literal.text);
  }
  return uniqueStrings(variants);
}

// evaluateSymbol resolves initialized declarations and parameter bindings while breaking recursive declaration cycles.
function evaluateSymbol(symbol, state) {
  if (!symbol) return null;
  if (state.bindings.has(symbol)) return state.bindings.get(symbol);
  if (state.symbolStack.has(symbol)) return null;
  state.symbolStack.add(symbol);
  const values = [];
  for (const declaration of symbol.declarations ?? []) {
    const initializer = declaration.initializer;
    if (!initializer) continue;
    values.push(...(evaluateExpression(initializer, state) ?? []));
  }
  state.symbolStack.delete(symbol);
  return values.length > 0 ? uniqueStrings(values) : null;
}

// objectLiteralFor resolves the const object behind property and indexed GraphQL operation maps.
function objectLiteralFor(node, state) {
  const current = unwrapExpression(node);
  if (ts.isObjectLiteralExpression(current)) return current;
  if (!ts.isIdentifier(current)) return null;
  const symbol = symbolFor(current, state.checker);
  for (const declaration of symbol?.declarations ?? []) {
    if (!declaration.initializer) continue;
    const initializer = unwrapExpression(declaration.initializer);
    if (ts.isObjectLiteralExpression(initializer)) return initializer;
  }
  return null;
}

// propertyName returns only statically named object properties that can safely select GraphQL variants.
function propertyName(property) {
  const name = property.name;
  if (!name) return null;
  if (ts.isIdentifier(name) || ts.isStringLiteral(name) || ts.isNumericLiteral(name)) return name.text;
  return null;
}

// objectPropertyValues evaluates matching operation-map entries without executing UI code.
function objectPropertyValues(object, keys, state) {
  const values = [];
  for (const property of object.properties) {
    if (!ts.isPropertyAssignment(property) && !ts.isShorthandPropertyAssignment(property)) continue;
    const name = propertyName(property);
    if (!name || (keys && !keys.includes(name))) continue;
    const expression = ts.isShorthandPropertyAssignment(property) ? property.name : property.initializer;
    values.push(...(evaluateExpression(expression, state) ?? []));
  }
  return values.length > 0 ? uniqueStrings(values) : null;
}

// staticStringKeys accepts values proven by syntax and deliberately ignores TypeScript assertions about runtime strings.
function staticStringKeys(node, state) {
  try {
    return evaluateExpression(node, state);
  } catch {
    // Why: an `as` assertion is not runtime evidence; all map entries must be validated when interpolation is unresolved.
    return null;
  }
}

// evaluatePropertyAccess resolves named entries such as PEOPLE_OPERATIONS.list through their object declaration.
function evaluatePropertyAccess(node, state) {
  const object = objectLiteralFor(node.expression, state);
  if (object) return objectPropertyValues(object, [node.name.text], state);
  return evaluateSymbol(symbolFor(node.name, state.checker), state);
}

// evaluateElementAccess expands every possible statically typed map key so dynamic operation selection stays fail closed.
function evaluateElementAccess(node, state) {
  const object = objectLiteralFor(node.expression, state);
  if (!object || !node.argumentExpression) return null;
  return objectPropertyValues(object, staticStringKeys(node.argumentExpression, state), state);
}

// directReturns collects only returns owned by this function, excluding nested callbacks.
function directReturns(body) {
  const returns = [];
  // visit stops at nested functions because their returns do not belong to the helper being evaluated.
  const visit = (node) => {
    if (node !== body && ts.isFunctionLike(node)) return;
    if (ts.isReturnStatement(node) && node.expression) returns.push(node.expression);
    ts.forEachChild(node, visit);
  };
  visit(body);
  return returns;
}

// functionDeclarationFor identifies static local helpers such as serviceDetailQuery and mutation builders.
function functionDeclarationFor(expression, state) {
  const symbol = symbolFor(expression, state.checker);
  return (symbol?.declarations ?? []).find((declaration) => ts.isFunctionLike(declaration) && declaration.body) ?? null;
}

// bindFunctionArguments creates an isolated parameter environment for a statically evaluated helper call.
function bindFunctionArguments(declaration, args, state) {
  const bindings = new Map(state.bindings);
  declaration.parameters.forEach((parameter, index) => {
    const symbol = symbolFor(parameter.name, state.checker);
    const values = args[index] ? evaluateExpression(args[index], state) : null;
    if (symbol && values) bindings.set(symbol, values);
  });
  return { ...state, bindings };
}

// evaluateCall evaluates pure string-building helpers by their return expressions and never runs application code.
function evaluateCall(node, state) {
  const declaration = functionDeclarationFor(node.expression, state);
  if (!declaration?.body) return null;
  const callState = bindFunctionArguments(declaration, node.arguments, state);
  if (!ts.isBlock(declaration.body)) return evaluateExpression(declaration.body, callState);
  const values = directReturns(declaration.body).flatMap((expression) => evaluateExpression(expression, callState) ?? []);
  return values.length > 0 ? uniqueStrings(values) : null;
}

// evaluateBinary supports the string concatenation form used by occasional shared GraphQL fragments.
function evaluateBinary(node, state) {
  if (node.operatorToken.kind !== ts.SyntaxKind.PlusToken) return null;
  const left = evaluateExpression(node.left, state);
  const right = evaluateExpression(node.right, state);
  if (!left || !right) return null;
  return appendVariants(left, right);
}

// evaluateCompoundExpression handles branching and helper expressions after atomic resolution has failed.
function evaluateCompoundExpression(node, state) {
  if (ts.isConditionalExpression(node)) return uniqueStrings([...(evaluateExpression(node.whenTrue, state) ?? []), ...(evaluateExpression(node.whenFalse, state) ?? [])]);
  if (ts.isCallExpression(node)) return evaluateCall(node, state);
  if (ts.isBinaryExpression(node)) return evaluateBinary(node, state);
  return null;
}

// evaluateExpression is the fail-closed dispatcher for the small static-expression language used by UI documents.
function evaluateExpression(node, state) {
  const current = unwrapExpression(node);
  const literal = literalString(current);
  if (literal !== null) return [literal];
  if (ts.isTemplateExpression(current)) return evaluateTemplate(current, state);
  if (ts.isIdentifier(current)) return evaluateSymbol(symbolFor(current, state.checker), state);
  if (ts.isPropertyAccessExpression(current)) return evaluatePropertyAccess(current, state);
  if (ts.isElementAccessExpression(current)) return evaluateElementAccess(current, state);
  return evaluateCompoundExpression(current, state);
}

// transportEndpoint maps the two UI-owned clients onto the schemas that validate their documents.
function transportEndpoint(node) {
  if (!ts.isCallExpression(node) || !ts.isPropertyAccessExpression(node.expression)) return "";
  if (node.expression.name.text === "graphql") return "registry";
  if (node.expression.name.text === "mcpGraphql") return "engine";
  return "";
}

// sourceLocation gives scanner failures a stable file and line without leaking absolute checkout paths.
function sourceLocation(sourceFile, node, appRoot) {
  const line = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line + 1;
  return `${path.relative(appRoot, sourceFile.fileName).split(path.sep).join("/")}:${line}`;
}

// directGraphQLBypass detects statically resolvable browser fetches that evade the two schema-owned transport methods.
function directGraphQLBypass(node, state) {
  if (!ts.isCallExpression(node) || !ts.isIdentifier(node.expression) || node.expression.text !== "fetch") return false;
  if (!node.arguments[0]) return false;
  let targets;
  try {
    targets = evaluateExpression(node.arguments[0], state);
  } catch {
    return false;
  }
  return targets?.some((target) => /\/graphql(?:\b|\/|\?)/i.test(target)) ?? false;
}

// evaluateCallDocuments attaches the owning UI call site to otherwise context-free static evaluation failures.
function evaluateCallDocuments(node, context, sourceFile) {
  try {
    return node.arguments[0] ? evaluateExpression(node.arguments[0], context.state) : null;
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new Error(`${sourceLocation(sourceFile, node, context.appRoot)}: ${message}`);
  }
}

// scanSourceFile extracts each transport call in source order and proves that every call yields a document.
function scanSourceFile(sourceFile, context) {
  const calls = [];
  const documents = [];
  let callIndex = 0;
  // visit accounts for calls and bypasses in one traversal so their source coordinates cannot drift.
  const visit = (node) => {
    if (directGraphQLBypass(node, context.state)) {
      throw new Error(`${sourceLocation(sourceFile, node, context.appRoot)}: direct GraphQL transport bypasses api.graphql or api.mcpGraphql`);
    }
    const endpoint = transportEndpoint(node);
    if (endpoint) {
      callIndex += 1;
      const values = evaluateCallDocuments(node, context, sourceFile);
      if (!values?.length) throw new Error(`${sourceLocation(sourceFile, node, context.appRoot)}: unresolved GraphQL document expression`);
      const file = path.relative(context.appRoot, sourceFile.fileName).split(path.sep).join("/");
      calls.push({ endpoint, file, call: callIndex, document_count: values.length });
      values.forEach((document, index) => documents.push({ endpoint, file, call: callIndex, variant: index + 1, document }));
    }
    ts.forEachChild(node, visit);
  };
  visit(sourceFile);
  return { calls, documents };
}

// filterScan creates an endpoint-owned subset while retaining original per-file call coordinates.
function filterScan(scan, endpoint) {
  if (!endpoint) return scan;
  const calls = scan.calls.filter((call) => call.endpoint === endpoint);
  const documents = scan.documents.filter((document) => document.endpoint === endpoint);
  return { call_count: calls.length, document_count: documents.length, calls, documents };
}

// scanProgram merges deterministic file results and applies an optional endpoint boundary.
function scanProgram(program, files, appRoot, options) {
  const checker = program.getTypeChecker();
  const scan = { call_count: 0, document_count: 0, calls: [], documents: [] };
  const displayRoot = appRoot === defaultAppRoot ? uiRoot : appRoot;
  for (const file of files) {
    const sourceFile = program.getSourceFile(file);
    if (!sourceFile) throw new Error(`TypeScript omitted source file: ${file}`);
    const state = { checker, bindings: new Map(), symbolStack: new Set(), allowRuntimeScalarInterpolation: options.allowRuntimeScalarInterpolation };
    const result = scanSourceFile(sourceFile, { appRoot: displayRoot, state });
    scan.calls.push(...result.calls);
    scan.documents.push(...result.documents);
  }
  scan.call_count = scan.calls.length;
  scan.document_count = scan.documents.length;
  return filterScan(scan, options.endpoint);
}

// serializeScan owns the final newline so scanner stdout and checked-in manifests compare byte for byte.
function serializeScan(scan) {
  return `${JSON.stringify(scan, null, 2)}\n`;
}

// writeManifests updates both repository-owned contracts from one full scan.
function writeManifests(fullScan) {
  fs.mkdirSync(path.dirname(manifestPath), { recursive: true });
  fs.writeFileSync(manifestPath, serializeScan(fullScan));
  if (fs.existsSync(path.dirname(registryManifestPath))) {
    fs.writeFileSync(registryManifestPath, serializeScan(filterScan(fullScan, "registry")));
  }
}

// main performs provider-free extraction and never executes imported UI modules.
function main() {
  const options = parseOptions(process.argv.slice(2));
  const appRoot = path.resolve(process.env.FUSED_UI_GRAPHQL_APP_ROOT || defaultAppRoot);
  const files = collectSourceFiles(appRoot);
  const program = createGraphQLProgram(files);
  const fullScan = scanProgram(program, files, appRoot, { ...options, endpoint: "" });
  if (options.writeManifest) writeManifests(fullScan);
  process.stdout.write(serializeScan(filterScan(fullScan, options.endpoint)));
}

try {
  main();
} catch (error) {
  fail(error instanceof Error ? error.message : String(error));
}
