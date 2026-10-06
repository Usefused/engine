import { CreateCredentialButton } from "./CreateCredentialButton";
import type { RefObject } from "react";

type BucketPageHeaderProps = {
  onCreateClick: () => void;
  canCreate: boolean;
  createAnchor: RefObject<HTMLDivElement>;
  createOpen: boolean;
};

/** Anchors the compact bucket form to its authorized create action. */
export function BucketPageHeader({ onCreateClick, canCreate, createAnchor, createOpen }: BucketPageHeaderProps) {
  return (
    <div className="flex flex-col justify-between gap-4 md:flex-row md:items-start">
        <div>
          <h1 className="text-2xl font-bold text-slate-900">Credentials</h1>
          <p className="mt-1 text-slate-500">Keep service credentials separate by environment, customer, or team.</p>
        </div>
        {/* Creation is shown only for actors with workspace bucket management. */}
        {canCreate && <div ref={createAnchor} className="self-start"><CreateCredentialButton onClick={onCreateClick} expanded={createOpen} controls="create-bucket-popover" /></div>}
    </div>
  );
}
