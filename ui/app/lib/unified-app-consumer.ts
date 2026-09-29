export type HostedAppReference = { app_id: string; kind: string; name: string; version: string };
export type UnifiedAppReference = { name: string; version: string };

const reservedAliases = new Set("false true null none self cls async await and as assert break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield constructor then execute __proto__".split(" "));

/** Creates a portable alias while keeping human-readable name and version as the declared identity. */
export function unifiedAppAlias(name: string): string {
  const alias = name.toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^_+|_+$/g, "").slice(0, 60);
  // Empty and digit-leading names still need valid cross-language method identifiers.
  return /^[a-z]/.test(alias) && !reservedAliases.has(alias) ? alias : `app_${alias}`;
}

/** Preserves saved consumer scope and attaches exactly one reviewed hosted version. */
export function composeUnifiedAppConsumer(fresh: Record<string, unknown>, source: Record<string, unknown> | undefined, attachment: HostedAppReference | null, alias: string): Record<string, unknown> {
  // Existing immutable configuration supplies all private routing, credentials, and operations.
  const config: Record<string, unknown> = source ? { ...source, version: fresh.version } : { ...fresh };
  // Ordinary consumer changes preserve their existing configuration path.
  if (!attachment) return config;
  // Public aliases must remain valid in both generated languages.
  if (attachment.kind !== "unified_app" || !validHostedCallName(alias)) throw new Error("Choose a valid Unified App alias.");
  const refs = { ...(config.unified_apps as Record<string, UnifiedAppReference> | undefined) };
  const previous = refs[alias];
  // An existing callable name must never be silently redirected to a different app.
  if (previous && (previous.name !== attachment.name || previous.version !== attachment.version)) throw new Error("This alias is already used. Choose another alias.");
  // Python adds companion methods, so neither side of that pair may be shadowed.
  if (hasSyncCollision(refs, alias)) throw new Error("This alias conflicts with a synchronous call name.");
  refs[alias] = { name: attachment.name, version: attachment.version };
  return { ...config, unified_apps: refs };
}

/** Shares cross-language alias admission without coupling it to config merge decisions. */
function validHostedCallName(alias: string): boolean {
  return /^[a-z][a-z0-9_]{0,63}$/.test(alias) && !reservedAliases.has(alias);
}

/** Protects generated Python companion methods while retaining the same alias in both adapters. */
function hasSyncCollision(refs: Record<string, UnifiedAppReference>, alias: string): boolean {
  return Boolean(refs[`${alias}_sync`] || (alias.endsWith("_sync") && refs[alias.slice(0, -5)]));
}
