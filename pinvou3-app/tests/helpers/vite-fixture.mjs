import { createServer as createViteServer } from 'vite';
import { fixtureScope, ownedDirectory, pendingFixtureShutdown } from './fixture-scope.mjs';
export { build } from 'vite';

// Dev-server compilation is fixture setup, not a page-action deadline. Walk
// Vite's static import crawl in this server's private optimizer before the first
// browser case; callers still navigate/assert with their original short budget.
// Callers own the server and must close it in finally, including setup failure.
export async function prepareFixtureModules(server, entries, { timeoutMs = 30_000 } = {}) {
  if (!Array.isArray(entries) || entries.length === 0 || entries.some(entry =>
    typeof entry !== 'string' || !entry.startsWith('/') || entry.startsWith('//') ||
    entry.includes('..') || /[?#\\]/u.test(entry))) {
    throw new TypeError('fixture module entries must be explicit root-relative paths');
  }
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 30_000) {
    throw new RangeError('fixture module setup budget must be between 1 and 30000ms');
  }
  const environment = server.environments.client;
  let timer;
  try {
    await Promise.race([
      (async () => {
        for (const entry of entries) {
          const result = await environment.transformRequest(entry);
          if (!result) throw new Error('fixture entry did not produce a module');
        }
        // Includes recursive static imports and dependency optimizer requests,
        // rather than only the HTTP listener or the entry's first transform.
        await environment.waitForRequestsIdle();
      })(),
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error('fixture module setup budget exceeded')), timeoutMs);
      }),
    ]);
  } finally { clearTimeout(timer); }
}


// Optimizer output is private and HMR updates are disabled. Browser fixtures
// still load @vite/client, so keep its WebSocket on the owned HTTP listener.
// SSR-only middleware fixtures have no HTTP listener: disable WS there rather
// than opening Vite's shared default port 24678.
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
      server: { ...config.server, host: '127.0.0.1', port: 0, strictPort: true, hmr: false, ws: config.server?.middlewareMode ? false : {}, watch: null },
    });
    nativeClose = server.close.bind(server);
    server.close = () => pendingFixtureShutdown() || release();
    return server;
  } catch (primary) {
    setupFailed = !server;
    try { await release(); } catch (cleanup) { console.error('Vite fixture cleanup failed', cleanup); }
    throw primary;
  }
}
