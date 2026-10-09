import { Database, Plus } from "lucide-react";
import { CataloguePageHeader, catalogueActionClassName } from "~/components/layout/CataloguePageHeader";
import type { RefObject } from "react";

type BucketPageHeaderProps = {
  onCreateClick: () => void;
  canCreate: boolean;
  createAnchor: RefObject<HTMLDivElement>;
  createOpen: boolean;
};

/** Keeps bucket creation beside its title while preserving its anchored form and permission boundary. */
export function BucketPageHeader({ onCreateClick, canCreate, createAnchor, createOpen }: BucketPageHeaderProps) {
  return <CataloguePageHeader title="Bucket" icon={Database} description="Keep service credentials separate by environment, customer, or team." actions={
    // Only actors with workspace bucket management may open the creation form.
    canCreate && <div ref={createAnchor}><button type="button" onClick={onCreateClick} aria-expanded={createOpen} aria-controls="create-bucket-popover" aria-haspopup="dialog" className={catalogueActionClassName}><Plus className="h-3.5 w-3.5" aria-hidden="true" />Create bucket</button></div>
  } />;
}
