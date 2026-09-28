import fs from "node:fs";
import vm from "node:vm";

// Evaluate bundle declarations in a bounded local child process without a host bridge.
function evaluateManifest(code: string): unknown {
  const context: Record<string, unknown> = Object.create(null);
  vm.runInNewContext(code, context, { timeout: 5000, microtaskMode: "afterEvaluate" });
  return context.FusedExecutionManifest;
}

// Emit only the public manifest so the parent can validate it before writing artifacts.
const code = fs.readFileSync(0, "utf8");
const manifest = evaluateManifest(code);
process.stdout.write(JSON.stringify(manifest));
