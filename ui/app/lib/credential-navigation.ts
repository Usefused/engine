export const CREATE_CREDENTIAL_PATH = "/integrations/buckets?create=credential";

export function requestsCredentialCreation(searchParams: URLSearchParams): boolean {
  return searchParams.get("create") === "credential";
}

export function consumeCredentialCreationRequest(searchParams: URLSearchParams): URLSearchParams {
  const next = new URLSearchParams(searchParams);
  next.delete("create");
  return next;
}
