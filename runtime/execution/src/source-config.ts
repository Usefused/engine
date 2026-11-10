import ts from "typescript";

// Retired authoring syntax must pass through the one-time migration before compilation.
export function validateAppMetadata(source: ts.SourceFile): void {
 for (const statement of source.statements) {
  // Only exported metadata declarations are retired; provider payloads may legitimately contain the same field name.
  if (!ts.isVariableStatement(statement)) continue;
  const exported = statement.modifiers?.some(/** Only module exports can declare the retired metadata authority. */ (modifier) => modifier.kind === ts.SyntaxKind.ExportKeyword);
  // Local provider data can use this identifier without declaring app deployment metadata.
  if (!exported) continue;
  const old = statement.declarationList.declarations.some(/** Inspect syntax only, without evaluating authored initializers. */ (declaration) => ts.isIdentifier(declaration.name) && declaration.name.text === "fusedConfig");
  // Reject the old declaration even when a config property is also present.
  if (old) throw new Error("fusedConfig is no longer supported. Migrate metadata into buildUnifiedApp({ config: { ... } }).");
 }
}
