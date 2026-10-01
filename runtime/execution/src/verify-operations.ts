import path from "node:path";
import ts from "typescript";
import type { SelectedOperation } from "./bundle";

// Admit finite literal targets only; arbitrary runtime strings cannot be verified before execution.
function literalTargets(type: ts.Type): string[] {
  if (type.isStringLiteral()) return [type.value];
  // Every union member must identify a concrete target rather than widening to unrestricted strings.
  if (type.isUnion() && type.types.every((item) => item.isStringLiteral())) return type.types.map((item) => (item as ts.StringLiteralType).value);
  return [];
}

// Preserve inline literal types that TypeScript widens under the general fetch request interface.
function requestTargets(checker: ts.TypeChecker, argument: ts.Expression, key: string): string[] {
  // Spreads can replace earlier fields, so their final type must supply the proof instead of an earlier initializer.
  if (ts.isObjectLiteralExpression(argument) && !argument.properties.some(ts.isSpreadAssignment)) {
    const property = argument.properties.find((item) => item.name?.getText().replace(/["']/g, "") === key);
    // Named initializers retain literal identity even when the surrounding request is contextually typed.
    if (property && ts.isPropertyAssignment(property)) return literalTargets(checker.getTypeAtLocation(property.initializer));
  }
  const property = checker.getPropertyOfType(checker.getTypeAtLocation(argument), key);
  // Variables are verifiable when their request type preserves exact literal fields, for example with as const.
  return property ? literalTargets(checker.getTypeOfSymbolAtLocation(property, argument)) : [];
}

// Match the pinned runtime's request signature, including aliased and connected-user fetch functions.
function isRuntimeFetch(checker: ts.TypeChecker, call: ts.CallExpression): boolean {
  const declaration = checker.getResolvedSignature(call)?.getDeclaration();
  const runtime = path.resolve(__dirname, "../../src/index.ts");
  // Local helper methods named fetch must not be mistaken for Engine operation dispatch.
  if (!declaration || declaration.getSourceFile().fileName !== runtime || declaration.parameters.length !== 1) return false;
  return checker.typeToString(checker.getTypeAtLocation(declaration.parameters[0])) === "WorkspaceOperationRequest";
}

// Reject any low-level target not admitted by the same exact service/operation pairs as generated methods.
function verifyFetch(checker: ts.TypeChecker, call: ts.CallExpression, allowed: Set<string>): void {
  const service = requestTargets(checker, call.arguments[0], "service");
  const operation = requestTargets(checker, call.arguments[0], "operation");
  // Dynamic names need the selected SDK-style methods to retain pre-execution verification.
  if (!service.length || !operation.length) throw new Error("Cannot verify dynamic fused.fetch target. Use a selected SDK-style method or literal service and operation names.");
  for (const name of service) {
    for (const method of operation) {
      // Service and operation membership must be checked together, never as independent allowlists.
      if (!allowed.has(`${name}\u0000${method}`)) throw new Error(`Service/operation verification failed: ${name}.${method} is not selected.`);
    }
  }
}

// Generated method names are type-checked; this closes the equivalent low-level fetch authoring path.
export function verifyOperationCalls(program: ts.Program, entry: string, operations: readonly SelectedOperation[]): void {
  const checker = program.getTypeChecker();
  const allowed = new Set(operations.map((item) => `${item.service}\u0000${item.operation}`));
  // Visit authored code only; generated bindings deliberately implement the verified methods with a shared fetch bridge.
  function visit(node: ts.Node): void {
    if (ts.isCallExpression(node) && isRuntimeFetch(checker, node)) verifyFetch(checker, node, allowed);
    ts.forEachChild(node, visit);
  }
  visit(program.getSourceFile(entry)!);
}
