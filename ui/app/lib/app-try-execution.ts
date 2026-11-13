type TryTokenAPI = {
  generate: (familyID: string, input: { name: string; allow: string[]; expires_in: number }) => Promise<{ token: string }>;
  revoke: (familyID: string, name: string) => Promise<void>;
};

export class TryTokenError extends Error {}

/** Uses ordinary token issuance permissions; browser sessions never authenticate runtime requests. */
export async function executeAppTry(options: { familyID: string; operation: string; token?: string; url: string; body: string; signal: AbortSignal; sessionSignal?: AbortSignal }, tokens: TryTokenAPI, send: typeof fetch = fetch): Promise<Response> {
  let token = options.token;
  let generatedName: string | undefined;
  if (token === undefined) {
    generatedName = `try-${crypto.randomUUID()}`;
    try {
      const issued = await tokens.generate(options.familyID, { name: generatedName, allow: [options.operation], expires_in: 300 });
      token = issued.token;
    } catch {
      throw new TryTokenError("Could not authorize this test. Check your connection and app token permissions, or choose Use execution token. No operation was sent.");
    }
  }
  const revoke = generatedName ? sessionTokenCleanup(tokens, options.familyID, generatedName, options.sessionSignal ?? options.signal) : () => undefined;
  let dispatched = false;
  let completed = false;
  try {
    // Issuance is not cancelled: a late result still needs cleanup when its sidebar closes.
    options.signal.throwIfAborted();
    options.sessionSignal?.throwIfAborted();
    dispatched = true;
    const response = await send(options.url, {
      method: "POST", credentials: "omit", cache: "no-store", redirect: "error", signal: options.signal,
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` }, body: options.body,
    });
    completed = response.status !== 202;
    return response;
  } finally {
    // Pending or uncertain work retains its token only while the sidebar session remains open.
    if (!dispatched || completed) revoke();
  }
}

/** Revokes a generated credential once, including after the request has settled but its sidebar later closes. */
function sessionTokenCleanup(tokens: TryTokenAPI, familyID: string, name: string, signal: AbortSignal): () => void {
  let revoked = false;
  function revoke() {
    if (revoked) return;
    revoked = true;
    signal.removeEventListener("abort", revoke);
    // Five-minute server expiry remains the fallback if the browser cannot deliver revocation.
    void tokens.revoke(familyID, name).catch(() => undefined);
  }
  signal.addEventListener("abort", revoke, { once: true });
  // Closing during issuance must clean a token that arrives after the session has ended.
  if (signal.aborted) revoke();
  return revoke;
}
