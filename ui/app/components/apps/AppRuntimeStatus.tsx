interface AppRuntimeStatusProps {
  status?: string;
  className?: string;
}

export function AppRuntimeStatus({ status, className = "" }: AppRuntimeStatusProps) {
  if (!status || status === "active") return null;

  const label = status === "deprecated" ? "Deprecated" : status === "building" ? "Building" : status;
  const colour = status === "deprecated" ? "text-amber-700" : "text-slate-600";
  return (
    <span className={`flex items-center gap-1.5 text-[11px] font-medium leading-4 ${colour} ${className}`}>
      <span className="h-1.5 w-1.5 shrink-0 rounded-full bg-current" aria-hidden="true" />
      {label}
    </span>
  );
}
