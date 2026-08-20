export type BucketEntryKind = "secret" | "value";

export type BucketEntryActions = {
  canCopy: boolean;
  canRemove: boolean;
};

/** Returns only actions that the row can truthfully and safely perform. */
export function bucketEntryActions(
  kind: BucketEntryKind,
  value: string,
  canRemove: boolean
): BucketEntryActions {
  return {
    canCopy: kind === "value" && value !== "-",
    canRemove,
  };
}
