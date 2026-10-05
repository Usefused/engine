import { readAllBoundedPages, type BoundedPage } from "./bounded-pages.ts";

type SecretMetadata = { key_name: string; key_names?: string[]; service_id?: string | null };
type SecretCreation = { bucket: { id: string; name: string }; keyName: string; value: string };
type SecretWriter = {
  read: (bucketId: string, limit: number, offset: number) => Promise<BoundedPage<SecretMetadata>>;
  save: (payload: { bucketId: string; keyName: string; value: string }) => Promise<void>;
};

/** Saves a new generic secret and returns only its canonical reference to the calling form. */
export async function createReferencedSecret(input: SecretCreation, writer: SecretWriter): Promise<string> {
  const keyName = input.keyName.trim();
  // Dots split reference segments; secret names must also match Engine's whitespace and escape restrictions.
  if (!input.bucket.id || !input.bucket.name.trim() || /[.{}]/.test(input.bucket.name) || !keyName || /[.{}$\s]/.test(keyName)) {
    throw new Error("Choose a bucket without dots or braces, and a secret name without spaces, dots, dollar signs, or braces.");
  }
  // Preserve the exact secret bytes, including meaningful leading or trailing whitespace.
  if (!input.value) throw new Error("Enter a secret value.");
  const metadata = await readAllBoundedPages((limit, offset) => writer.read(input.bucket.id, limit, offset), 100, 100);
  // The existing endpoint is an upsert; creation must not knowingly replace a stored secret.
  if (metadata.some((item) => (!item.service_id || item.service_id === "00000000-0000-0000-0000-000000000000") && [item.key_name, ...(item.key_names ?? [])].includes(`secret:${keyName}`))) {
    throw new Error("A secret with this name already exists. Choose another name, or use the existing reference.");
  }
  await writer.save({ bucketId: input.bucket.id, keyName, value: input.value });
  return "${bucket." + input.bucket.name + ".secret." + keyName + "}";
}
