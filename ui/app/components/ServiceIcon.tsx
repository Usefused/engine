import { useState } from "react";
import { Globe2 } from "lucide-react";

/** Renders an imported service icon with a deterministic identity fallback. */
export function ServiceIcon({ name, iconURL }: { name: string; iconURL?: string | null }) {
  const [failed, setFailed] = useState(false);
  // Registry validates imported URLs; a failed network image still falls back without leaving a broken marker.
  if (iconURL && !failed) {
    return (
      <span className="flex h-10 w-10 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-slate-100">
        <img
          src={iconURL}
          alt=""
          loading="lazy"
          referrerPolicy="no-referrer"
          className="h-full w-full object-contain p-1.5"
          onError={() => setFailed(true)}
        />
      </span>
    );
  }
  // A globe remains legible for malformed or intentionally symbol-only service names.
  const fallback = name.trim().slice(0, 1).toUpperCase() || <Globe2 className="h-4 w-4" />;
  return <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-slate-100 text-sm font-semibold text-slate-700">{fallback}</span>;
}
