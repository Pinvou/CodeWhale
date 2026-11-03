import assert from 'node:assert/strict';
import { createServer } from './helpers/http-fixture.mjs';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createInterface } from 'node:readline';
import { build } from './helpers/vite-fixture.mjs';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
export async function runManagedPriceSmoke({ plugins = [], serveOnly = false } = {}) {
  const app = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
  const fixture = JSON.parse(
    fs.readFileSync(
      path.join(app, '../account-platform/test/contract/fixtures/managed-price-pages.json'),
    ),
  );
  const first = fixture.first,
    second = fixture.second,
    active = second.items[0],
    unitIDs = active.units.map((u) => u.unit_definition_id);
  const formula = {
    formula_version_id: active.formula_version_id,
    protocol_code: active.protocol_code,
    status: 'PUBLISHED',
    version: 1,
    required_unit_versions: Object.fromEntries(unitIDs.map((id, i) => ['UNIT_' + i, id])),
  };
  const owned = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-price-pages-'));
  const realOwned = fs.realpathSync(owned);
  let browser;
  let server;
  let cleanupPromise;
  const state = { calls: [], failNext: false, holdNext: false, release: null, writes: [] };
  // Every exit path owns exactly the temporary directory allocated above.
  const cleanup = () => {
    cleanupPromise ??= (async () => {
      state.release?.();
      state.release = null;
      try {
        await browser?.close();
      } finally {
        try {
          if (server?.listening) {
            const closed = new Promise((resolve, reject) =>
              server.close((error) => (error ? reject(error) : resolve())),
            );
            server.closeAllConnections();
            await closed;
          }
        } finally {
          assert.equal(fs.realpathSync(owned), realOwned);
          assert.equal(path.dirname(realOwned), fs.realpathSync(os.tmpdir()));
          assert.ok(path.basename(realOwned).startsWith('pinvou-price-pages-'));
          fs.rmSync(realOwned, { recursive: true });
        }
      }
    })();
    return cleanupPromise;
  };
  try {
    const entryID = path.join(app, 'src/managed-price-smoke-entry.jsx').replaceAll('\\', '/');
    const entry = `
import React from 'react';
import { createRoot } from 'react-dom/client';
import { PricingLifecycleWorkspace } from './features/account-platform/PricingLifecycleWorkspace.jsx';
import { AdminPlatformClient } from './features/account-platform/admin-client.js';
import { adminPlatformCopy, tokenManagementCopy } from './shared/i18n.js';

const client = new AdminPlatformClient();
const root = createRoot(document.getElementById('root'));
const control = window.priceSmoke = {
  labels: tokenManagementCopy.en.pricingLifecycle,
  manage: true,
};
control.render = () => root.render(
  <PricingLifecycleWorkspace
    copy={{
      ...adminPlatformCopy.en,
      billing: {
        ...adminPlatformCopy.en.billing,
        pricingLifecycle: control.labels,
      },
    }}
    client={client}
    permissions={[
      'platform.pricing.read',
      ...(control.manage ? ['platform.pricing.manage'] : []),
    ]}
    owner="public-fixture-owner"
  />
);
client.session().then(control.render);
`;
    await build({
      root: app,
      configFile: false,
      logLevel: 'warn',
      plugins: [
        ...plugins,
        {
          name: 'managed-price-entry',
          resolveId: (id) => (id === 'managed-price-smoke' ? entryID : undefined),
          load: (id) => (id === entryID ? entry : undefined),
        },
      ],
      build: {
        outDir: path.join(owned, 'static'),
        target: 'es2021',
        rollupOptions: { input: 'managed-price-smoke', output: { entryFileNames: 'app.js' } },
      },
    });
    const reply = (res, body, status = 200) => {
      res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
      res.end(JSON.stringify(body));
    };
    server = createServer(async (req, res) => {
      try {
        const u = new URL(req.url, 'http://fixture.invalid');
        if (u.pathname === '/') {
          res.setHeader('Content-Type', 'text/html');
          res.end('<div id="root"></div><script type="module" src="/app.js"></script>');
          return;
        }
        if (u.pathname === '/app.js') {
          res.setHeader('Content-Type', 'text/javascript');
          res.end(fs.readFileSync(path.join(owned, 'static/app.js')));
          return;
        }
        if (u.pathname === '/favicon.ico') {
          res.writeHead(204);
          res.end();
          return;
        }
        if (u.pathname === '/fixture/state')
          return reply(res, { ...state, release: Boolean(state.release) });
        if (u.pathname === '/fixture/fail-next') {
          state.failNext = true;
          return reply(res, {});
        }
        if (u.pathname === '/fixture/hold-next') {
          state.holdNext = true;
          return reply(res, {});
        }
        if (u.pathname === '/fixture/release') {
          state.release?.();
          state.release = null;
          return reply(res, {});
        }
        state.calls.push({
          path: u.pathname,
          query: Object.fromEntries(u.searchParams),
          method: req.method,
        });
        if (u.pathname === '/api/session')
          return reply(res, { authenticated: true, csrf_token: 'PUBLIC_SYNTHETIC_CSRF' });
        if (u.pathname.endsWith('/protocols'))
          return reply(res, { protocols: [], formulas: [formula] });
        if (u.pathname.endsWith('/metering-units'))
          return reply(res, {
            items: unitIDs.map((id, i) => ({
              unit_definition_id: id,
              unit_code: 'UNIT_' + i,
              version: 1,
              status: 'ACTIVE',
              minimum_precision: '1',
              price_denominator: '1000000',
              rounding_rule: 'CEILING',
            })),
          });
        if (u.pathname.endsWith('/preview'))
          return reply(res, { item: active, units: [], conflicts: [] });
        if (u.pathname.endsWith('/draft') && req.method === 'PUT') {
          let body = '';
          for await (const chunk of req) body += chunk;
          const id = u.pathname.split('/').at(-2);
          state.writes.push({ id, body: JSON.parse(body), key: req.headers['idempotency-key'] });
          return reply(res, {
            ...first.items.find((item) => item.price_version_id === id),
            ...JSON.parse(body),
          });
        }
        if (u.pathname.endsWith('/pricing')) {
          if (u.searchParams.has('price_version_id'))
            return reply(res, {
              item: [...first.items, ...second.items].find(
                (item) => item.price_version_id === u.searchParams.get('price_version_id'),
              ),
            });
          if (u.searchParams.has('scope_type'))
            return reply(res, {
              items: [...first.items, ...second.items],
              has_more: false,
              current_space_etag: active.current_space_etag,
            });
          if (u.searchParams.has('cursor')) {
            assert.equal(u.searchParams.get('cursor'), first.next_cursor);
            if (state.holdNext) {
              state.holdNext = false;
              await new Promise((done) => (state.release = done));
            }
            if (state.failNext) {
              state.failNext = false;
              return reply(res, { code: 'CONTROLLED_RETRY' }, 503);
            }
            return reply(res, second);
          }
          return reply(res, first);
        }
        return reply(res, { code: 'UNEXPECTED_ROUTE' }, 404);
      } catch (error) {
        console.error(error);
        reply(res, { code: 'FIXTURE_FAILURE' }, 500);
      }
    });
    await new Promise((done) => server.listen(0, '127.0.0.1', done));
    const origin = `http://127.0.0.1:${server.address().port}`;
    if (serveOnly) {
      console.log(
        JSON.stringify({
          origin,
          activeID: active.price_version_id,
          firstDraftID: first.items[0].price_version_id,
          mode: 'controlled-HTTP-real-React',
          owned,
          shutdown: 'Enter q then Return, or send SIGINT/SIGTERM; cleanup completes before exit.',
        }),
      );
      const input = createInterface({ input: process.stdin });
      let finish;
      const stopped = new Promise((resolve) => {
        finish = resolve;
      });
      const onLine = (line) => {
        if (line.trim().toLowerCase() === 'q') finish();
      };
      const onSignal = () => finish();
      input.on('line', onLine);
      process.on('SIGINT', onSignal);
      process.on('SIGTERM', onSignal);
      try {
        await stopped;
      } finally {
        input.removeListener('line', onLine);
        input.close();
        process.stdin.pause();
        process.removeListener('SIGINT', onSignal);
        process.removeListener('SIGTERM', onSignal);
      }
      return;
    }

    const chrome =
      process.env.CHROME ||
      [
        'C:/Program Files/Google/Chrome/Application/chrome.exe',
        'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
        '/usr/bin/chromium',
        '/usr/bin/google-chrome',
      ].find(fs.existsSync);
    assert.ok(chrome, 'Browser unavailable; UI not performed');
    browser = await puppeteer.launch({
      executablePath: chrome,
      headless: true,
      userDataDir: path.join(owned, 'profile'),
      args: ['--disable-gpu', '--no-first-run', '--disable-background-networking'],
    });
    const page = await browser.newPage();
    await page.goto(origin);
    await page.waitForFunction(() => document.querySelectorAll('tbody')[0]?.rows.length === 100);
    const click = (key) =>
      page.evaluate((key) => {
        const text = window.priceSmoke.labels[key];
        const b = [...document.querySelectorAll('button')].find((b) => b.textContent === text);
        if (!b || b.disabled) throw Error('unavailable ' + key);
        b.click();
      }, key);
    state.failNext = true;
    await click('priceNext');
    await page.waitForFunction(() =>
      [...document.querySelectorAll('button')].some(
        (b) => b.textContent === window.priceSmoke.labels.priceRetry,
      ),
    );
    assert.equal(await page.$$eval('tbody', (n) => n[0].rows.length), 100);
    await click('priceRetry');
    await page.waitForFunction(() => document.querySelectorAll('tbody')[0]?.rows.length === 1);
    assert.equal(
      await page.$eval('tbody tr [aria-label="ACTIVE"]', (n) => n.textContent),
      'ACTIVE',
    );
    await click('endActive');
    await page.waitForSelector('[role="dialog"]');
    assert.ok(
      (await page.$eval('[role="dialog"]', (n) => n.textContent)).includes(active.price_version_id),
    );
    assert.equal(
      await page.evaluate(
        () =>
          [...document.querySelectorAll('button')].find(
            (b) => b.textContent === window.priceSmoke.labels.pricePrevious,
          ).disabled,
      ),
      true,
    );
    await click('cancel');
    await click('pricePrevious');
    await page.waitForFunction(() => document.querySelectorAll('tbody')[0]?.rows.length === 100);
    await click('editDraft');
    await page.waitForSelector('form.adm-pricing-editor');
    assert.equal(
      await page.evaluate(
        () =>
          [...document.querySelectorAll('button')].find(
            (b) => b.textContent === window.priceSmoke.labels.priceNext,
          ).disabled,
      ),
      true,
    );
    await click('save');
    await page.waitForFunction(() => !document.querySelector('form.adm-pricing-editor'));
    assert.equal(state.writes.length, 1);
    assert.equal(state.writes[0].id, first.items[0].price_version_id);
    assert.ok(state.writes[0].key);
    console.log(
      'PASS managed-price real React retry/101 ACTIVE/original-ID action/back/edit-lock/save',
    );
  } finally {
    await cleanup();
  }
}
if (process.argv[1] === fileURLToPath(import.meta.url))
  await runManagedPriceSmoke({ serveOnly: process.argv.includes('--serve-only') });
