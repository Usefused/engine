import { type ReactNode, useEffect, useRef, useState } from "react";
import { ChevronDown, Database, KeyRound, Plus } from "lucide-react";
import { type BucketEntryKind } from "~/lib/buckets";

type BucketAddDropdownProps = {
  disabled?: boolean;
  onSelect: (kind: BucketEntryKind) => void;
  allowedKinds: BucketEntryKind[];
};

/** Offers only entry kinds the actor is allowed to create. */
export function BucketAddDropdown({ disabled, onSelect, allowedKinds }: BucketAddDropdownProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const closeOnOutsideClick = (event: MouseEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", closeOnOutsideClick);
    return () => document.removeEventListener("mousedown", closeOnOutsideClick);
  }, []);

  const choose = (kind: BucketEntryKind) => {
    setOpen(false);
    onSelect(kind);
  };

  return (
    <div className="relative" ref={rootRef}>
      <button
        type="button"
        disabled={disabled}
        onClick={() => setOpen((value) => !value)}
        className="inline-flex items-center gap-2 rounded-lg bg-blue-600 px-3 py-2 text-sm font-medium text-white hover:bg-blue-700 disabled:opacity-50"
      >
        <Plus className="w-4 h-4" />
        Add
        <ChevronDown className="w-4 h-4" />
      </button>
      {open && (
        <div className="absolute right-0 top-full z-20 mt-2 w-44 rounded-lg border border-slate-200 bg-white p-1 shadow-lg">
          {allowedKinds.includes("secret") && <MenuItem icon={<KeyRound className="w-4 h-4" />} label="Secret" onClick={() => choose("secret")} />}
          {allowedKinds.includes("value") && <MenuItem icon={<Database className="w-4 h-4" />} label="Value" onClick={() => choose("value")} />}
        </div>
      )}
    </div>
  );
}

function MenuItem({ icon, label, onClick }: { icon: ReactNode; label: string; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm font-medium text-slate-700 hover:bg-slate-50"
    >
      <span className="text-slate-400">{icon}</span>
      {label}
    </button>
  );
}
