import { createElement } from "react";
import type { SecretMeta } from "../../lib/api";

/** Displays persisted storage keys, including credential pairs, without deriving scheme names or exposing values. */
export function StoredSecretKeys({ secret }: { secret: Pick<SecretMeta, "key_name" | "key_names"> }) {
  // Grouped credentials carry their authoritative keys together; older singular rows retain their exact key.
  const names = secret.key_names?.length ? secret.key_names : [secret.key_name];
  return createElement("div", { className: "mt-2 min-w-0 text-xs" },
    createElement("p", { className: "text-slate-500" }, "Stored under"),
    ...names.map((name, index) =>
      // Missing storage metadata must never be reconstructed from the credential type.
      createElement("code", { key: index, className: "block select-text break-all font-mono text-slate-700" }, name || "Not provided")),
  );
}
