import { copyFile } from 'node:fs/promises';
// Build from Engine's authoritative schema so the client cannot carry a divergent wire contract.
await copyFile(new URL('../../../proto/engine/v1/engine.proto', import.meta.url), new URL('../dist/src/engine.proto', import.meta.url));
