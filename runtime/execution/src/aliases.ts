// These names belong to the runtime facade or JavaScript object machinery, never provider aliases.
const RESERVED = new Set(["fetch", "callApp", "db", "forUserRef", "forServiceUserRefs", "constructor", "prototype", "__proto__", "then", "toString", "valueOf", "hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable", "toLocaleString"]);

// Normalize only presentation names; the generated method retains the exact dispatch identity.
export function operationIdentifier(value: string, prefix: string): string {
  const words = value.match(/[A-Za-z0-9]+/g) ?? [];
  // Names without ASCII identifier characters still need an explicit, callable alias.
  if (words.length === 0) return prefix;
  // Preserve provider casing within words while choosing lower camel case at word boundaries.
  const name = words.map((word, index) => index === 0 ? word[0].toLowerCase() + word.slice(1) : word[0].toUpperCase() + word.slice(1)).join("");
  // A digit cannot start a dot-accessible TypeScript identifier.
  return /^[0-9]/.test(name) ? prefix + name : name;
}

// Assign collision-safe names independently of discovery order, matching Registry's advertised call map.
export function operationAliases(values: readonly string[], service: boolean): Map<string, string> {
  const aliases = new Map<string, string>();
  const used = new Set(RESERVED);
  const ordered = [...new Set(values)].sort((left, right) => Buffer.compare(Buffer.from(left), Buffer.from(right)));
  for (const value of ordered) {
    // Qualified service names display their slug; exact provider identity stays in the binding.
    const display = service ? value.slice(value.lastIndexOf("/") + 1) : value;
    const base = operationIdentifier(display, service ? "service" : "operation");
    let alias = base;
    let suffix = 2;
    // Reserved or colliding names receive deterministic suffixes without overwriting a sibling.
    while (used.has(alias)) alias = `${base}_${suffix++}`;
    used.add(alias);
    aliases.set(value, alias);
  }
  return aliases;
}
