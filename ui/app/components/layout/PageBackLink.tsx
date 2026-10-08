import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { Link } from "@remix-run/react";
import { ArrowLeft } from "lucide-react";

/** Keeps parent navigation in the compact utility row above the page title and its actions. */
export function PageBackLink({ to, children, className = "" }: { to: string; children: string; className?: string }) {
  const [host, setHost] = useState<HTMLElement | null>(null);
  // The workspace owns the navigation row; the route retains its exact parent destination.
  useEffect(() => { setHost(document.getElementById("integrations-back-navigation")); }, []);
  const navigation = <nav aria-label="Back navigation" className="flex min-w-0 items-center">
    <Link to={to} className={`inline-flex min-h-8 items-center gap-2 rounded text-xs font-medium text-slate-500 transition-colors hover:text-slate-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 ${className}`}><ArrowLeft aria-hidden="true" className="h-3.5 w-3.5" />{children}</Link>
  </nav>;
  // Standalone renders and the initial hydration pass still provide a usable back link.
  return host ? createPortal(navigation, host) : navigation;
}
