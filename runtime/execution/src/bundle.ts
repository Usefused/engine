import path from "node:path";
import fs from "node:fs";
import { createHash } from "node:crypto";
import { build } from "esbuild";
import { validateSearchablePaths } from "./index";
import { generateExecutionBindingsSource } from "./bindings";

export interface ExactOperationBinding {
  service: string;
  operation: string;
  serviceId: string;
  serviceVersionId: string;
  endpointId: string;
}

export interface SelectedOperation extends ExactOperationBinding {
  inputSchema?: Record<string, unknown>;
  outputSchema?: Record<string, unknown>;
}

export interface BundleSpec {
  entryFile: string;
  selectedOperations: readonly SelectedOperation[];
}

export interface ExecutionBundleManifest {
  schemaVersion: 1;
  inputSchema: Record<string, unknown>;
  outputSchema: Record<string, unknown>;
  searchable: readonly string[];
  selectedOperations: readonly ExactOperationBinding[];
}

export interface ExecutionBundle {
  code: string;
  bundleDigest: string;
}

export type ManifestEvaluator = (code: string) => Promise<unknown>;

const UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
const AUTHOR_IMPORTS = new Set(["@fused/unified-app", "@fused/operations", "zod", "zod/mini"]);

// Validate one exact endpoint and its optional local typing hints before bundling.
function validateSelection(operation: SelectedOperation): void {
  // Incomplete bindings must fail before a bundle can reach Engine apply.
  if (!operation || ![operation.service, operation.operation, operation.serviceId, operation.serviceVersionId, operation.endpointId].every((field) => typeof field === "string" && field.trim().length > 0)) {
    throw new Error("Unified App contains an incomplete selected operation");
  }
  // Engine decodes each target ID as a UUID at its physical dispatch boundary.
  if (![operation.serviceId, operation.serviceVersionId, operation.endpointId].every((identifier) => UUID.test(identifier))) {
    throw new Error("Unified App contains a non-UUID selected operation ID");
  }
  // Reviewed input contracts must preserve the provider parameter object required by Engine.
  if (operation.inputSchema !== undefined && (!isRecord(operation.inputSchema) || operation.inputSchema.type !== "object")) {
    throw new Error("Unified App operation input schema must be a JSON object");
  }
  // Output typing may represent any JSON response root, but never executable values.
  if (operation.outputSchema !== undefined && !isRecord(operation.outputSchema)) {
    throw new Error("Unified App operation output schema must be a JSON Schema object");
  }
}

// Require every author-facing operation key to resolve to one immutable Engine endpoint.
function validateSelections(operations: readonly SelectedOperation[]): void {
  const seen = new Set<string>();
  // Every hosted version must bind at least one admitted workspace operation.
  if (operations.length === 0) {
    throw new Error("Unified App requires at least one selected operation");
  }
  // Engine admission caps each immutable app version at 64 bound endpoints.
  if (operations.length > 64) {
    throw new Error("Unified App supports at most 64 selected operations");
  }
  // Every declared provider target must remain pinned across execution and replay.
  for (const operation of operations) {
    validateSelection(operation);
    const key = `${operation.service}\u0000${operation.operation}`;
    // Duplicate author keys would make fused.fetch routing ambiguous.
    if (seen.has(key)) {
      throw new Error(`Unified App selects ${operation.service}.${operation.operation} more than once`);
    }
    seen.add(key);
  }
}

// Keep local typing hints outside the public manifest and Engine authorization binding.
function exactSelections(operations: readonly SelectedOperation[]): ExactOperationBinding[] {
  return operations.map(({ service, operation, serviceId, serviceVersionId, endpointId }) => ({ service, operation, serviceId, serviceVersionId, endpointId }));
}

// Bundle one default-exported builder and pinned Zod into a self-contained worker script.
export async function buildExecutionBundle(spec: BundleSpec): Promise<ExecutionBundle> {
  validateSelections(spec.selectedOperations);
  const entryFile = path.resolve(spec.entryFile);
  const entryRealPath = fs.realpathSync(entryFile);
  // Bundle the ESM-capable source so esbuild can remove unused Zod/runtime branches.
  const runtimeFile = path.resolve(__dirname, "../../src/index.ts");
  const selections = JSON.stringify(exactSelections(spec.selectedOperations));
  const bindingSource = generateExecutionBindingsSource(spec.selectedOperations);
  const source = `
import authored from ${JSON.stringify(entryFile)};
import { toPublicSchema } from ${JSON.stringify(runtimeFile)};
const app = authored;
// A missing builder contract cannot become the one app-level execute operation.
if (!app || !app.input || !app.output || !app.fetch || typeof app.execute !== "function") {
  throw new Error("Missing default Unified App export");
}
const inputSchema = toPublicSchema(app.input);
const outputSchema = toPublicSchema(app.output);
const searchable = app.fetch.searchable;
const selectedOperations = ${selections};
globalThis.FusedUnifiedApp = {
  input: app.input,
  output: app.output,
  fetch: { searchable },
  selectedOperations,
  // The Engine runner parses both Zod boundaries around authored execution.
  execute: ({ input }) => app.execute({ input })
};
globalThis.FusedExecutionManifest = { schemaVersion: 1, inputSchema, outputSchema, searchable, selectedOperations };
`;
  const result = await build({
    stdin: { contents: source, resolveDir: path.dirname(entryFile), sourcefile: "fused-execution-entry.ts", loader: "ts" },
    bundle: true,
    write: false,
    format: "iife",
    platform: "neutral",
    target: "es2020",
    // Remove unused Zod/runtime branches and shorten the isolated payload without changing its contract.
    minify: true,
    treeShaking: true,
    // Inline maps let the isolated interpreter report authored TypeScript line numbers.
    sourcemap: "inline",
    sourcesContent: false,
    // Author imports resolve to the pinned runtime and Zod versions in this package.
    alias: {
      "@fused/unified-app": runtimeFile,
      "zod/v4/core": path.join(path.dirname(require.resolve("zod/mini")), "../v4/core/index.js"),
      "zod/mini": path.join(path.dirname(require.resolve("zod/mini")), "index.js"),
      "zod": path.join(path.dirname(require.resolve("zod")), "index.js"),
    },
    plugins: [{
      name: "fused-author-imports",
      setup(builder) {
        // Engine compiles tenant-authored source, so only pinned runtime modules may be read from its filesystem.
        builder.onResolve({ filter: /.*/ }, (args) => {
          // The synthetic entry and pinned runtime dependencies have their own trusted import graph.
          if (!args.importer || !fs.existsSync(args.importer) || fs.realpathSync(args.importer) !== entryRealPath) {
            return undefined;
          }
          // Relative, absolute, Node, and arbitrary package imports could copy Engine files into a bundle.
          if (!AUTHOR_IMPORTS.has(args.path)) {
            return { errors: [{ text: `Unified App source cannot import ${args.path}` }] };
          }
          // This virtual method surface is generated from the exact Engine-selected operations.
          if (args.path === "@fused/operations") {
            return { path: "@fused/operations", namespace: "fused-operation-bindings" };
          }
          return undefined;
        });
        // Bundled methods call only author keys whose immutable IDs were pinned above.
        builder.onLoad({ filter: /.*/, namespace: "fused-operation-bindings" }, () => ({ contents: bindingSource, loader: "ts", resolveDir: path.dirname(entryFile) }));
      },
    }],
    logLevel: "silent",
    legalComments: "none",
  });
  // An immutable version owns exactly one executable JavaScript bundle.
  if (result.outputFiles.length !== 1) {
    throw new Error("Unified App bundler did not produce a single script");
  }
  const code = stableSourceMap(result.outputFiles[0].text, entryFile);
  // Plan and apply pin SHA-256 of the exact emitted UTF-8 bytes.
  const bundleDigest = "sha256:" + createHash("sha256").update(code, "utf8").digest("hex");
  return { code, bundleDigest };
}

// Distinguish public manifest records from arrays and executable values.
function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

// Validate the one public app contract before client generation or Engine persistence.
export function parseExecutionManifest(raw: unknown): ExecutionBundleManifest {
  // The app descriptor must contain both object contracts and its declared effects.
  if (!isRecord(raw) || raw.schemaVersion !== 1 || !isRecord(raw.inputSchema) || !isRecord(raw.outputSchema) || !Array.isArray(raw.searchable) || !Array.isArray(raw.selectedOperations)) {
    throw new Error("Invalid Unified App bundle manifest");
  }
  // REST and generated client arguments use named fields, not a scalar root.
  if (raw.inputSchema.type !== "object" || raw.outputSchema.type !== "object") {
    throw new Error("Unified App manifest schemas must be JSON objects");
  }
  const searchable = validateSearchablePaths(raw.searchable);
  const selectedOperations = raw.selectedOperations as SelectedOperation[];
  validateSelections(selectedOperations);
  return { schemaVersion: 1, inputSchema: raw.inputSchema, outputSchema: raw.outputSchema, searchable, selectedOperations: exactSelections(selectedOperations) };
}

// Let the caller's isolated build worker evaluate top-level code without invoking execute.
export async function inspectExecutionBundle(code: string, evaluator: ManifestEvaluator): Promise<ExecutionBundleManifest> {
  return parseExecutionManifest(await evaluator(code));
}

// Normalize build-machine paths so equivalent source produces one deterministic immutable bundle.
function stableSourceMap(code: string, entryFile: string): string {
  return code.replace(/(sourceMappingURL=data:application\/json;base64,)([^\s]+)/, (_match, prefix: string, encoded: string) => {
    const map = JSON.parse(Buffer.from(encoded, "base64").toString("utf8"));
    // Only stable filenames enter private stack traces; source text stays in the app configuration.
    map.sources = map.sources.map((source: string, index: number) => path.basename(source) === path.basename(entryFile) ? "unified-app.ts" : `fused-runtime/${index}/${path.basename(source)}`);
    return prefix + Buffer.from(JSON.stringify(map)).toString("base64");
  });
}
