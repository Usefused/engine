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
}

const BUILD_FLAGS = new Set(["--config", "--out", "--manifest", "--digest", "--client", "--bindings"]);
const USAGE = "Usage: fused-execution-build --config spec.json --out bundle.js --manifest manifest.json [--digest digest.json] [--client client.ts] [--bindings operations.d.ts]";

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
  if (!options["--config"] || !options["--out"] || !options["--manifest"]) {
    throw new Error(USAGE);
  }
  const out = path.resolve(options["--out"]);
  const manifest = path.resolve(options["--manifest"]);
  const digest = path.resolve(options["--digest"] ?? out + ".digest.json");
  const client = options["--client"] ? path.resolve(options["--client"]) : undefined;
  const bindings = options["--bindings"] ? path.resolve(options["--bindings"]) : undefined;
  const outputs = [out, manifest, digest, client, bindings].filter((file): file is string => !!file);
  // Separate artifacts prevent a generated file from overwriting another output.
  if (new Set(outputs).size !== outputs.length) {
    throw new Error("Build artifact paths must differ");
  }
  return { config: path.resolve(options["--config"]), out, manifest, digest, client, bindings };
}

// Ask a memory-limited child to evaluate declarations without Engine effects.
function evaluateInChild(code: string): Promise<unknown> {
  const worker = path.join(__dirname, "manifest-worker.js");
  // The parent deadline leaves startup margin around the worker's five-second VM limit.
  const result = spawnSync(process.execPath, ["--max-old-space-size=128", worker], {
    input: code,
    encoding: "utf8",
    timeout: 15000,
    maxBuffer: 1024 * 1024,
    env: {},
  });
  // Failed evaluation must not produce a deployable manifest artifact.
  if (result.error || result.status !== 0) {
    throw new Error(`Manifest evaluation failed: ${result.error?.message ?? result.stderr.trim()}`);
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
  const manifest = await inspectExecutionBundle(bundle.code, evaluateInChild);
  const clientSource = paths.client ? generateExecutionClientSource(manifest) : undefined;
  // Local declarations guide authoring but do not alter the Engine's ID-only manifest.
  const bindingsSource = paths.bindings ? generateExecutionBindingsDeclaration(config.selectedOperations) : undefined;
  fs.mkdirSync(path.dirname(paths.out), { recursive: true });
  fs.mkdirSync(path.dirname(paths.manifest), { recursive: true });
  fs.mkdirSync(path.dirname(paths.digest), { recursive: true });
  fs.writeFileSync(paths.out, bundle.code);
  fs.writeFileSync(paths.manifest, JSON.stringify(manifest, null, 2) + "\n");
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
