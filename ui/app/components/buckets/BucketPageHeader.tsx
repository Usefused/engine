import { CreateCredentialButton } from "./CreateCredentialButton";

type BucketPageHeaderProps = {
  onCreateClick: () => void;
  canCreate: boolean;
};

/** Renders the credential-set heading and an authorized create action. */
export function BucketPageHeader({ onCreateClick, canCreate }: BucketPageHeaderProps) {
  return (
    <div className="flex flex-col justify-between gap-4 md:flex-row md:items-start">
        <div>
          <h1 className="text-2xl font-bold text-slate-900">Credentials</h1>
          <p className="mt-1 text-slate-500">Keep service credentials separate by environment, customer, or team.</p>
        </div>
        {/* Creation is shown only for actors with workspace bucket management. */}
        {canCreate && <div className="self-start"><CreateCredentialButton onClick={onCreateClick} /></div>}
    </div>
  );
}
