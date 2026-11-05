// Shared public loopback fixture factory. Never connects to an account service or database.
// Own HTTP, Vite, requests and the projection as one ordered lifetime. The generic
// Vite factory closes its scope on signals before this factory can drain scans.
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { dirname, join, resolve } from 'node:path';
import { copyFileSync, cpSync, lstatSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { createServer as createHttpServer } from 'node:http';
import { tmpdir } from 'node:os';
const require = createRequire(new URL('../../package.json', import.meta.url));
const { createServer: createViteServer } = await import(pathToFileURL(require.resolve('vite')).href);
const react = (await import(pathToFileURL(require.resolve('@vitejs/plugin-react')).href)).default;
const inputRoot = fileURLToPath(new URL('../../', import.meta.url));
// Vite correctly denies .git inputs. Serve only a fresh public source projection
// outside .git; keep the default deny list and exact filesystem allow-list.
export async function startRechargePollingFixture() {
const port = Number(process.env.PINVOU_TEST_TOK03_PORT || 0);
if (!Number.isSafeInteger(port) || port < 0 || port > 65535) throw new Error('FIXTURE_PORT_INVALID');
let root; let rootIdentity;
const removeProjection = () => {
  if (!root) return;
  let current;
  try { current = lstatSync(root); } catch (reason) { if (reason.code === 'ENOENT') return; throw reason; }
  if (dirname(resolve(root)) !== resolve(tmpdir()) || !root.split(/[/\\]/).pop().startsWith('pinvou-public-tok03-app-')
    || !rootIdentity || !current.isDirectory() || current.isSymbolicLink()
    || current.dev !== rootIdentity.dev || current.ino !== rootIdentity.ino) throw new Error('FIXTURE_CACHE_TARGET_INVALID');
  rmSync(root, { recursive: true, force: true });
};
try {
  root = mkdtempSync(join(tmpdir(), 'pinvou-public-tok03-app-'));
  rootIdentity = lstatSync(root);
  for (const part of ['src/features/account-platform', 'src/shared'])
    cpSync(join(inputRoot, part), join(root, part), { recursive: true, dereference: false });
  mkdirSync(join(root, 'tests/fixtures'), { recursive: true });
  copyFileSync(join(inputRoot, 'package.json'), join(root, 'package.json'));
  for (const name of ['account-platform-recharge-polling.html', 'account-platform-recharge-polling.jsx'])
    copyFileSync(join(inputRoot, 'tests/fixtures', name), join(root, 'tests/fixtures', name));
} catch (reason) {
  const errors = [reason];
  try { removeProjection(); } catch (cleanupReason) {
    errors.push(cleanupReason);
  }
  if (errors.length > 1) throw new AggregateError(errors, 'FIXTURE_STARTUP_CLEANUP_FAILED', { cause: reason });
  throw reason;
}
const cache = join(root, '.vite-cache');
const id = '33333333-3333-4333-8333-333333333333';
let actor = 'A', status = 'PENDING', closeStatus = 'OPEN', hold = false, created = Date.now(), lifetime = 120000, denyDetail = 0, denyList = 0, denyCreate = 0, denyWallet = 0, denyPurchase = 0, unknownCreate = false, requests = [], held = new Map();
const createdKeys = new Map(), heldLists = new Map(), heldCreates = new Map(); let holdList = false, holdCreate = false;
const order = (org = '') => ({ ...(org ? { beneficiary_organization_id: org } : {}), order_id: id, product_id: 'public-fixture-product', plan_name: 'Public fixture plan', amount_fen: '1234', status,
  channel: 'ALIPAY', qr_scan_apps: ['ALIPAY'], close_status: closeStatus, fulfillment_status: ['PAID', 'PAID_LATE'].includes(status) ? 'CREDITED' : 'PENDING', expires_at: new Date(created + lifetime).toISOString(), created_at: new Date(created).toISOString(),
  payment_url: ['CREATED', 'PENDING'].includes(status) && closeStatus === 'OPEN' ? 'https://example.com/public-fixture-payment' : '', qr_data: ['CREATED', 'PENDING'].includes(status) && closeStatus === 'OPEN' ? 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aNbQAAAAASUVORK5CYII=' : '' });
// Match the public history projection: collection rows never carry payment instructions.
const historyOrder = () => {
  const { payment_url, qr_data, qr_scan_apps, ...facts } = order();
  return facts;
};
const send = (r, body, code = 200) => { r.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); r.end(JSON.stringify(body)); };
const state = () => ({ actor, status, hold, held: held.size, heldLists: heldLists.size, heldCreates: heldCreates.size, requests, detailReads: requests.filter(item => item.path.endsWith('/' + id)).length,
  walletReads: requests.filter(item => item.path.endsWith('/balance')).length, packageReads: requests.filter(item => item.path.endsWith('/entitlement-packages')).length,
  productMutations: requests.filter(item => item.method !== 'GET').length, createKeys: [...createdKeys.keys()], listReads: requests.filter(item => item.path === '/api/token/v1/recharge/orders' && item.method === 'GET').length });
const fixtureAPI = async (req, res, next) => {
    const path = req.url.split('?')[0];
    if (path === '/__fixture__/state') { send(res, state()); return; }
    if (path === '/__fixture__/control' && req.method === 'POST') {
      let body = ''; for await (const chunk of req) { body += chunk; if (body.length > 4096) { send(res, { error: 'TOO_LARGE' }, 413); return; } }
      const input = JSON.parse(body);
      if (input.reset) { actor = 'A'; status = 'PENDING'; closeStatus = 'OPEN'; hold = false; created = Date.now(); lifetime = 120000; denyDetail = 0; denyList = 0; denyCreate = 0; denyWallet = 0; denyPurchase = 0; unknownCreate = false; createdKeys.clear(); holdList = false; holdCreate = false; for (const response of [...heldLists.keys(), ...heldCreates.keys()]) response.destroy(); heldLists.clear(); heldCreates.clear(); requests = []; for (const r of held.keys()) r.destroy(); held.clear(); }
      if (input.actor === 'B') actor = 'B'; if (['CREATED','PENDING','PAID','PAID_LATE','FAILED','CLOSED','EXPIRED','MANUAL_REVIEW'].includes(input.status)) status = input.status;
      if (typeof input.closeStatus === 'string') closeStatus = input.closeStatus;
      if (Number.isSafeInteger(input.expiresInMS)) lifetime = Date.now() - created + input.expiresInMS;
      if (Number.isInteger(input.denyPurchase)) denyPurchase = input.denyPurchase;
      if ([0,403].includes(input.denyCreate)) denyCreate = input.denyCreate; if ([0,403].includes(input.denyWallet)) denyWallet = input.denyWallet;
      if ([0,401,403].includes(input.denyDetail)) denyDetail = input.denyDetail; if ([0,401,403,503].includes(input.denyList)) denyList = input.denyList;
      if (input.holdList === true) holdList = true; if (input.holdCreate === true) holdCreate = true;
      if (input.releaseList) { holdList = false; for (const [response, snapshot] of heldLists) send(response, snapshot); heldLists.clear(); }
      if (input.releaseCreate) { holdCreate = false; for (const [response, snapshot] of heldCreates) send(response, snapshot, 201); heldCreates.clear(); }
      if (input.unknownCreate === true) unknownCreate = true; if (input.hold === true) hold = true;
      if (input.release) { hold = false; for (const [r, heldOrder] of held) send(r, input.releasePending ? heldOrder.snapshot : { ...order(heldOrder.org), status: 'PAID', qr_data: '', payment_url: '' }); held.clear(); }
      send(res, state()); return;
    }
    if (!path.startsWith('/api/')) { next(); return; }
    requests.push({ method: req.method, path, actor });
    if (req.method === 'POST' && /\/recharge\/orders$/.test(path)) {
      let body = ''; for await (const chunk of req) body += chunk;
      const facts = JSON.parse(body), key = req.headers['idempotency-key'];
      if (facts.product_id !== 'public-fixture-product' || facts.channel !== 'ALIPAY' || !key) { send(res, {}, 400); return; }
      if (denyCreate) { send(res, { code: 'CONTROLLED_CREATE_DENIAL' }, denyCreate); return; }
      const previous = createdKeys.get(key); if (previous && JSON.stringify(previous) !== JSON.stringify(facts)) { send(res, {}, 409); return; }
      createdKeys.set(key, facts); const org = path.match(/organizations\/([a-f0-9-]{36})/)?.[1] || '';
      if (unknownCreate) { unknownCreate = false; send(res, { code: 'CONTROLLED_UNKNOWN_AFTER_CREATE' }, 503); return; }
      if (holdCreate) { heldCreates.set(res, order(org)); res.on('close', () => heldCreates.delete(res)); return; }
      send(res, order(org), 201); return;
    }
    if (req.method !== 'GET') { send(res, { code: 'FIXTURE_READ_ONLY' }, 405); return; }
    const end = path.replace('/api/token', '');
    const scoped = end.match(/^\/v1\/organizations\/([a-f0-9-]{36})(\/.*)$/);
    if (scoped) {
      const [, org, route] = scoped;
      if (!['44444444-4444-4444-8444-444444444444', '55555555-5555-4555-8555-555555555555'].includes(org)) { send(res, {}, 404); return; }
      if (denyWallet && ['/balance', '/entitlement-packages'].includes(route)) { send(res, { code: 'CONTROLLED_WALLET_DENIAL' }, denyWallet); return; }
      if (denyPurchase && ['/recharge/products', '/payment-capabilities'].includes(route)) { send(res, { code: 'CONTROLLED_PURCHASE_DENIAL' }, denyPurchase); return; }
      if (route === '/recharge/orders/' + id) {
        if (denyDetail) { send(res, { code: 'CONTROLLED_DENIAL' }, denyDetail); return; }
        if (hold) { held.set(res, { org, snapshot: order(org) }); res.on('close', () => held.delete(res)); } else send(res, order(org));
      } else if (route === '/balance') send(res, { owner_type: 'ORGANIZATION', owner_id: org, wallet_id: '66666666-6666-4666-8666-666666666666', posted_microcredits: status === 'PAID' ? '100000000' : '0', reserved_microcredits: '0', available_microcredits: status === 'PAID' ? '100000000' : '0' });
      else if (route === '/entitlement-packages') send(res, { items: [] });
      else if (route === '/recharge/products') send(res, { items: [{ product_id: 'public-fixture-product', availability_status: 'AVAILABLE', name: 'Public fixture plan', amount_fen: '1234', channels: ['ALIPAY'] }] });
      else if (route === '/payment-capabilities') send(res, { channels: ['ALIPAY'], test_only: true, formal_collection_admitted: false });
      else send(res, {}, 404);
      return;
    }
    if (end === '/v1/recharge/products') send(res, { items: [{ product_id: 'public-fixture-product', availability_status: 'AVAILABLE', name: 'Public fixture plan', amount_fen: '1234', channels: ['ALIPAY'] }] });
    else if (end === '/v1/payment-capabilities') send(res, { channels: ['ALIPAY'], test_only: true, formal_collection_admitted: false });
    else if (end === '/v1/recharge/orders') {
      const snapshot = { items: actor === 'A' ? [historyOrder()] : [] };
      if (holdList && !denyList) { heldLists.set(res, snapshot); res.on('close', () => heldLists.delete(res)); }
      else send(res, denyList ? { code: 'CONTROLLED_DENIAL' } : snapshot, denyList || 200);
    }
    else if (end === '/v1/me/entitlement-packages') send(res, { items: [] });
    else if (end === '/v1/recharge/orders/' + id) {
      if (denyDetail) { send(res, { code: 'CONTROLLED_DENIAL' }, denyDetail); return; }
      if (hold) { held.set(res, { org: '', snapshot: order() }); res.on('close', () => held.delete(res)); }
      else send(res, order());
    } else send(res, { code: 'FIXTURE_NOT_FOUND' }, 404);
  };
let vite; let httpServer; let cleanupPromise;
const sockets = new Set();
const apiTasks = new Set();
const handleFixtureAPI = (request, response, next) => {
  // Connect does not consume a Promise returned by async middleware. Own each
  // request's completion before cancellation can reject its body iterator.
  const task = fixtureAPI(request, response, next).catch(reason => {
    if (!request.socket.destroyed && !response.destroyed) next(reason);
  }).finally(() => apiTasks.delete(task));
  apiTasks.add(task);
  // A dispatch failure must also be observed if an error middleware throws.
  task.catch(reason => { console.error(reason); response.destroy(); });
};
const cleanup = () => {
  if (cleanupPromise) return cleanupPromise;
  cleanupPromise = (async () => {
    for (const response of [...held.keys(), ...heldLists.keys(), ...heldCreates.keys()]) response.destroy();
    held.clear(); heldLists.clear(); heldCreates.clear();
    const errors = [];
    const closeHttp = async () => {
      if (!httpServer) return;
      for (const socket of sockets) socket.destroy();
      await new Promise((resolveClose, rejectClose) => httpServer.close(reason => {
        if (reason && reason.code !== 'ERR_SERVER_NOT_RUNNING') rejectClose(reason); else resolveClose();
      }));
    };
    try { await closeHttp(); } catch (reason) { errors.push(reason); }
    for (const result of await Promise.allSettled([...apiTasks]))
      if (result.status === 'rejected') errors.push(result.reason);
    // Vite's close snapshots its optimizer cancellation before awaiting a scan.
    // Drain that existing scan first so a late optimizer cannot recreate the
    // retired cache after close has resolved. HTTP accepts no new work now.
    for (const result of await Promise.allSettled(Object.values(vite?.environments ?? {})
      .map(async environment => {
        await environment.depsOptimizer?.scanProcessing;
        await environment.waitForRequestsIdle?.();
        await Promise.all(Object.values(environment.depsOptimizer?.metadata.discovered ?? {})
          .map(dependency => dependency.processing));
      })))
      if (result.status === 'rejected') errors.push(result.reason);
    try { await vite?.close(); } catch (reason) { errors.push(reason); }
    try { removeProjection(); } catch (reason) { errors.push(reason); }
    if (errors.length) throw new AggregateError(errors, 'FIXTURE_CLEANUP_FAILED');
  })();
  return cleanupPromise;
};
try {
  // The fixture owns HTTP and process lifetime. Middleware mode prevents Vite
  // from installing SIGTERM/stdin handlers that call process.exit before this
  // projection has been retired. WebSocket traffic shares the owned listener.
  httpServer = createHttpServer();
  httpServer.on('connection', socket => { sockets.add(socket); socket.once('close', () => sockets.delete(socket)); });
  vite = await createViteServer({ root, configFile: false, appType: 'mpa', clearScreen: false, logLevel: 'error', cacheDir: cache,
    plugins: [react(), { name: 'public-loopback-fixture-api', configureServer(server) { server.middlewares.use(handleFixtureAPI); } }], resolve: { alias: { react: dirname(require.resolve('react/package.json')), 'react-dom': dirname(require.resolve('react-dom/package.json')) } },
    optimizeDeps: { entries: ['tests/fixtures/account-platform-recharge-polling.html'] },
    server: { host: '127.0.0.1', port, strictPort: true, middlewareMode: true, ws: { server: httpServer }, watch: null,
      fs: { allow: [root, dirname(require.resolve('react/package.json')), dirname(require.resolve('react-dom/package.json'))] } } });
  httpServer.on('request', (request, response) => vite.middlewares(request, response));
  await new Promise((resolveListen, rejectListen) => {
    const failed = reason => { httpServer.off('listening', ready); rejectListen(reason); };
    const ready = () => { httpServer.off('error', failed); resolveListen(); };
    httpServer.once('error', failed); httpServer.once('listening', ready);
    httpServer.listen(port, '127.0.0.1');
  });
} catch (reason) {
  const errors = [reason];
  try { await cleanup(); } catch (cleanupReason) {
    errors.push(cleanupReason);
  }
  if (errors.length > 1) throw new AggregateError(errors, 'FIXTURE_STARTUP_CLEANUP_FAILED', { cause: reason });
  throw reason;
}
return { url: 'http://127.0.0.1:' + httpServer.address().port + '/tests/fixtures/account-platform-recharge-polling.html', rootPath: root, close: cleanup };
}
