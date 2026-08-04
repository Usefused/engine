interface ArtifactRuntimeStatusProps {
  active?: boolean;
  runtimeState?: string;
  className?: string;
}

export function ArtifactRuntimeStatus({ active, runtimeState, className = "" }: ArtifactRuntimeStatusProps) {
  // Reconciled definitions intentionally survive without credentials, so the
  // Engine runtime state—not navigation—controls when setup remains required.
  const needsSetup = active === false || runtimeState === "needs_configuration";
  if (!needsSetup) return null;

  return (
    <span className={`flex items-center gap-1.5 text-[11px] font-medium leading-4 text-amber-700 ${className}`}>
      <span className="h-1.5 w-1.5 shrink-0 rounded-full bg-amber-500" aria-hidden="true" />
      Setup required
    </span>
  );
}
