import type { BucketContentAccess, BucketContentPages, BucketContentSection, BucketContentState } from "./buckets";

type ReadContents = (bucketId: string, pages: BucketContentPages, access: BucketContentAccess, section: BucketContentSection) => Promise<BucketContentState>;

/** Keys only the selected view's pagination and filters so unrelated controls cannot invalidate it. */
function sectionPages(section: BucketContentSection, pages: BucketContentPages): unknown {
  // Connected users also own the service-filter search, independently of the Services tab.
  if (section === "connected-users") return [pages.connections, pages.connectionServices];
  if (section === "apps") return pages.sdks;
  if (section === "env") return pages.values;
  return pages[section];
}

/** Retains visited pages and in-flight reads until a bucket write, explicit refresh, or access change. */
export function createBucketContentCache(readContents: ReadContents) {
  const buckets = new Map<string, Map<string, Promise<BucketContentState>>>();
  return {
    /** Shares in-flight and completed reads for the same authorized view. */
    read(bucketId: string, pages: BucketContentPages, access: BucketContentAccess, section: BucketContentSection) {
      let entries = buckets.get(bucketId);
      // A fresh map isolates a bucket's lifetime from older requests invalidated by a write.
      if (!entries) {
        entries = new Map();
        buckets.set(bucketId, entries);
      }
      const key = JSON.stringify([section, sectionPages(section, pages), access]);
      const existing = entries.get(key);
      // Returning the same promise prevents duplicate reads during rapid tab changes.
      if (existing) return existing;
      const pending = readContents(bucketId, pages, access, section).catch(/** Failed reads remain retryable and never erase a newer request. */ (error) => {
        if (entries.get(key) === pending) entries.delete(key);
        throw error;
      });
      entries.set(key, pending);
      return pending;
    },
    /** A write can affect counts and service usage, so invalidate every cached view of that bucket. */
    invalidate(bucketId: string) {
      buckets.delete(bucketId);
    },
    /** Access changes discard all cached data rather than retaining previously authorized values. */
    clear() {
      buckets.clear();
    },
  };
}
