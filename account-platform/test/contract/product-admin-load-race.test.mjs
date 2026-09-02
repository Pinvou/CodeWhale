import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';

const rootPath = path.resolve(import.meta.dirname, '../..');

function response(data) {
  return { ok: true, status: 200, json: async () => data };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolveValue, rejectValue) => {
    resolve = resolveValue;
    reject = rejectValue;
  });
  return { promise, resolve, reject };
}

async function waitFor(predicate) {
  for (let attempt = 0; attempt < 30; attempt += 1) {
    if (predicate()) return;
    await new Promise((resolve) => setImmediate(resolve));
  }
  throw new Error('condition not reached');
}

test('recharge product lazy loader ignores an older failure after a newer success', async () => {
  const source = await readFile(path.join(rootPath, 'internal/portal/static/assets/product-admin.js'), 'utf8');
  const listeners = new Map();
  const elements = new Map();
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, { id, hidden: false, innerHTML: '', textContent: '', className: '', reset() {} });
    return elements.get(id);
  };
  const root = element('product-admin-root');
  root.hidden = true;
  root.addEventListener = (name, listener) => listeners.set(name, listener);
  root.closest = () => null;
  const oldCatalog = deferred();
  let catalogCalls = 0;
  const fetch = async (url) => {
    const value = String(url);
    if (value === '/api/session') return response({ authenticated: true, csrf_token: 'csrf' });
    if (value === '/api/account/v1/me/authorization-context') return response({ platform_permissions: ['platform.products.read'] });
    if (value.includes('/admin/v1/recharge-products?limit=200')) {
      catalogCalls += 1;
      if (catalogCalls === 1 || catalogCalls === 3 || catalogCalls === 5) return response({ items: [] });
      if (catalogCalls === 2) return oldCatalog.promise;
      if (catalogCalls === 4) throw new Error('current request failed');
    }
    throw new Error(`unexpected fetch: ${value}`);
  };
  const storage = { getItem: () => null, setItem() {}, removeItem() {} };
  const context = {
    console,
    fetch,
    localStorage: storage,
    sessionStorage: storage,
    crypto: { randomUUID: () => 'uuid' },
    FormData: class {},
    document: { getElementById: (id) => id === 'product-admin-root' ? root : element(id) },
    window: { addEventListener() {} },
    location: { assign() {}, reload() {} },
  };
  context.globalThis = context;
  vm.runInNewContext(source, context, { filename: 'product-admin.js' });

  listeners.get('pinvou-admin-extension-visibility')({ detail: { visible: true, authorizationVersion: 'v1' } });
  await waitFor(() => catalogCalls === 1 && element('product-list').innerHTML.includes('暂无商品'));

  const older = element('product-refresh').onclick();
  await waitFor(() => catalogCalls === 2);
  const newer = element('product-refresh').onclick();
  await newer;
  assert.equal(catalogCalls, 3);
  assert.match(element('product-list').innerHTML, /暂无商品/);

  oldCatalog.reject(new Error('older request failed'));
  await older;
  assert.equal(element('product-message').textContent, '');
  assert.equal(element('product-load-message').textContent, '');
  assert.match(element('product-list').innerHTML, /暂无商品/);

  await element('product-refresh').onclick();
  assert.equal(catalogCalls, 4);
  assert.match(element('product-load-message').textContent, /current request failed/);
  assert.equal(element('product-message').textContent, '');

  await element('product-refresh').onclick();
  assert.equal(catalogCalls, 5);
  assert.equal(element('product-load-message').textContent, '');
  assert.equal(element('product-message').textContent, '');
  assert.match(element('product-list').innerHTML, /暂无商品/);
});
