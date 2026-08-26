import { createElement, useState } from "react";
import { Copy } from "lucide-react";

const authNameLabels = {
  service: { label: "Authentication scheme name", field: "auth.name" },
  bucket: { label: "Stored auth name", field: "auth_name" },
};

/** Copies only the exact scheme identifier; unavailable clipboard access never reports success. */
export async function copyAuthName(name: string, clipboard?: Pick<Clipboard, "writeText">): Promise<string> {
  // Missing names must remain missing instead of becoming a guessed auth selector.
  if (!name || !clipboard) return "Copy unavailable. Select the name to copy it manually.";
  try {
    await clipboard.writeText(name);
    return "Auth name copied.";
  } catch {
    // Browser permission failures are actionable locally without exposing raw error details.
    return "Copy failed. Select the name to copy it manually.";
  }
}

/** Service definitions are primary; bucket views identify the stored binding without implying ownership of the scheme. */
export function AuthNameField({ name, context = "service" }: { name?: string | null; context?: keyof typeof authNameLabels }) {
  const { label, field } = authNameLabels[context];
  const [copyResult, setCopyResult] = useState({ name, message: "" });
  // Copy is a local action; neither credentials nor scheme names enter tracking metadata.
  async function handleCopy() {
    setCopyResult({ name, message: await copyAuthName(name ?? "", navigator.clipboard) });
  }
  // Version or connection changes must not retain success feedback for a different identifier.
  const status = copyResult.name === name ? copyResult.message : "";
  // An absent legacy name cannot safely be inferred from type, user reference, or provider.
  const value = name || "Not provided";
  return createElement("div", { className: "min-w-0 text-xs" },
    createElement("p", { className: "text-slate-500" }, `${label} · `,
      createElement("code", { className: "text-slate-600" }, field)),
    createElement("div", { className: "mt-1 flex min-w-0 items-center gap-2" },
      createElement("code", { className: "min-w-0 select-text break-all font-mono text-slate-900" }, value),
      // Only a declared identifier can be offered as a copyable config value.
      name && createElement("button", {
        type: "button", "data-track": "copy_auth_name", "aria-label": "Copy auth name", title: "Copy auth name",
        onClick: handleCopy,
        className: "shrink-0 rounded p-1 text-slate-500 hover:bg-slate-100 hover:text-slate-900 focus-visible:outline focus-visible:outline-2 focus-visible:outline-blue-500",
      }, createElement(Copy, { className: "h-3.5 w-3.5", "aria-hidden": true }))),
    createElement("p", { role: "status", className: "text-xs text-slate-500" }, status),
  );
}
