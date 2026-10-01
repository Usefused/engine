#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { buildExecutionBundle, BundleSpec, inspectExecutionBundle } from "./bundle";
import { generateExecutionClientSource } from "./client_generator";
import { generateExecutionBindingsDeclaration } from "./bindings";

interface CliPaths {
  config: string;
  out: string;
  manifest: string;
  digest: string;
  client?: string;
  bindings?: string;
  bundleOnly: boolean;
  inspector: string;
}

const BUILD_FLAGS = new Set(["--config", "--out", "--manifest", "--digest", "--client", "--bindings", "--bundle-only", "--inspector"]);
const USAGE = "Usage: fused-unified-app-build --config spec.json --out bundle.js (--bundle-only true | --manifest manifest.json) [--inspector /path/to/fused-engine] [--digest digest.json] [--client client.ts] [--bindings operations.d.ts]";

// Each flag can claim one value so output paths remain unambiguous.
function validFlagPair(flag: string, value: string | undefined, options: Record<string, string>): boolean {
  return BUILD_FLAGS.has(flag) && !!value && !options[flag];
}

// Require explicit artifact destinations so a build never overwrites source files by default.
function parseArgs(args: string[]): CliPaths {
  const options: Record<string, string> = {};
  // Each supported flag consumes exactly one path value.
  for (let index = 0; index < args.length; index += 2) {
    const flag = args[index];
    // Unknown or unpaired arguments should fail before any file is written.
    if (!validFlagPair(flag, args[index + 1], options)) {
      throw new Error(USAGE);
    }
    options[flag] = args[index + 1];
  }
  // A complete command must provide each required artifact path once.
  if (!options["--config"] || !options["--out"]) {
    throw new Error(USAGE);
  }
  const bundleOnly = validateBuildMode(options);
  const out = path.resolve(options["--out"]);
  const manifest = path.resolve(options["--manifest"] ?? out + ".manifest.json");
  const digest = path.resolve(options["--digest"] ?? out + ".digest.json");
  const client = optionalPath(options["--client"]);
  const bindings = optionalPath(options["--bindings"]);
  const outputs = [out, manifest, digest, client, bindings].filter((file): file is string => !!file);
  // Separate artifacts prevent a generated file from overwriting another output.
  if (new Set(outputs).size !== outputs.length) {
    throw new Error("Build artifact paths must differ");
  }
  return { config: path.resolve(options["--config"]), out, manifest, digest, client, bindings, bundleOnly, inspector: options["--inspector"] ?? "fused-engine" };
}

// Keep absent optional outputs absent rather than resolving them to the working directory.
function optionalPath(value: string | undefined): string | undefined {
  return value ? path.resolve(value) : undefined;
}

// Compile-only mode cannot accidentally request artifacts that require authored code evaluation.
function validateBuildMode(options: Record<string, string>): boolean {
  // A typo must not silently select a less restrictive execution path.
  if (options["--bundle-only"] !== undefined && options["--bundle-only"] !== "true") throw new Error(USAGE);
  const bundleOnly = options["--bundle-only"] === "true";
  // Only the confined Engine inspector may produce a manifest or a typed REST client.
  if (bundleOnly ? !!options["--manifest"] || !!options["--client"] : !options["--manifest"]) throw new Error(USAGE);
  return bundleOnly;
}

// Delegate all authored declaration evaluation to Engine's OS-confined worker; never use node:vm.
function evaluateInChild(code: string, inspector: string): Promise<unknown> {
  const result = spawnSync(inspector, ["inspect-unified-app-bundle"], {
    input: code,
    encoding: "utf8",
    timeout: 30000,
    maxBuffer: 1024 * 1024,
    env: { PATH: process.env.PATH },
  });
  // Failed evaluation must not produce a deployable manifest artifact.
  if (result.error || result.status !== 0) {
    throw new Error("Confined manifest inspection failed. Install the Fused Engine and its execution worker on a supported host, or submit source to Engine planning.");
  }
  return Promise.resolve(JSON.parse(result.stdout));
}

// Build from an exact selection spec, then persist only validated artifacts.
async function main(): Promise<void> {
  const paths = parseArgs(process.argv.slice(2));
  const config = JSON.parse(fs.readFileSync(paths.config, "utf8")) as BundleSpec;
  // The entry path is resolved relative to its spec for reproducible local builds.
  if (typeof config.entryFile !== "string" || !Array.isArray(config.selectedOperations)) {
    throw new Error("Build spec requires entryFile and selectedOperations");
  }
  config.entryFile = path.resolve(path.dirname(paths.config), config.entryFile);
  const bundle = await buildExecutionBundle(config);
  // Hosted planning only compiles here; its Go parent inspects the bytes inside OS confinement.
  const manifest = paths.bundleOnly ? undefined : await inspectExecutionBundle(bundle.code, (code) => evaluateInChild(code, paths.inspector));
  const clientSource = paths.client && manifest ? generateExecutionClientSource(manifest) : undefined;
  // Local declarations guide authoring but do not alter the Engine's ID-only manifest.
  const bindingsSource = paths.bindings ? generateExecutionBindingsDeclaration(config.selectedOperations) : undefined;
  writeBuildArtifacts(paths, bundle, manifest, clientSource, bindingsSource);
}

// Persist only the artifacts produced by the selected compile or confined inspection mode.
function writeBuildArtifacts(paths: CliPaths, bundle: {code: string; bundleDigest: string}, manifest: unknown, clientSource?: string, bindingsSource?: string): void {
  fs.mkdirSync(path.dirname(paths.out), { recursive: true });
  fs.mkdirSync(path.dirname(paths.digest), { recursive: true });
  fs.writeFileSync(paths.out, bundle.code);
  // No manifest may be emitted before the confined worker returns a valid declaration.
  if (manifest) {
    fs.mkdirSync(path.dirname(paths.manifest), { recursive: true });
    fs.writeFileSync(paths.manifest, JSON.stringify(manifest, null, 2) + "\n");
  }
  fs.writeFileSync(paths.digest, JSON.stringify({ bundle_digest: bundle.bundleDigest }, null, 2) + "\n");
  // The generated client remains an external REST adapter over this bundle.
  if (paths.client && clientSource) {
    fs.mkdirSync(path.dirname(paths.client), { recursive: true });
    fs.writeFileSync(paths.client, clientSource);
  }
  // Emit the typed selected-operation surface beside the authored source when requested.
  if (paths.bindings && bindingsSource) {
    fs.mkdirSync(path.dirname(paths.bindings), { recursive: true });
    fs.writeFileSync(paths.bindings, bindingsSource);
  }
}

// Surface a concise local build error and a nonzero exit code to the caller.
main().catch((error: unknown) => {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
  process.exitCode = 1;
});
