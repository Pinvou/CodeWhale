import { createServer as createViteServer } from 'vite';
import { fixtureScope, ownedDirectory } from './fixture-scope.mjs';
export { build } from 'vite';


// All one-shot fixtures use isolated optimizer output and no HMR listener.
// HTTP port=0 alone does not isolate middleware-mode Vite's HMR port 24678.
export async function createServer(config = {}) {
  const directory = ownedDirectory('vite');
  let server;
  let nativeClose;
  let setupFailed = false;
  let release;
  try { release = fixtureScope.register({
    kind: 'vite',
    close: async () => { if (nativeClose) await nativeClose(); },
    verify: async () => !setupFailed && !server?.httpServer?.listening,
    remove: () => directory.remove(),
  }); } catch (primary) {
    try { directory.remove(); } catch (cleanup) {
      throw new AggregateError([primary, cleanup], 'Vite fixture registration and directory retirement failed', { cause: cleanup });
    }
    throw primary;
  }
  try {
    server = await createViteServer({
      ...config,
      cacheDir: directory.path,
      server: { ...config.server, host: '127.0.0.1', port: 0, strictPort: true, hmr: false, ws: false, watch: null },
    });
    nativeClose = server.close.bind(server);
    server.close = release;
    return server;
  } catch (primary) {
    setupFailed = !server;
    try { await release(); } catch (cleanup) { console.error('Vite fixture cleanup failed', cleanup); }
    throw primary;
  }
}
