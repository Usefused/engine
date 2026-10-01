import { api, type SecretMeta, type GraphQLPage } from "./api";

/** Finds one named variable using bounded metadata pages; plaintext is never requested. */
export async function readNamedBucketSecret(bucketID: string, key: string): Promise<SecretMeta | null> {
  const limit=100;
  for(let offset=0;offset<10000;offset+=limit){
    const {secretMetaPage:page}=await api.mcpGraphql<{secretMetaPage:GraphQLPage<SecretMeta>}>(`query NamedBucketSecretPage($bucketId: String!, $limit: Int!, $offset: Int!) {
      secretMetaPage(bucket_id:$bucketId,limit:$limit,offset:$offset) {
        total items { id bucket_id service_id key_name key_names credential_type expires_at }
      }
    }`,{bucketId:bucketID,limit,offset});
    const found=page.items.find((item)=>item.credential_type==="bucket_secret" && item.key_name===`secret:${key}`);
    // Keep the original expiry and exact stored name when replacing a value.
    if(found) return found;
    // An exhausted authorized result is a missing variable, not permission to create one implicitly.
    if(offset+page.items.length>=page.total || page.items.length===0) return null;
  }
  throw new Error("Secret lookup exceeded the page limit. Open the bucket to locate the variable.");
}
