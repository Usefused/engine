import { useEffect, useRef, useState, type MouseEvent, type ReactNode } from "react";
import { Check, Copy } from "lucide-react";

/** Copies only on an explicit click; clipboard errors never expose the value or claim success. */
export function CopyButton({ value, label, onCopied }: { value: string; label: string; onCopied?: () => void }) {
  const [status, setStatus] = useState<"idle" | "copied" | "failed">("idle");
  const request = useRef(0);
  // Feedback belongs to the displayed value, including when a mounted field changes versions.
  useEffect(() => {
    request.current++;
    setStatus("idle");
    // A pending clipboard result cannot acknowledge a different value after navigation.
    return () => { request.current++; };
  }, [value]);
  // Keep successful confirmation brief so the control remains available for repeated copies.
  useEffect(() => {
    // Confirmation should return to a copy affordance without requiring another navigation.
    if (status !== "copied") return;
    const timer = setTimeout(() => setStatus("idle"), 2000);
    return () => clearTimeout(timer);
  }, [status]);
  /** Stops parent disclosure/navigation while keeping the copied payload out of tracking and storage. */
  async function copy(event: MouseEvent<HTMLButtonElement>) {
    event.preventDefault(); event.stopPropagation();
    const current = ++request.current;
    try {
      await navigator.clipboard.writeText(value);
      // Ignore completion after the field changed or unmounted.
      if (current !== request.current) return;
      setStatus("copied"); onCopied?.();
    } catch {
      // Browser error text may contain private values; show a fixed actionable message instead.
      if (current === request.current) setStatus("failed");
    }
  }
  // Empty values cannot be copied; status text distinguishes confirmed success from a browser failure.
  return <span className="relative inline-flex shrink-0">
    <button type="button" aria-label={status === "copied" ? `${label} copied` : `Copy ${label}`} title={status === "copied" ? "Copied" : `Copy ${label}`} disabled={!value} onClick={copy} className="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded-md text-slate-500 hover:bg-slate-100 hover:text-slate-900 focus-visible:outline-2 focus-visible:outline-[var(--brand-violet)] disabled:opacity-40">
      {status === "copied" ? <Check className="h-3.5 w-3.5 text-emerald-600" aria-hidden="true" /> : <Copy className="h-3.5 w-3.5" aria-hidden="true" />}
    </button>
    <span role="status" className={status === "failed" ? "absolute right-0 top-full z-20 mt-1 w-48 rounded-md border border-slate-200 bg-white p-2 text-xs text-red-700 shadow-sm" : "sr-only"}>{status === "failed" ? "Could not copy. Select the value and copy it manually." : status === "copied" ? "Copied to clipboard." : ""}</span>
  </span>;
}

/** Keeps long values on one scrollable line with the copy action anchored inside the field. */
export function CopyValue({ value, label, prefix, className = "", onCopied, sensitive = /token|secret|credential|key/i.test(label) }: { value: string; label: string; prefix?: ReactNode; className?: string; sensitive?: boolean; onCopied?: () => void }) {
  // Optional method badges stay outside the scrollable value and never enter the clipboard payload.
  return <div data-fused-visible={sensitive ? "false" : undefined} className={`flex min-w-0 items-center gap-2 rounded-md border border-slate-200 bg-slate-50/70 py-1 pl-3 pr-1 ${className}`}>
    {prefix && <span className="shrink-0 rounded bg-slate-200/60 px-1.5 py-0.5 text-[10px] font-semibold tracking-wide text-slate-600">{prefix}</span>}
    <code className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap py-1 text-xs text-slate-700 select-all [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">{value}</code>
    <CopyButton value={value} label={label} onCopied={onCopied} />
  </div>;
}
