# Fused Runtime

`@fused/runtime` is the shared Node client for Fused Engine's gRPC execution protocol. It authenticates an existing SDK/API app identity; it does not generate a provider SDK or create a new app kind.

Requires Node 24+. Download the compiled npm package with the built-in CLI command,
then install it in your application:

```sh
fused-cli app runtime-client --out ./vendor
npm install ./vendor/fused-runtime/fused-runtime-0.1.0.tgz
```

The download works without an Engine connection or login, like `app auth-client`.
The CLI reports the exact archive and install command for its bundled version.
Provider execution still requires an existing Engine app and execution token.

```ts
import { EngineTransport } from '@fused/runtime';

const engine = new EngineTransport({
  grpcUrl: process.env.FUSED_ENGINE_GRPC_URL!,
  appId: process.env.FUSED_APP_ID!,
  token: process.env.FUSED_APP_TOKEN!,
});
try {
  const result = await engine.execute({
    endpoint: 'getStockData', // exact selected Engine operation name
    input: { symbol: 'IBM' },
    // Optional: select an exact bucket admitted by this app's immutable scope.
    // bucketId: '00000000-0000-4000-8000-000000000001',
    selector: { endUserRef: 'verified-user-reference' },
  });
} finally {
  await engine.close();
}
```

`connect`, `execute`, `startConnectSession`, `getConnection`, `listConnectionResources`, and `close` share one channel and per-RPC app authentication. `execute` accepts cancellation, a bounded pagination intent, an idempotency key, and non-secret connection selectors. Engine owns provider credentials, permissions, retries and pagination. HTTP/OAuth and gRPC addresses are independent settings.

TLS is mandatory except on explicit loopback IPs. Connection recovery repeats admission but never replays an execution. This initial extraction supports bounded JSON execution, selected unary Bridge aliases, and provider OAuth metadata. Binary/streaming response and durable webhook client APIs remain unsupported; protocol declarations alone do not provide those higher-level clients.

The wire schema is exported as `@fused/runtime/protocol`. Minion imports this package and retains its `@fused/minion/grpc` compatibility export. Builds copy Engine's authoritative `proto/engine/v1/engine.proto` directly; request field 11 is the explicitly authorized optional `bucket_id`.

Engine's `runtime/sdk` is the only maintained source. Run `npm ci && npm test` here,
then `npm run pack` to produce `dist/package/fused-runtime-0.1.0.tgz` and
`manifest.json`. The archive contains compiled JavaScript, declarations and the
protocol, with no install-time compilation. The manifest records the archive
checksum and a digest of the source/build inputs.

To update downstream bundles, copy those two generated files into
`cli/assets/fused-runtime/` and Hostlier's `packages/vendor/fused-runtime/`, removing
any older archive. Regenerate Minion's lockfile when the runtime version changes.
These are distribution snapshots, not separate source implementations; both
repositories can build independently without a sibling Engine checkout or npm
publication. Minion's release archive bundles the installed runtime package.

Tests use real loopback gRPC servers and exercise admission, selectors, cancellation, response bounds, reconnection and non-replay. No live provider credentials are needed.
