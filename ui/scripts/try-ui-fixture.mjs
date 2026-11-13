import { createServer } from 'vite';
import tailwindcss from '@tailwindcss/vite';
import path from 'node:path';
const ui = path.resolve(import.meta.dirname, '..');
const server = await createServer({ configFile: false, root: path.join(ui, 'testdata/try-ui'), resolve: { dedupe: ['react', 'react-dom'], alias: { '~': path.join(ui, 'app') } }, esbuild: { jsx: 'automatic' }, plugins: [tailwindcss()], server: { host: '127.0.0.1', port: 18213, strictPort: true, fs: { allow: [ui] } } });
await server.listen();
console.log('Try sidebar preview: http://127.0.0.1:18213/');
