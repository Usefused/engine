import path from "node:path";
import ts from "typescript";
import type { BundleSpec } from "./bundle";
import { generateExecutionBindingsDeclaration } from "./bindings";
import { verifyOperationCalls } from "./verify-operations";

// Reject reference directives before TypeScript can read files or ambient capabilities outside the pinned import graph.
function validateAuthorReferences(entry: string): void {
  const source = ts.sys.readFile(entry);
  // A missing authored entry cannot be substituted with a declaration-only program.
  if (source === undefined) throw new Error("Unified App source is unavailable");
  const parsed = ts.createSourceFile(entry, source, ts.ScriptTarget.ES2022, true);
  // Triple-slash references bypass module resolution, so import allowlisting alone is insufficient.
  if (parsed.referencedFiles.length || parsed.typeReferenceDirectives.length || parsed.libReferenceDirectives.length || parsed.hasNoDefaultLib) {
    throw new Error("Unified App source cannot use TypeScript reference directives");
  }
}

// Resolve author imports against the same pinned packages and selected operations used by the bundler.
function typecheckHost(entry: string, declarations: string, operations: string, options: ts.CompilerOptions): ts.CompilerHost {
  const runtime = path.resolve(__dirname, "../../src/index.ts");
  const host = ts.createCompilerHost(options);
  const readSource = host.getSourceFile.bind(host);
  // Generated declarations stay in memory and never overwrite an authored file.
  host.getSourceFile = (file, language, onError, fresh) => file === declarations
    ? ts.createSourceFile(file, operations, ts.ScriptTarget.ES2022, true)
    : readSource(file, language, onError, fresh);
  // Author imports cannot introduce local modules or ambient Node/browser capabilities.
  host.resolveModuleNames = (names, containing) => names.map((name) => {
    // Both virtual packages refer to Engine-owned definitions, not a caller's node_modules.
    if (name === "@fused/unified-app") return { resolvedFileName: runtime, extension: ts.Extension.Ts };
    if (name === "@fused/operations") return { resolvedFileName: declarations, extension: ts.Extension.Dts };
    // The worker supports only the pinned Zod authoring packages alongside Fused.
    if (containing === entry && name !== "zod" && name !== "zod/mini") throw new Error(`Unified App source cannot import ${name}`);
    return ts.resolveModuleName(name, containing === entry ? runtime : containing, options, host).resolvedModule;
  });
  return host;
}

// Report bounded, source-relative diagnostics without exposing compiler filesystem paths.
function diagnosticText(diagnostic: ts.Diagnostic): string {
  let location = "";
  // Global configuration errors have no authored source location.
  if (diagnostic.file && diagnostic.start !== undefined) {
    const position = diagnostic.file.getLineAndCharacterOfPosition(diagnostic.start);
    location = `app.ts:${position.line + 1}:${position.character + 1} `;
  }
  return `${location}TS${diagnostic.code}: ${ts.flattenDiagnosticMessageText(diagnostic.messageText, " ")}`;
}

// Reject semantic TypeScript errors and unselected operation targets before evaluating authored declarations.
export function validateExecutionSource(spec: BundleSpec): void {
  const entry = path.resolve(spec.entryFile);
  validateAuthorReferences(entry);
  const declarations = path.join(path.dirname(entry), ".fused-operation-types.d.ts");
  const options: ts.CompilerOptions = {
    noEmit: true, strict: true, skipLibCheck: true, types: [], lib: ["lib.es2022.d.ts"],
    target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, moduleResolution: ts.ModuleResolutionKind.Bundler,
  };
  const host = typecheckHost(entry, declarations, generateExecutionBindingsDeclaration(spec.selectedOperations), options);
  const program = ts.createProgram([entry, declarations], options, host);
  const diagnostics = ts.getPreEmitDiagnostics(program);
  // Failed type checks must never produce a deployable bundle or hide behind successful transpilation.
  if (diagnostics.length) throw new Error(`TypeScript validation failed:\n${diagnostics.slice(0, 8).map(diagnosticText).join("\n")}`);
  verifyOperationCalls(program, entry, spec.selectedOperations);
}
