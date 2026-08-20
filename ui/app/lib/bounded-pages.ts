export type BoundedPage<T> = {
  items: T[];
  total: number;
};

/** Collects a finite server catalogue while failing if pagination cannot advance. */
export async function readAllBoundedPages<T>(
  readPage: (limit: number, offset: number) => Promise<BoundedPage<T>>,
  pageSize: number,
  maxPages: number
): Promise<T[]> {
  const items: T[] = [];
  for (let pageNumber = 0; pageNumber < maxPages; pageNumber += 1) {
    const page = await readPage(pageSize, items.length);
    items.push(...page.items);
    if (items.length >= page.total) return items;
    // An empty partial page cannot advance, so failing avoids both an infinite
    // request loop and a silently incomplete selector.
    if (page.items.length === 0) throw new Error("Catalogue pagination did not advance.");
  }
  throw new Error("Catalogue exceeds the supported page bound.");
}
