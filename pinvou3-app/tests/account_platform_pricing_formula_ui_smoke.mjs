import assert from 'node:assert/strict';
import { createServer } from './helpers/http-fixture.mjs';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { build } from './helpers/vite-fixture.mjs';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';

// The optional Vite plugins let an isolated reviewer compile a candidate without
// writing repository sources. Normal CI uses the production files directly.
export async function runPricingFormulaSmoke({ plugins = [], fixturePath } = {}) {
  const app = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
  const repository = path.dirname(app);
  const producer = JSON.parse(
    fs.readFileSync(
      fixturePath ||
        path.join(
          repository,
          'account-platform/test/contract/fixtures/pricing-producer-pages.json',
        ),
      'utf8',
    ),
  );
  const chrome =
    process.env.CHROME ||
    [
      'C:/Program Files/Google/Chrome/Application/chrome.exe',
      'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
      '/usr/bin/chromium',
      '/usr/bin/google-chrome',
    ].find(fs.existsSync);
  assert.ok(chrome, 'Browser unavailable; pricing UI validation not performed');
  const owned = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-pricing-formula-'));
  const realOwned = fs.realpathSync(owned);
  const id = (number) => `00000000-0000-4000-8000-${String(number).padStart(12, '0')}`;
  const targetID = id(3001);
  const unitID = id(401);
  const rows = Array.from({ length: 3001 }, (_, index) => ({
    ...producer['formula-page-one'].body.items[0],
    formula_version_id: id(index + 1),
    version: 3001 - index,
  }));
  const draft = {
    price_version_id: id(501),
    scope_type: 'GLOBAL',
    scope_id: null,
    protocol_code: 'PUBLIC_PROTOCOL',
    formula_version_id: targetID,
    credit_unit: 'MICROCREDIT',
    lifecycle_status: 'DRAFT',
    derived_status: 'DRAFT',
    version: 1,
    space_etag: '9007199254740993',
    effective_from: '2099-01-01T00:00:00+08:00',
    effective_to: null,
    units: [
      { unit_definition_id: unitID, config_type: 'FIXED_PRICE', fixed_price_per_denominator: '25' },
    ],
  };
  const entryID = path.join(app, 'src/pricing-formula-smoke-entry.jsx').replaceAll('\\', '/');
  const entry = `
import React from 'react';
import { createRoot } from 'react-dom/client';
import { PricingLifecycleWorkspace } from './features/account-platform/PricingLifecycleWorkspace.jsx';
import { AdminPlatformClient } from './features/account-platform/admin-client.js';
import { adminPlatformCopy, tokenManagementCopy } from './shared/i18n.js';
const root = createRoot(document.getElementById('root'));
const client = new AdminPlatformClient();
const originalRead = client.pricingProtocols.bind(client);
const control = window.formulaSmoke = { labels: tokenManagementCopy.en.pricingLifecycle,
  reads: [], canManage: true, render: null };
client.pricingProtocols = (filters, signal) => {
  const read = { filters, aborted: signal.aborted }; control.reads.push(read);
  signal.addEventListener('abort', () => { read.aborted = true; }, { once: true });
  // Model a transport/cache completion that ignores cancellation. The real
  // client still decodes each controlled HTTP response; generation must guard UI.
  return originalRead(filters);
};
control.render = () => root.render(<PricingLifecycleWorkspace
  copy={{...adminPlatformCopy.en, billing:{...adminPlatformCopy.en.billing,
    pricingLifecycle: control.labels}}} client={client}
  permissions={['platform.pricing.read', ...(control.canManage ? ['platform.pricing.manage'] : [])]}
  owner="${id(101)}" />);
client.session().then(control.render);
`;
  let active;
  let browser;
  let server;
  const failures = [];
  const errors = [];
  const external = [];
  const passed = [];
  const respond = (response, body, status = 200) => {
    response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
    response.end(JSON.stringify(body));
  };
  try {
    await build({
      root: app,
      configFile: false,
      logLevel: 'warn',
      plugins: [
        ...plugins,
        {
          name: 'pricing-formula-smoke-entry',
          resolveId(value) {
            if (value === 'pricing-formula-smoke') return entryID;
          },
          load(value) {
            if (value === entryID) return entry;
          },
        },
      ],
      build: {
        outDir: path.join(owned, 'static'),
        target: 'es2021',
        rollupOptions: { input: 'pricing-formula-smoke', output: { entryFileNames: 'app.js' } },
      },
    });
    server = createServer(async (request, response) => {
      const scenario = active;
      try {
        const url = new URL(request.url, 'http://fixture.invalid');
        if (url.pathname === '/') {
          response.writeHead(200, { 'Content-Type': 'text/html' });
          response.end('<div id="root"></div><script type="module" src="/app.js"></script>');
          return;
        }
        if (url.pathname === '/app.js') {
          response.writeHead(200, { 'Content-Type': 'text/javascript' });
          response.end(fs.readFileSync(path.join(owned, 'static/app.js')));
          return;
        }
        if (url.pathname === '/favicon.ico') {
          response.writeHead(204);
          response.end();
          return;
        }
        scenario.calls.push({
          path: url.pathname,
          query: Object.fromEntries(url.searchParams),
          method: request.method,
          idempotencyKey: request.headers['idempotency-key'] || null,
        });
        if (
          url.pathname.endsWith('/pricing/drafts') &&
          request.method === 'POST' &&
          scenario.allowDraftWrites
        ) {
          let body = '';
          for await (const chunk of request) body += chunk;
          scenario.calls.at(-1).body = body;
          scenario.draftWrites = (scenario.draftWrites || 0) + 1;
          // A lost response is not evidence of no effect; recovery must replay.
          return respond(
            response,
            scenario.draftWrites === 1
              ? { code: 'CONTROLLED_UNKNOWN' }
              : { ...draft, ...JSON.parse(body) },
            scenario.draftWrites === 1 ? 503 : 201,
          );
        }
        assert.equal(request.method, 'GET', 'read race must dispatch zero domain writes');
        if (url.pathname === '/api/session')
          return respond(response, { authenticated: true, csrf_token: 'PUBLIC_SYNTHETIC_CSRF' });
        if (url.pathname.endsWith('/protocols'))
          return respond(response, producer['protocol-page-one'].body);
        if (url.pathname.endsWith('/metering-units'))
          return respond(response, {
            items: [
              {
                unit_definition_id: unitID,
                unit_code: 'INPUT',
                version: 1,
                status: 'ACTIVE',
                minimum_precision: '1',
                price_denominator: '1000000',
                rounding_rule: 'CEILING',
              },
            ],
          });
        if (url.pathname.endsWith('/pricing')) {
          if (scenario.holdPreflight && url.searchParams.has('formula_version_id')) {
            await new Promise((resolve) => {
              scenario.releasePreflight = resolve;
            });
          }
          return respond(response, {
            items: [{ ...draft, formula_version_id: scenario.cached ? id(1) : targetID }],
            current_space_etag: '9007199254740993',
            has_more: false,
          });
        }
        assert.ok(url.pathname.endsWith('/formulas'), `unexpected fixture route ${url.pathname}`);
        scenario.formulaCalls++;
        assert.ok(scenario.formulaCalls <= 60, 'existing per-minute resource quota');
        const cursor = url.searchParams.get('cursor');
        const query = url.searchParams.get('q') || '';
        if (scenario.preflightDenial && query === 'DENIED') {
          return respond(response, { code: 'PERMISSION_DENIED' }, 403);
        }
        const start = cursor ? Number(cursor.split(':')[1]) : 0;
        if (cursor && cursor.split(':')[0] !== String(scenario.snapshot)) {
          return respond(response, { code: 'CURSOR_EXPIRED' }, 410);
        }
        if (!cursor) scenario.snapshot++;
        const filtered = query
          ? rows.filter(
              (row) =>
                row.formula_version_id === query || row.protocol_code === query.toUpperCase(),
            )
          : rows;
        const end = Math.min(start + 50, filtered.length);
        const page = {
          ...producer['formula-page-one'].body,
          items: filtered.slice(start, end),
          has_more: end < filtered.length,
          next_cursor: end < filtered.length ? `${scenario.snapshot}:${end}` : null,
        };
        const hold = scenario.delay === 'page' ? Boolean(cursor) : query === targetID;
        if (hold && !scenario.held) {
          scenario.held = true;
          await new Promise((resolve) => {
            scenario.release = resolve;
          });
          if (scenario.lateStatus)
            return respond(
              response,
              { code: scenario.lateStatus === 410 ? 'CURSOR_EXPIRED' : 'PERMISSION_DENIED' },
              scenario.lateStatus,
            );
        }
        return respond(response, page);
      } catch (error) {
        failures.push(error.stack);
        respond(response, { code: 'FIXTURE_FAILURE' }, 500);
      }
    });
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
    const origin = `http://127.0.0.1:${server.address().port}`;
    browser = await puppeteer.launch({
      executablePath: chrome,
      headless: true,
      userDataDir: path.join(owned, 'profile'),
      args: ['--disable-gpu', '--no-first-run', '--disable-background-networking'],
    });
    const click = async (page, key) =>
      assert.equal(
        await page.evaluate((key) => {
          const label = window.formulaSmoke.labels[key];
          const button = [...document.querySelectorAll('button')].find(
            (item) => item.textContent.trim() === label && !item.disabled,
          );
          if (!button) return false;
          button.click();
          return true;
        }, key),
        true,
        `enabled ${key}`,
      );
    const settle = (page) => page.waitForNetworkIdle({ idleTime: 100 });
    const filter = async (page, query) => {
      await page.$eval(
        '.adm-finance-subpanel input',
        (input, query) => {
          Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(
            input,
            query,
          );
          input.dispatchEvent(new Event('input', { bubbles: true }));
        },
        query,
      );
      await click(page, 'formulaSearchAction');
    };
    const waitHeld = async () => {
      const until = Date.now() + 5000;
      while (!active.release) {
        assert.ok(Date.now() < until, 'delayed read reached server');
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
    };
    const assertFilter = (page, query) =>
      page.waitForFunction(
        (query) => document.querySelector('.adm-finance-subpanel > p')?.textContent.includes(query),
        {},
        query,
      );
    const clean = async (page) => {
      assert.deepEqual(failures, []);
      assert.deepEqual(errors, []);
      assert.deepEqual(external, []);
      if (active.preflightDenial || active.allowDraftWrites) return;
      assert.equal(
        await page.evaluate(() => [...document.querySelectorAll('[role=alert]')].length),
        0,
        'late errors have no current authority',
      );
      assert.ok(
        await page.evaluate(() => window.formulaSmoke.reads.some((read) => read.aborted)),
        'superseded read was cancelled',
      );
      assert.equal(active.calls.filter((call) => call.method !== 'GET').length, 0);
    };
    const run = async (name, options, action) => {
      if (process.env.PRICING_SMOKE_CASE && !new RegExp(process.env.PRICING_SMOKE_CASE).test(name))
        return;
      active = { name, calls: [], snapshot: 0, formulaCalls: 0, ...options };
      const context = await browser.createBrowserContext();
      const page = await context.newPage();
      page.setDefaultTimeout(7000);
      page.on('pageerror', (error) => errors.push(error.message));
      await page.setRequestInterception(true);
      page.on('request', (request) => {
        if (!request.url().startsWith('data:') && new URL(request.url()).origin !== origin) {
          external.push(request.url());
          void request.abort();
        } else void request.continue();
      });
      try {
        await page.goto(origin, { waitUntil: 'networkidle0' });
        await page.waitForSelector('tbody tr');
        await action(page);
        await clean(page);
        passed.push(name);
        console.log(`PASS ${name}`);
      } catch (error) {
        console.error(
          JSON.stringify({
            scenario: name,
            calls: active.calls,
            ui: await page.evaluate(() => ({
              text: document.body.textContent,
              reads: window.formulaSmoke?.reads,
            })),
          }),
        );
        throw error;
      } finally {
        active.release?.();
        active.releasePreflight?.();
        await context.close();
      }
    };
    for (const lateStatus of [0, 403, 410]) {
      await run(
        `older-page-${lateStatus || 'success'}-cannot-replace-draft-lookup`,
        { delay: 'page', lateStatus },
        async (page) => {
          await click(page, 'formulaNext');
          await waitHeld();
          await click(page, 'editDraft');
          await page.waitForSelector('.adm-pricing-editor');
          await assertFilter(page, targetID);
          active.release();
          await settle(page);
          await assertFilter(page, targetID);
          assert.equal(
            await page.$$eval('.adm-pricing-fields select', (selects) => selects.at(-1).value),
            targetID,
          );
          assert.equal(
            await page.evaluate(
              () =>
                [...document.querySelectorAll('button')].find(
                  (button) => button.textContent === window.formulaSmoke.labels.formulaNext,
                ).disabled,
            ),
            true,
          );
          assert.equal(active.formulaCalls, 3);
        },
      );
      await run(
        `older-lookup-${lateStatus || 'success'}-cannot-replace-new-filter`,
        { delay: 'lookup', lateStatus },
        async (page) => {
          await click(page, 'editDraft');
          await waitHeld();
          await filter(page, 'PUBLIC_PROTOCOL');
          await assertFilter(page, 'PUBLIC_PROTOCOL');
          active.release();
          await settle(page);
          assert.equal(
            await page.$('.adm-pricing-editor'),
            null,
            'retired lookup cannot open its editor',
          );
          await assertFilter(page, 'PUBLIC_PROTOCOL');
          assert.equal(
            await page.evaluate(
              () =>
                [...document.querySelectorAll('button')].find(
                  (button) => button.textContent === window.formulaSmoke.labels.createDraft,
                ).disabled,
            ),
            false,
            'retired denial cannot revoke newer selection authority',
          );
          await click(page, 'formulaNext');
          await settle(page);
          assert.equal(
            active.calls.filter((call) => call.path.endsWith('/formulas')).at(-1).query.cursor,
            '3:50',
          );
          await click(page, 'createDraft');
          await page.waitForSelector('.adm-pricing-editor');
        },
      );
    }
    await run('older-lookup-cannot-replace-explicit-restart', { delay: 'lookup' }, async (page) => {
      await click(page, 'editDraft');
      await waitHeld();
      await click(page, 'formulaRestart');
      await page.waitForFunction(
        () => !document.querySelector('.adm-finance-subpanel input').disabled,
      );
      active.release();
      await settle(page);
      assert.equal(await page.$('.adm-pricing-editor'), null);
      await click(page, 'formulaNext');
      await settle(page);
    });
    await run(
      'older-lookup-cannot-replace-workspace-refresh',
      { delay: 'lookup' },
      async (page) => {
        await click(page, 'editDraft');
        await waitHeld();
        await page.$eval('.adm-section-tools .adm-row-actions button:last-child', (button) => {
          if (button.disabled) throw new Error('refresh unavailable');
          button.click();
        });
        await page.waitForFunction(
          () => !document.querySelector('.adm-finance-subpanel input').disabled,
        );
        active.release();
        await settle(page);
        assert.equal(await page.$('.adm-pricing-editor'), null);
        await click(page, 'formulaNext');
        await settle(page);
      },
    );
    await run(
      'cached-edit-retires-older-page-without-another-catalogue-read',
      { delay: 'page', cached: true },
      async (page) => {
        await click(page, 'formulaNext');
        await waitHeld();
        await click(page, 'editDraft');
        await page.waitForSelector('.adm-pricing-editor');
        active.release();
        await settle(page);
        assert.equal(active.formulaCalls, 2);
        assert.equal(
          await page.$$eval('.adm-pricing-fields select', (selects) => selects.at(-1).value),
          id(1),
        );
        assert.equal(
          await page.$$eval('.adm-pricing-fields select option', (options) =>
            options.some((option) => option.value.endsWith('000000000051')),
          ),
          false,
          'retired page did not commit',
        );
      },
    );
    const submitNewDraft = async (page) => {
      await click(page, 'createDraft');
      await page.waitForSelector('.adm-pricing-editor');
      for (const [selector, value] of [
        ['.adm-pricing-editor input[type=date]', '2099-01-01'],
        ['.adm-pricing-unit-configs input[type=number]', '25'],
      ]) {
        await page.$eval(
          selector,
          (input, value) => {
            Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(
              input,
              value,
            );
            input.dispatchEvent(new Event('input', { bubbles: true }));
          },
          value,
        );
      }
      await page.$eval('.adm-pricing-editor', (form) =>
        form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })),
      );
    };
    const waitPreflight = async () => {
      const until = Date.now() + 5000;
      while (!active.releasePreflight) {
        assert.ok(Date.now() < until, 'new draft reached read-only price-space preflight');
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
    };
    for (const refreshAfterDenial of [false, true]) {
      await run(
        `preparing-draft-denial${refreshAfterDenial ? '-then-refresh' : ''}-prevents-dispatch`,
        { holdPreflight: true, preflightDenial: true, allowDraftWrites: true },
        async (page) => {
          await submitNewDraft(page);
          await waitPreflight();
          await filter(page, 'DENIED');
          await page.waitForFunction(() => !document.querySelector('.adm-pricing-editor'));
          assert.equal(active.calls.filter((call) => call.query.q === 'DENIED').length, 1);
          if (refreshAfterDenial) {
            await filter(page, 'PUBLIC_PROTOCOL');
            await assertFilter(page, 'PUBLIC_PROTOCOL');
            await page.waitForFunction(
              () => !document.querySelector('.adm-finance-subpanel input').disabled,
            );
          }
          active.releasePreflight();
          await settle(page);
          assert.equal(
            active.calls.filter((call) => call.method === 'POST').length,
            0,
            'a current denial permanently retires this still-undispatched preparation',
          );
          assert.equal(
            await page.evaluate(() =>
              // Executes in the browser page, not Node Web Storage.
              // eslint-disable-next-line n/no-unsupported-features/node-builtins
              Object.keys(sessionStorage).some((key) =>
                key.startsWith('pinvou.admin.pricing-unknown-mutation:'),
              ),
            ),
            false,
            'an undispatched preparation must not leave a recoverable write',
          );
        },
      );
    }
    await run(
      'dispatched-unknown-keeps-original-input-and-key-after-directory-denial',
      { delay: 'page', lateStatus: 403, allowDraftWrites: true },
      async (page) => {
        await click(page, 'formulaNext');
        await waitHeld();
        await submitNewDraft(page);
        await page.waitForFunction(() =>
          document.body.textContent.includes(window.formulaSmoke.labels.resultUnknown),
        );
        const first = active.calls.find((call) => call.method === 'POST');
        assert.ok(first?.idempotencyKey, 'dispatch records its nonempty command key');
        assert.ok(first.body, 'dispatch records the exact command facts');
        active.release();
        await settle(page);
        assert.equal(active.calls.filter((call) => call.method === 'POST').length, 1);
        await click(page, 'verifyResult');
        await page.waitForFunction(() =>
          document.body.textContent.includes(window.formulaSmoke.labels.recoveryConfirmed),
        );
        const writes = active.calls.filter((call) => call.method === 'POST');
        assert.equal(writes.length, 2);
        assert.equal(writes[1].idempotencyKey, first.idempotencyKey);
        assert.deepEqual(writes[1].body, first.body);
        assert.equal(
          await page.evaluate(() =>
            document.body.textContent.includes(window.formulaSmoke.labels.resultUnknown),
          ),
          false,
        );
      },
    );
    assert.ok(passed.length, 'no pricing scenarios selected');
    console.log(`RESULT ${passed.length} PASS 0 FAIL 0 SKIP`);
    return { passed, failures, errors, external };
  } finally {
    active?.release?.();
    active?.releasePreflight?.();
    if (browser) await browser.close();
    if (server) await new Promise((resolve) => server.close(resolve));
    assert.equal(fs.realpathSync(owned), realOwned);
    assert.equal(path.dirname(realOwned), fs.realpathSync(os.tmpdir()));
    assert.ok(path.basename(realOwned).startsWith('pinvou-pricing-formula-'));
    fs.rmSync(realOwned, { recursive: true });
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  await runPricingFormulaSmoke();
}
