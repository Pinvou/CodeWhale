import { createServer as createViteServer, parse } from 'vite';
import { fixtureScope, ownedDirectory, pendingFixtureShutdown } from './fixture-scope.mjs';
export { build } from 'vite';

// Dev-server compilation is fixture setup, not a page-action deadline. Walk
// each requested static import graph in this server's private optimizer before the first
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
  const optimizer = environment.depsOptimizer;
  const deadline = performance.now() + timeoutMs;
  const expired = () => {
    if (performance.now() >= deadline) throw new Error('fixture module setup budget exceeded');
  };
  // Parse transformed JavaScript using Vite's own public parser. This follows
  // imports and re-exports without executing modules or eagerly loading lazy
  // import() branches. Cycles and shared dependencies are visited once per
  // optimizer generation, including Vite's injected client dependencies.
  const walk = async (url, seen) => {
    expired();
    if (seen.has(url)) return;
    seen.add(url);
    const result = await environment.transformRequest(url);
    if (!result) throw new Error('fixture entry did not produce a module');
    const parsed = await parse('fixture-module.js', result.code, { sourceType: 'module' });
    if (parsed.errors.length) throw new Error(`fixture module parsing failed: ${url}`);
    for (const statement of parsed.program.body) {
      if (!['ImportDeclaration', 'ExportNamedDeclaration', 'ExportAllDeclaration'].includes(statement.type)
          || typeof statement.source?.value !== 'string') continue;
      const base = new URL(url, 'http://vite-fixture.invalid');
      const dependency = new URL(statement.source.value, base);
      // External/data modules are not fetched by fixture preparation.
      if (dependency.origin !== base.origin) continue;
      await walk(dependency.pathname + dependency.search, seen);
    }
  };
  let timer;
  try {
    await Promise.race([
      (async () => {
        await optimizer?.scanProcessing;
        let generation;
        do {
          expired();
          generation = optimizer?.metadata.browserHash;
          const seen = new Set();
          for (const entry of entries) await walk(entry, seen);
          // Vite's idle promise settles only for its FIRST crawl. Keep it for
          // initial optimizer coordination; the explicit walk is the barrier
          // for later fixture entries after server.listen()/browser launch.
          await environment.waitForRequestsIdle();
          await optimizer?.scanProcessing;
          await Promise.all(Object.values(optimizer?.metadata.discovered || {})
            .map(dependency => dependency.processing));
          // A prebundle may invalidate URLs while preparing a late entry.
          // Follow its final generation under the same single setup deadline.
        } while (generation !== optimizer?.metadata.browserHash);
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
