import { createServer, Server } from "node:http";
import { AddressInfo } from "node:net";
import { expect, it } from "vitest";
import { runExecute, SessionState } from "./sandbox.js";

/** Starts an ephemeral loopback bridge; no provider, session credential, or personal data is involved. */
async function listen(server: Server): Promise<string> {
  // Surface denied loopback permissions immediately instead of hanging until the test timeout.
  await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  return String((server.address() as AddressInfo).port);
}

/** Closes test-owned sockets so failed assertions cannot leave background test resources alive. */
async function close(server: Server): Promise<void> {
  server.closeAllConnections();
  await new Promise<void>((resolve) => { server.close(() => resolve()); });
}

/** Tests real fetch cancellation before headers and while streaming the response body. */
it.each([false, true])("aborts the bridge HTTP request on timeout (headers sent: %s)", async (sendHeaders) => {
  let calls = 0;
  let observeClose!: () => void;
  // The close event proves transport cancellation, not just rejection of a wrapper promise.
  const disconnected = new Promise<void>((resolve) => { observeClose = resolve; });
  // Hold the response open until the invocation deadline closes its fetch connection.
  const server = createServer((request, response) => {
    calls++;
    request.resume();
    response.on("close", observeClose);
    // Body consumption must share the same AbortSignal as initial connection establishment.
    if (sendHeaders) { response.writeHead(200, { "Content-Type": "application/json" }); response.write('{"result":'); }
  });
  const port = await listen(server);
  try {
    const output = await runExecute('try { await call("fixture.wait"); } catch {} await call("fixture.late");', { sessionId: "synthetic", enginePort: port }, new SessionState(), { timeoutMs: 250, maxCalls: 10 });
    expect(output).toMatchObject({ isError: true, executionOutcome: "timed_out" });
    await disconnected;
    expect(calls).toBe(1);
  } finally {
    await close(server);
  }
});

/** End-to-end runtime composition still allows ordinary awaited delays and healthy subsequent invocations. */
it("executes a delayed sequence once and reuses the session afterwards", async () => {
  let calls = 0;
  // A synthetic UTF-8 response exercises conversion on data received through the real bridge client.
  const server = createServer((request, response) => {
    calls++;
    request.resume();
    response.writeHead(200, { "Content-Type": "application/json" });
    response.end(JSON.stringify({ result: { text: Buffer.from("Résumé 🌍").toString("base64url") } }));
  });
  const options = { sessionId: "synthetic", enginePort: await listen(server) };
  const session = new SessionState();
  try {
    const output = await runExecute('await call("fixture.first"); await sleep(20); const result=await call("fixture.second"); return decodeBase64(result.text);', options, session);
    expect(output).toMatchObject({ isError: false, executionOutcome: "completed", text: JSON.stringify("Résumé 🌍") });
    expect(calls).toBe(2);
    expect(await runExecute('return atob("b2s=");', options, session)).toMatchObject({ text: '"ok"', isError: false });
    expect(calls).toBe(2);
  } finally {
    await close(server);
  }
});
