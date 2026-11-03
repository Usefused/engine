import { cp, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const execute = promisify(execFile);
const root = fileURLToPath(new URL('..', import.meta.url));
const args = process.argv.slice(2);
// Only an output directory is configurable; release payloads always use reviewed build inputs.
if (args.length && (args.length !== 2 || args[0] !== '--pack-destination')) throw new Error('Usage: npm run pack -- [--pack-destination DIRECTORY]');
const destination = resolve(args[1] ?? resolve(root, 'dist/package'));

/** Hash the source inputs in stable path order so consumers can identify their Engine-owned build. */
async function sourceDigest() {
  const hash = createHash('sha256');
  const files = ['package.json', 'package-lock.json', 'tsconfig.json', 'README.md', '../../proto/engine/v1/engine.proto'];
  for (const directory of ['src', 'scripts']) {
    const entries = await readdir(resolve(root, directory));
    files.push(...entries.map(entry => `${directory}/${entry}`));
  }
  for (const file of files.sort()) {
    hash.update(file + '\0');
    hash.update(await readFile(resolve(root, file)));
    hash.update('\0');
  }
  return hash.digest('hex');
}

await execute('npm', ['run', 'build'], { cwd: root });
const manifest = JSON.parse(await readFile(resolve(root, 'package.json'), 'utf8'));
const stage = await mkdtemp(resolve(tmpdir(), 'fused-runtime-'));
try {
  for (const file of manifest.files) await cp(resolve(root, file), resolve(stage, file), { recursive: true });
  // Installed packages contain compiled code and types; they never need repository build scripts.
  delete manifest.scripts;
  delete manifest.devDependencies;
  await writeFile(resolve(stage, 'package.json'), JSON.stringify(manifest, null, 2) + '\n');
  await mkdir(destination, { recursive: true });
  const { stdout } = await execute('npm', ['pack', '--ignore-scripts', '--json', '--pack-destination', destination], { cwd: stage });
  const [receipt] = JSON.parse(stdout);
  const archive = await readFile(resolve(destination, receipt.filename));
  const provenance = {
    name: manifest.name,
    version: manifest.version,
    filename: receipt.filename,
    sha256: createHash('sha256').update(archive).digest('hex'),
    source: 'Usefused/engine:runtime/sdk',
    sourceSha256: await sourceDigest(),
  };
  await writeFile(resolve(destination, 'manifest.json'), JSON.stringify(provenance, null, 2) + '\n');
  process.stdout.write(JSON.stringify(provenance, null, 2) + '\n');
} finally {
  await rm(stage, { recursive: true, force: true });
}
