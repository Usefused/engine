// Exclude examples without changing source offsets used by the static metadata reader.
const nonCode = /\/\/[^\r\n]*|\/\*[\s\S]*?\*\/|"(?:\\[\s\S]|[^"\\\r\n])*"|'(?:\\[\s\S]|[^'\\\r\n])*'|`(?:\\[\s\S]|[^`\\])*`/g;
interface ConfigLocation { start: number; end: number; appStart: number }

/** Keeps code and metadata boundaries independent of comments and quoted examples. */
function maskSource(source: string): string {
  return source.replace(nonCode, /** Preserve locations while hiding non-code contents. */ (match) => match.replace(/[^\r\n]/g, " "));
}

/** Finds one literal's boundary without interpreting executable TypeScript. */
function objectEnd(source: string, start: number): number {
  // Metadata must be static JSON so the browser and CLI never execute it to discover deployment scope.
  if (source[start] !== "{") throw new Error("Use a literal JSON object for config.");
  const masked = maskSource(source);
  let depth = 0;
  for (let index = start; index < source.length; index++) {
    // Quoted braces and comments have already been removed from the structural scan.
    if (masked[index] === "{") depth++;
    else if (masked[index] === "}") {
      depth--;
      // The initializer ends at its matching brace, not at the surrounding app definition.
      if (depth === 0) return index + 1;
    }
  }
  throw new Error("Complete the config JSON object in Code.");
}

/** Locates the default literal options object, excluding commented and quoted app examples. */
function appOptionsStart(masked: string): number {
  const apps = [...masked.matchAll(/\bexport\s+default\s+buildUnifiedApp\s*\(\s*\{/g)];
  // A single default literal gives code and form one unambiguous metadata owner.
  if (apps.length !== 1) throw new Error("Use one default buildUnifiedApp({ … }) declaration.");
  return apps[0].index! + apps[0][0].length;
}

/** Reads one direct option without allowing spreads to replace reviewed metadata at runtime. */
function directConfig(source: string, masked: string, index: number, appStart: number): ConfigLocation | undefined {
  // An options spread could override config after the browser has statically reviewed it.
  if (/^\s*\.\.\./.test(masked.slice(index))) throw new Error("Use explicit buildUnifiedApp properties instead of an options spread.");
  const property = /^\s*config\s*:\s*/.exec(masked.slice(index));
  // Other options belong to executable TypeScript and remain untouched.
  if (!property) return undefined;
  const start = index + property[0].length, end = objectEnd(source, start);
  // Expressions after an object would create metadata that cannot be represented by the form.
  if (!/^[\s]*[,}]/.test(masked.slice(end))) throw new Error("config must be a literal JSON object, not an expression.");
  return { start, end, appStart };
}

const delimiterDepth: Record<string, number> = { "{": 1, "[": 1, "(": 1, "}": -1, "]": -1, ")": -1 };

/** Visits only direct app options while skipping nested schema expressions and executable method bodies. */
function inlineLocations(source: string, masked: string, appStart: number): ConfigLocation[] {
  const found: ConfigLocation[] = [];
  let depth = 0, propertyStart = appStart;
  for (let index = appStart; index < masked.length; index++) {
    // Metadata inside input schemas or execute bodies cannot claim app-level authority.
    if (depth === 0 && index === propertyStart) {
      const location = directConfig(source, masked, index, appStart);
      // Only the named metadata property enters the candidate set.
      if (location) found.push(location);
    }
    const character = masked[index];
    // Stop at the default app's options boundary rather than scanning following source statements.
    if (depth === 0 && character === "}") break;
    depth += delimiterDepth[character] ?? 0;
    // Only root commas begin another option; inner commas belong to schemas and statements.
    if (depth === 0 && character === ",") propertyStart = index + 1;
  }
  return found;
}

/** Resolves only the current inline metadata authority; old declarations belong exclusively to the migration. */
function configLocation(source: string): ConfigLocation {
  const masked = maskSource(source), appStart = appOptionsStart(masked);
  // Reject old exports even beside valid config so no caller can keep two authoring authorities.
  if (/\bexport\s+const\s+fusedConfig\b/.test(masked)) throw new Error("fusedConfig is no longer supported. Migrate app metadata into buildUnifiedApp({ config: { … } }).");
  const found = inlineLocations(source, masked, appStart);
  // Multiple inline properties must never silently disagree about deployed app scope.
  if (found.length > 1) throw new Error("Keep exactly one config object inside buildUnifiedApp.");
  return found[0] ?? { start: appStart, end: appStart, appStart };
}

/** Reads source metadata without executing code or altering an incomplete editor buffer. */
export function readUnifiedAppSourceConfig(source: string): Record<string, unknown> {
  const location = configLocation(source);
  // Removing metadata is incomplete authoring, never a request to silently restore old form settings.
  if (location.start === location.end) throw new Error("Add config: { … } inside buildUnifiedApp.");
  return JSON.parse(source.slice(location.start, location.end));
}

/** Compares JSON recursively so ordinary body edits retain authored formatting and caret positions. */
function sameConfig(left: unknown, right: unknown): boolean {
  // Scalar identity terminates recursion without allocating another serialized document.
  if (left === right) return true;
  // JSON values of different kinds cannot represent the same setting.
  if (!left || !right || typeof left !== "object" || typeof right !== "object" || Array.isArray(left) !== Array.isArray(right)) return false;
  const keys = Object.keys(left);
  return keys.length === Object.keys(right).length && keys.every(/** Compare values independent of object property order. */ (key) => Object.hasOwn(right, key) && sameConfig((left as Record<string, unknown>)[key], (right as Record<string, unknown>)[key]));
}

/** Writes form changes into the app's metadata property while preserving all executable source. */
export function unifiedAppSourceView(source: string, metadata: Record<string, unknown>): { source: string; error: string } {
  // Pending generation must not become a deployable metadata-only file.
  if (!source.trim()) return { source, error: "" };
  try {
    const location = configLocation(source);
    const json = JSON.stringify(metadata, null, 2);
    // New and implementation-only drafts receive the canonical property inside their default app.
    if (location.start === location.end) return { source: source.slice(0, location.appStart) + `\n  config: ${json},` + source.slice(location.appStart), error: "" };
    const current = JSON.parse(source.slice(location.start, location.end));
    // Unchanged inline metadata keeps the user's spacing and native editor selection intact.
    if (sameConfig(current, metadata)) return { source, error: "" };
    return { source: source.slice(0, location.start) + json + source.slice(location.end), error: "" };
  } catch (cause) {
    // Incomplete metadata stays visible until corrected instead of being overwritten from the form.
    return { source, error: `Config could not be synchronized: ${String(cause)}` };
  }
}
