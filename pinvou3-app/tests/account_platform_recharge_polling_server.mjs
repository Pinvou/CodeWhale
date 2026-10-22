// Public loopback HTTP fixture. Never connects to any account service or database.
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { dirname, join, resolve } from 'node:path';
import { copyFileSync, cpSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
const require = createRequire(new URL('../package.json', import.meta.url));
const { createServer } = await import(pathToFileURL(require.resolve('vite')).href);
const react = (await import(pathToFileURL(require.resolve('@vitejs/plugin-react')).href)).default;
const inputRoot = fileURLToPath(new URL('../', import.meta.url));
// Vite correctly denies .git inputs. Serve only a fresh public source projection
// outside .git; keep the default deny list and exact filesystem allow-list.
const root = mkdtempSync(join(tmpdir(), 'pinvou-public-tok03-app-'));
for (const part of ['src/features/account-platform', 'src/shared'])
  cpSync(join(inputRoot, part), join(root, part), { recursive: true, dereference: false });
mkdirSync(join(root, 'tests/fixtures'), { recursive: true });
copyFileSync(join(inputRoot, 'package.json'), join(root, 'package.json'));
for (const name of ['account-platform-recharge-polling.html', 'account-platform-recharge-polling.jsx'])
  copyFileSync(join(inputRoot, 'tests/fixtures', name), join(root, 'tests/fixtures', name));
const cache = join(root, '.vite-cache');
const port = Number(process.env.PINVOU_TEST_TOK03_PORT || 0);
if (!Number.isSafeInteger(port) || port < 0 || port > 65535) throw new Error('FIXTURE_PORT_INVALID');
const id = '33333333-3333-4333-8333-333333333333';
let actor = 'A', status = 'PENDING', hold = false, created = Date.now(), requests = [], held = new Map();
const order = (org = '') => ({ ...(org ? { beneficiary_organization_id: org } : {}), order_id: id, product_id: 'public-fixture-product', plan_name: 'Public fixture plan', amount_fen: '1234', status,
  close_status: 'OPEN', fulfillment_status: status === 'PAID' ? 'CREDITED' : 'PENDING', expires_at: new Date(created + 120000).toISOString(), created_at: new Date(created).toISOString(),
  payment_url: '', qr_data: status === 'PENDING' ? 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aNbQAAAAASUVORK5CYII=' : '' });
const send = (r, body, code = 200) => { r.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); r.end(JSON.stringify(body)); };
const state = () => ({ actor, status, hold, held: held.size, requests, detailReads: requests.filter(item => item.path.endsWith('/' + id)).length,
  walletReads: requests.filter(item => item.path.endsWith('/balance')).length, packageReads: requests.filter(item => item.path.endsWith('/entitlement-packages')).length,
  productMutations: requests.filter(item => item.method !== 'GET').length });
const fixtureAPI = async (req, res, next) => {
    const path = req.url.split('?')[0];
    if (path === '/__fixture__/state') { send(res, state()); return; }
    if (path === '/__fixture__/control' && req.method === 'POST') {
      let body = ''; for await (const chunk of req) { body += chunk; if (body.length > 4096) { send(res, { error: 'TOO_LARGE' }, 413); return; } }
      const input = JSON.parse(body);
      if (input.reset) { actor = 'A'; status = 'PENDING'; hold = false; created = Date.now(); requests = []; for (const r of held.keys()) r.destroy(); held.clear(); }
      if (input.actor === 'B') actor = 'B'; if (input.status === 'PAID') status = 'PAID'; if (input.hold === true) hold = true;
      if (input.release) { hold = false; for (const [r, org] of held) send(r, { ...order(org), status: 'PAID', qr_data: '', payment_url: '' }); held.clear(); }
      send(res, state()); return;
    }
    if (!path.startsWith('/api/')) { next(); return; }
    requests.push({ method: req.method, path, actor });
    if (req.method !== 'GET') { send(res, { code: 'FIXTURE_READ_ONLY' }, 405); return; }
    const end = path.replace('/api/token', '');
    const scoped = end.match(/^\/v1\/organizations\/([a-f0-9-]{36})(\/.*)$/);
    if (scoped) {
      const [, org, route] = scoped;
      if (!['44444444-4444-4444-8444-444444444444', '55555555-5555-4555-8555-555555555555'].includes(org)) { send(res, {}, 404); return; }
      if (route === '/recharge/orders/' + id) {
        if (hold) { held.set(res, org); res.on('close', () => held.delete(res)); } else send(res, order(org));
      } else if (route === '/balance') send(res, { owner_type: 'ORGANIZATION', owner_id: org, wallet_id: '66666666-6666-4666-8666-666666666666', posted_microcredits: status === 'PAID' ? '100000000' : '0', reserved_microcredits: '0', available_microcredits: status === 'PAID' ? '100000000' : '0' });
      else if (route === '/entitlement-packages') send(res, { items: [] });
      else if (route === '/recharge/products') send(res, { items: [{ product_id: 'public-fixture-product', name: 'Public fixture plan', amount_fen: '1234', channels: ['ALIPAY'] }] });
      else if (route === '/payment-capabilities') send(res, { channels: ['ALIPAY'], test_only: true, formal_collection_admitted: false });
      else send(res, {}, 404);
      return;
    }
    if (end === '/v1/recharge/products') send(res, { items: [{ product_id: 'public-fixture-product', name: 'Public fixture plan', amount_fen: '1234', channels: ['ALIPAY'] }] });
    else if (end === '/v1/payment-capabilities') send(res, { channels: ['ALIPAY'], test_only: true, formal_collection_admitted: false });
    else if (end === '/v1/recharge/orders') send(res, { items: actor === 'A' ? [order()] : [] });
    else if (end === '/v1/me/entitlement-packages') send(res, { items: [] });
    else if (end === '/v1/recharge/orders/' + id) {
      if (hold) { held.set(res, ''); res.on('close', () => held.delete(res)); }
      else send(res, order());
    } else send(res, { code: 'FIXTURE_NOT_FOUND' }, 404);
  };
let vite;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', clearScreen: false, logLevel: 'error', cacheDir: cache,
    plugins: [react(), { name: 'public-loopback-fixture-api', configureServer(server) { server.middlewares.use(fixtureAPI); } }], resolve: { alias: { react: dirname(require.resolve('react/package.json')), 'react-dom': dirname(require.resolve('react-dom/package.json')) } },
    optimizeDeps: { entries: ['tests/fixtures/account-platform-recharge-polling.html'] },
    server: { host: '127.0.0.1', port, strictPort: true, watch: null, fs: { allow: [root, dirname(require.resolve('react/package.json')), dirname(require.resolve('react-dom/package.json')), dirname(require.resolve('scheduler/package.json'))] } } });

  await vite.listen();
  console.log(JSON.stringify({ url: `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-recharge-polling.html`,
    statePath: '/__fixture__/state', publicSourceRoot: root, scope: 'Actual React+production component+actual BFF client; public disposable loopback HTTP only; no real accounts' }));
} catch (reason) { if (vite) await vite.close(); throw reason; }
const cleanup = async () => { for (const r of held.keys()) r.destroy(); held.clear(); await vite.close();
  if (dirname(resolve(root)) !== resolve(tmpdir()) || !root.split(/[/\\]/).pop().startsWith('pinvou-public-tok03-app-')) throw new Error('FIXTURE_CACHE_TARGET_INVALID');
  rmSync(root, { recursive: true, force: true }); };
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => { void cleanup().finally(() => process.exit(0)); });
