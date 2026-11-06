import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createServer } from './helpers/vite-fixture.mjs';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';

const root = path.resolve(import.meta.dirname, '..');
if (process.env.UI_ARTIFACT_DIR) fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
const chrome = [process.env.CHROME, 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(value => value && fs.existsSync(value));
assert.ok(chrome, 'Chromium required; validation cannot be skipped');
const server = await createServer({ root, configFile: false, logLevel: 'error', plugins: [react()], css: { postcss: { plugins: [] } }, server: { host: '127.0.0.1', port: 0 } });
await server.listen();
let browser, passed = 0, skipped = 0;
try {
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage(), errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
  const fresh = async (query = '') => {
    errors.length = 0;
    await page.setViewport({ width: 1440, height: 1000 });
    await page.goto(`${origin}/tests/fixtures/account-platform-restored-ui.html${query}`, { waitUntil: 'domcontentloaded', timeout: 60000 });
    await page.waitForSelector(query.includes('mode=admin') ? '.adm-restored-shell .adm-page-head' : '.ap-restored-portal .ap-product-picker');
  };
  const click = async (selector, text) => {
    const handle = await page.evaluateHandle(({ selector, text }) => {
      const button = [...document.querySelectorAll(selector)].find(node => node.textContent.trim() === text && node.getClientRects().length);
      return button && !button.disabled ? button : null;
    }, { selector, text });
    const button = handle.asElement();
    assert.ok(button, `visible enabled button missing: ${selector} ${text}`);
    await button.click(); await handle.dispose();
  };
  const accountTab = section => page.click(`.ap-sidebar [data-section="${section}"]`);
  const adminTab = key => page.click(`.adm-sidebar button[data-section="${key}"]`);
  const activeAccount = section => page.waitForSelector(`#ap-panel-${section}:not([hidden])`);
  const noOverflow = async () => {
    const result = await page.evaluate(() => ({ width: innerWidth, scroll: document.documentElement.scrollWidth, offenders: [...document.querySelectorAll('body *')].filter(node => node.getBoundingClientRect().right > innerWidth + 1 && getComputedStyle(node).position !== 'fixed').slice(0, 8).map(node => node.tagName + '.' + node.className) }));
    assert.ok(result.scroll <= result.width + 1, `page has horizontal overflow: ${JSON.stringify(result)}`);
  };
  const run = async (name, action) => {
    if (process.env.UI_CASE && !name.includes(process.env.UI_CASE)) { skipped++; return; }
    try { await action(); assert.deepEqual(errors, [], 'uncaught UI error'); console.log(`PASS ${name}`); passed++; }
    catch (error) { console.error(JSON.stringify({ case: name, pageErrors: errors, dialog: await page.evaluate(() => document.querySelector('[role=dialog]')?.textContent.slice(0, 1200)) })); throw error; }
  };
  await run('approved merged identity keeps localized controls and one settings card across sizes', async () => {
    for (const language of ['zh', 'en', 'ja']) {
      await fresh();
      await page.evaluate(language => {
        window.restored.language = language;
        window.restored.personal = ['personal.context.switch', 'personal.profile.update', 'personal.invitations.read', 'personal.invitations.accept'];
        window.restored.reload();
      }, language);
      await accountTab('accountSettings'); await activeAccount('accountSettings');
      await page.waitForSelector('.ap-current-context-row button:not(:disabled)');
      assert.deepEqual(await page.$eval('.ap-sidebar-switch', node => ({ label: node.getAttribute('aria-label'), title: node.title, popup: node.getAttribute('aria-haspopup'), text: node.textContent.trim() })), { label: await page.evaluate(() => window.restored.copy().center.switchIdentity), title: await page.evaluate(() => window.restored.copy().center.switchIdentity), popup: 'dialog', text: '' });
      assert.equal(await page.$$eval('.ap-sidebar .ap-identity-card', nodes => nodes.length), 1);
      assert.equal(await page.$$eval('#ap-panel-accountSettings > .ap-account-overview', nodes => nodes.length), 1);
      assert.equal(await page.$$eval('.ap-account-overview > .ap-account-information, .ap-account-overview > .ap-context-settings', nodes => nodes.length), 2);
      assert.equal(await page.$eval('.ap-account-overview > .ap-context-settings', node => getComputedStyle(node).borderTopWidth), '1px');
      assert.equal(await page.$eval('.ap-account-information', node => getComputedStyle(node).borderTopWidth), '0px');
      assert.equal(await page.$eval('.ap-sidebar-context strong', node => node.textContent), await page.$eval('.ap-current-context-row strong', node => node.textContent));
      for (const width of [1440, 390, 320]) {
        await page.setViewport({ width, height: 1000 }); await noOverflow();
        for (const selector of ['.ap-sidebar-switch', '.ap-current-context-row button', '.ap-invitations .ap-section-action']) {
          assert.ok(await page.$eval(selector, node => { const r = node.getBoundingClientRect(); return r.width >= 36 && r.height >= 36 && node.scrollWidth <= node.clientWidth + 1; }), `control fits at ${width}: ${selector}`);
        }
        if (process.env.UI_ARTIFACT_DIR && language === 'zh') await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `merged-identity-${width}.png`), fullPage: true });
      }
      await page.focus('.ap-sidebar-switch'); await page.keyboard.press('Enter');
      await page.waitForSelector('[role=dialog]'); await page.keyboard.press('Escape');
      await page.waitForFunction(() => !document.querySelector('[role=dialog]') && document.activeElement?.classList.contains('ap-sidebar-switch'));
      await page.click('.ap-current-context-row button'); await page.waitForSelector('[role=dialog]'); await page.keyboard.press('Escape');
      await page.click('.ap-invitations .ap-section-action'); await page.waitForSelector('[role=dialog] input[type=password]');
      await page.type('[role=dialog] input', 'controlled-draft'); await page.keyboard.press('Escape');
      await page.click('.ap-invitations .ap-section-action');
      assert.equal(await page.$eval('[role=dialog] input', node => node.value), 'controlled-draft');
      await page.keyboard.press('Escape');
    }
  });
  await run('approved merged identity switches through the real controller and preserves permission boundaries', async () => {
    await fresh();
    await page.evaluate(() => { window.restored.centerOrganization = true; window.restored.personal = ['personal.context.switch', 'personal.profile.update']; window.restored.reload(); });
    await page.waitForSelector('.ap-sidebar-switch:not(:disabled)');
    await page.click('.ap-sidebar-switch'); await page.waitForSelector('[role=dialog] option[value="33333333-3333-4333-8333-333333333333"]');
    await page.select('[role=dialog] select', '33333333-3333-4333-8333-333333333333');
    await page.click('[role=dialog] .ap-primary');
    await page.waitForFunction(() => !document.querySelector('[role=dialog]') && document.querySelector('.ap-sidebar-context strong')?.textContent === 'Sample organization');
    assert.equal(await page.$eval('.ap-current-context-row strong', node => node.textContent), 'Sample organization');
    assert.deepEqual(await page.evaluate(() => JSON.parse(window.restored.calls.find(call => call.url.endsWith('/active-context')).body)), { context_type: 'ORGANIZATION', organization_id: '33333333-3333-4333-8333-333333333333', expected_context_version: 1 });
    await page.click('.ap-current-context-row button'); await page.waitForSelector('[role=dialog] select'); await page.select('[role=dialog] select', '');
    await page.evaluate(() => { window.restored.contextSwitchStatus = 403; }); await page.click('[role=dialog] .ap-primary');
    await page.waitForSelector('[role=dialog] [role=alert]');
    assert.equal(await page.$eval('.ap-sidebar-context strong', node => node.textContent), 'Sample organization', 'denied write cannot change the displayed owner');
    await page.keyboard.press('Escape');
    await page.evaluate(() => { window.restored.contextSwitchStatus = 0; });
    await page.click('.ap-current-context-row button'); await page.waitForSelector('[role=dialog] select'); await page.select('[role=dialog] select', ''); await page.click('[role=dialog] .ap-primary');
    await page.waitForFunction(() => !document.querySelector('[role=dialog]') && document.querySelector('.ap-sidebar-context strong')?.textContent === window.restored.copy().center.personal);
    await page.evaluate(() => { window.restored.personal = []; window.restored.reload(); });
    await page.waitForFunction(() => !document.querySelector('.ap-sidebar-switch'));
    assert.equal(await page.$$eval('.ap-current-context-row button, .ap-invitations', nodes => nodes.length), 0);
    assert.ok(await page.$('.ap-account-overview .ap-account-information dl'), 'read-only masked profile remains available');
    assert.ok(!(await page.$eval('.ap-account-overview', node => node.textContent)).includes('+8613800000000'));
  });
  await run('approved merged identity contains long organization names and coarse pointer targets', async () => {
    await fresh();
    await page.setViewport({ width: 1440, height: 1000, hasTouch: true });
    await page.waitForSelector('.ap-product-picker');
    await page.evaluate(() => {
      window.restored.centerOrganization = true; window.restored.organizationName = 'Long organization name '.repeat(16); window.restored.username = 'LongAccountName'.repeat(12);
      window.restored.personal = ['personal.context.switch', 'personal.profile.update', 'personal.invitations.read', 'personal.invitations.accept'];
      window.restored.context = { context_type: 'ORGANIZATION', organization_id: '33333333-3333-4333-8333-333333333333', context_version: 1, membership_version: 1, organization_authorization_version: 1, permissions: [] }; window.restored.reload();
    });
    await accountTab('accountSettings'); await page.waitForFunction(() => document.querySelector('.ap-sidebar-context')?.title.includes('Long organization name'));
    for (const width of [1440, 390, 320]) {
      await page.setViewport({ width, height: 1000, hasTouch: true }); await noOverflow();
      assert.ok(await page.$eval('.ap-sidebar-switch', node => { const r = node.getBoundingClientRect(); return r.width >= 44 && r.height >= 44; }));
      assert.equal(await page.$eval('.ap-sidebar-context strong', node => getComputedStyle(node).textOverflow), 'ellipsis');
      assert.ok(await page.$eval('.ap-current-context-row strong', node => node.scrollWidth <= node.clientWidth + 1));
      if (process.env.UI_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `merged-long-identity-${width}.png`), fullPage: true });
    }
  });
  await run('refresh actions preserve section geometry during held HTTP reads', async () => {
    for (const viewport of [{ width: 1440, height: 850 }, { width: 390, height: 720 }]) {
      for (const target of [
        { section: 'creditsUsage', button: '.ap-portal-finance .ap-section-heading button', content: '.ap-recharge-form', endpoint: '/v1/recharge/products' },
        { section: 'ordersInvoices', button: '.ap-order-history > .ap-section-heading button', content: '.ap-order-history table', endpoint: '/v1/recharge/products' },
        { section: 'creditsUsage', button: '.ap-usage-workspace > .ap-section-heading button', content: '.ap-usage-filters', endpoint: '/v1/me/usage-' },
      ]) {
        await fresh(); await page.setViewport(viewport); await accountTab(target.section);
        await page.waitForFunction(selector => !document.querySelector(selector)?.disabled, {}, target.button);
        await page.$eval(target.button, node => node.scrollIntoView({ block: 'center' }));
        await page.evaluate(endpoint => {
          const fetch = globalThis.fetch;
          globalThis.fetch = async (...args) => {
            if (String(args[0]).includes(endpoint)) await new Promise(resolve => { window.restored.releaseSectionRefresh = resolve; });
            return fetch(...args);
          };
        }, target.endpoint);
        const geometry = () => page.evaluate(({ button, content }) => {
          const box = selector => { const r = document.querySelector(selector).getBoundingClientRect(); return [r.x, r.y, r.width, r.height]; };
          return { scroll: scrollY, button: box(button), content: box(content) };
        }, target);
        const before = await geometry(); await page.click(target.button);
        await page.waitForFunction(() => typeof window.restored.releaseSectionRefresh === 'function');
        assert.deepEqual(await geometry(), before, `loading must not move ${target.content}`);
        assert.equal(await page.$eval(target.button, b => b.getAttribute('aria-busy')), 'true');
        await page.evaluate(() => window.restored.releaseSectionRefresh());
        await page.waitForFunction(selector => !document.querySelector(selector).disabled, {}, target.button);
        assert.deepEqual(await geometry(), before, `completion must not move ${target.content}`);
      }
    }
  });
  await run('approved section actions are card-local and profile refresh belongs only to settings', async () => {
    for (const language of ['zh', 'en', 'ja']) {
      await fresh(); await page.select('.ap-sidebar-foot select', language);
      await page.waitForFunction(() => !document.querySelector('.ap-portal-finance .ap-section-heading button')?.disabled);
      assert.equal(await page.$$eval('.ap-page-header button', nodes => nodes.length), 0);
      assert.deepEqual(await page.$eval('.ap-portal-finance', node => { const style = getComputedStyle(node); return [style.paddingTop, style.paddingLeft, style.backgroundColor]; }), ['0px', '0px', 'rgba(0, 0, 0, 0)']);
      assert.ok(await page.$eval('.ap-recharge-form', node => Boolean(node.closest('.ap-recharge-card')?.querySelector('.ap-section-heading h3'))));
      for (const selector of ['.ap-recharge-card', '.ap-benefits', '.ap-usage-workspace']) {
        const style = await page.$eval(`${selector} > .ap-section-heading button`, node => { const s = getComputedStyle(node); return { border: s.borderTopWidth, height: node.getBoundingClientRect().height, radius: s.borderRadius, icon: Boolean(node.querySelector('svg')) }; });
        assert.deepEqual(style, { border: '1px', height: 40, radius: '8px', icon: true });
      }
      await accountTab('ordersInvoices'); assert.equal(await page.$$eval('.ap-page-header button', nodes => nodes.length), 0);
      await accountTab('accountSettings'); assert.equal(await page.$$eval('#ap-panel-accountSettings .ap-account-information .ap-section-action', nodes => nodes.length), 1);
      if (process.env.UI_ARTIFACT_DIR && language === 'zh') {
        await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, 'approved-settings-actions.png'), fullPage: true });
        await accountTab('creditsUsage'); await activeAccount('creditsUsage'); await page.waitForFunction(() => document.querySelector('[data-section=creditsUsage]').getAttribute('aria-selected') === 'true'); await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, 'approved-credit-actions.png'), fullPage: true });
      }
    }
  });
  await run('personal action hierarchy distinguish closure, query, cancel and recharge', async () => {
    for (const language of ['zh', 'en', 'ja']) {
      await fresh();
      await page.evaluate(language => {
        window.restored.language = language;
        window.restored.personal = ['personal.billing.read', 'personal.recharge.create', 'personal.usage.read', 'personal.profile.update', 'personal.account.close.read', 'personal.account.close.request'];
        window.restored.reload();
      }, language);
      await page.waitForSelector('.ap-product-picker label.selected');
      assert.equal(await page.$eval('.ap-balance-hero', node => getComputedStyle(node).marginTop), '0px');
      const recharge = await page.$eval('.ap-recharge-controls > button', node => ({ color: getComputedStyle(node).color, background: getComputedStyle(node).backgroundColor }));
      assert.deepEqual(recharge, { color: 'rgb(255, 255, 255)', background: 'rgb(19, 118, 71)' });
      await accountTab('accountSettings');
      await page.waitForSelector('.ap-danger-section button.ap-danger:not(:disabled)');
      const dangerStyle = async selector => page.$eval(selector, node => ({ color: getComputedStyle(node).color, border: getComputedStyle(node).borderTopColor, background: getComputedStyle(node).backgroundColor }));
      assert.deepEqual(await dangerStyle('.ap-danger-section button.ap-danger'), { color: 'rgb(217, 48, 37)', border: 'rgb(242, 197, 193)', background: 'rgb(255, 255, 255)' });
      assert.equal(await page.$eval('.ap-danger-section', node => getComputedStyle(node).borderTopColor), 'rgb(241, 216, 213)');
      const neutral = await page.$eval('.ap-danger-section > button:not(.ap-danger)', node => getComputedStyle(node).color);
      assert.notEqual(neutral, 'rgb(217, 48, 37)', 'read-only query must remain neutral');
      for (const width of [1440, 768, 390]) {
        await page.setViewport({ width, height: 1000 }); await noOverflow();
        assert.equal((await dangerStyle('.ap-danger-section button.ap-danger')).color, 'rgb(217, 48, 37)');
      }
      await page.click('.ap-danger-section button.ap-danger');
      await page.waitForSelector('.ap-closure-confirm-dialog');
      assert.equal(await page.$eval('.ap-closure-confirm-dialog', node => Boolean(node.closest('.ap-shell'))), false, 'confirmation really renders outside the shell');
      assert.deepEqual(await dangerStyle('.ap-closure-confirm-dialog button.ap-danger'), { color: 'rgb(217, 48, 37)', border: 'rgb(242, 197, 193)', background: 'rgb(255, 255, 255)' });
      assert.notEqual(await page.$eval('.ap-closure-confirm-dialog button[type=button]', node => getComputedStyle(node).color), 'rgb(217, 48, 37)', 'cancel must remain neutral');
      await page.focus('.ap-closure-confirm-dialog input');
      await page.keyboard.press('Tab'); await page.keyboard.press('Tab');
      assert.equal(await page.$eval('.ap-closure-confirm-dialog button.ap-danger', node => node === document.activeElement), true);
      assert.equal(await page.$eval('.ap-closure-confirm-dialog button.ap-danger', node => getComputedStyle(node).outlineStyle), 'solid');
      await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('.ap-closure-confirm-dialog'));
      assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0, 'visual review never sends a closure request');
      if (process.env.UI_ARTIFACT_DIR && language === 'zh') {
        fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, 'personal-settings-warning-mobile.png'), fullPage: true });
        await page.setViewport({ width: 1440, height: 1000 });
        await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, 'personal-settings-warning-desktop.png'), fullPage: true });
      }
    }
  });
  await run('late paid details preserve a newer review but still refresh a payable order', async () => {
    for (const mode of ['paid-to-review', 'unchanged-paid', 'newer-review', 'removed-review', 'other-order-paid', 'removed-first-paid']) {
      await fresh(); await accountTab('ordersInvoices');
      await page.evaluate(mode => {
        const originalFetch = globalThis.fetch, state = window.restored;
        const review = mode.endsWith('review');
        const initiallyPaid = mode === 'paid-to-review' || mode === 'unchanged-paid';
        state.newProjection = false; state.oldDetailReleased = false;
        const target = '00000000-0000-4000-8000-000000000001';
        globalThis.fetch = async (input, options = {}) => {
          const url = String(input);
          if (url.endsWith('/v1/recharge/products') && state.rejectFollowupProducts) return new Response(JSON.stringify({ code: 'UPSTREAM_UNAVAILABLE' }), { status: 503 });
          const response = await originalFetch(input, options);
          if ((options.method || 'GET') !== 'GET') return response;
          if (url.endsWith('/v1/recharge/orders')) {
            const body = await response.json();
            body.items = body.items.map((item, index) => {
              if (index === 0) return { ...item, status: state.newProjection && review ? 'MANUAL_REVIEW' : initiallyPaid ? 'PAID' : 'PENDING', fulfillment_status: state.newProjection && review ? 'MANUAL_REVIEW' : initiallyPaid ? 'CREDITED' : 'PENDING', paid_at: initiallyPaid || state.newProjection && review ? '2026-11-06T00:00:00Z' : null };
              if (index === 1 && mode !== 'newer-review') return { ...item, status: state.newProjection && !review && !initiallyPaid ? 'PAID' : 'PENDING', fulfillment_status: state.newProjection && !review && !initiallyPaid ? 'CREDITED' : 'PENDING', paid_at: state.newProjection && !review && !initiallyPaid ? '2026-11-06T00:00:00Z' : null };
              return item;
            });
            if (state.dropTarget) body.items = body.items.filter(item => item.order_id !== target);
            return new Response(JSON.stringify(body), { status: 200 });
          }
          if (url.endsWith(`/v1/recharge/orders/${target}`)) {
            const body = await response.json();
            // Valid producer sequence: a credited payment, then a distinct
            // provider transaction moves the projection to MANUAL_REVIEW.
            if (state.oldDetailReleased) return new Response(JSON.stringify({ ...body, status: 'MANUAL_REVIEW', fulfillment_status: 'MANUAL_REVIEW' }), { status: 200 });
            return new Promise(resolve => {
              state.releasePriorPaid = () => {
                state.oldDetailReleased = true; state.rejectFollowupProducts = review;
                resolve(new Response(JSON.stringify({ ...body, status: 'PAID', fulfillment_status: 'CREDITED', paid_at: '2026-11-06T00:00:00Z' }), { status: 200 }));
              };
            });
          }
          return response;
        };
        state.reload();
      }, mode);
      await page.waitForFunction(() => { const button = document.querySelector('.ap-order-history .ap-section-action'); return button && !button.disabled; });
      await page.click('.ap-order-history tbody tr:has(code[title="00000000-0000-4000-8000-000000000001"]) td:last-child button');
      await page.waitForFunction(() => typeof window.restored.releasePriorPaid === 'function');
      const readsBeforeProjection = await page.evaluate(() => window.restored.calls.filter(c => c.url.includes('/v1/me/balance?')).length);
      const collectionsBeforeProjection = await page.evaluate(() => window.restored.calls.filter(c => c.method === 'GET' && c.url.endsWith('/v1/recharge/orders')).length);
      await page.evaluate(() => { window.restored.newProjection = true; });
      const review = mode.endsWith('review');
      const projected = review ? 'MANUAL_REVIEW' : 'COMPLETED';
      const row = review ? 1 : 2;
      if (mode === 'unchanged-paid') {
        await page.waitForFunction(count => window.restored.calls.filter(c => c.method === 'GET' && c.url.endsWith('/v1/recharge/orders')).length >= count + 2, {}, collectionsBeforeProjection);
      } else {
        await page.waitForFunction(({ row, projected }) => document.querySelector(`.ap-order-history tbody tr:has(code[title="00000000-0000-4000-8000-${String(row).padStart(12, '0')}"]) .ap-status`)?.dataset.status === projected, {}, { row, projected });
      }
      if (!review && mode !== 'unchanged-paid') {
        await page.waitForFunction(count => window.restored.calls.filter(c => c.url.includes('/v1/me/balance?')).length > count && !document.querySelector('.ap-account-information .ap-section-action').disabled, {}, readsBeforeProjection);
      }
      if (mode.startsWith('removed-')) {
        await page.evaluate(() => { window.restored.dropTarget = true; });
        await page.waitForFunction(() => !document.querySelector('.ap-order-history tr:has(code[title="00000000-0000-4000-8000-000000000001"])'));
      }
      const balances = await page.evaluate(() => window.restored.calls.filter(c => c.url.includes('/v1/me/balance?')).length);
      await page.evaluate(() => window.restored.releasePriorPaid());
      await page.waitForFunction(() => document.querySelector('.ap-order-details-body')?.getAttribute('aria-busy') === 'false');
      if (review) {
        if (mode === 'removed-review') {
          assert.equal(await page.$$eval('.ap-order-history tr:has(code[title="00000000-0000-4000-8000-000000000001"])', nodes => nodes.length), 0, 'old paid cannot reinsert an evicted reviewed order');
        } else {
          assert.equal(await page.$eval('.ap-order-history tbody tr:has(code[title="00000000-0000-4000-8000-000000000001"]) .ap-status', n => n.dataset.status), 'MANUAL_REVIEW');
          assert.ok((await page.$eval('.ap-order-details-body dl', n => n.textContent)).includes(await page.evaluate(() => window.restored.copy().center.orderStatuses.MANUAL_REVIEW || window.restored.copy().accountLayout.statuses.MANUAL_REVIEW)));
        }
        assert.equal(await page.$$eval('.ap-order-details-body .ap-dialog-actions', nodes => nodes.length), 0, 'stale paid read must not unlock detail actions');
        await page.click('.ap-order-details-body > button');
        await page.waitForSelector('.ap-order-details-body textarea');
      } else {
        assert.equal(await page.$$eval('.ap-order-details-body .ap-dialog-actions', nodes => nodes.length), 1, 'unchanged facts must still admit the authorized paid detail');
        assert.ok((await page.$eval('.ap-order-details-body dl', n => n.textContent)).includes(await page.evaluate(() => window.restored.copy().center.orderStatuses.COMPLETED || window.restored.copy().accountLayout.statuses.COMPLETED)));
        await page.waitForFunction(count => window.restored.calls.filter(c => c.url.includes('/v1/me/balance?')).length > count, {}, balances);
      }
      assert.equal(await page.evaluate(() => window.restored.calls.filter(c => c.method === 'POST' && c.url.includes('/recharge/orders')).length), 0);
    }
  });
  await run('order details keep the underlying list and dialog geometry stable through delayed reads', async () => {
    for (const viewport of [{ width: 1440, height: 700 }, { width: 390, height: 560 }]) {
    await fresh(); await accountTab('ordersInvoices'); await activeAccount('ordersInvoices');
    await page.setViewport(viewport);
    await page.$eval('.ap-order-history tbody tr:nth-child(2) td:last-child button', node => node.scrollIntoView({ block: 'center' }));
    await page.evaluate(() => { window.restored.holdOrderDetail = true; });
    const geometry = () => page.evaluate(() => {
      const table = document.querySelector('.ap-order-history table').getBoundingClientRect();
      const dialog = document.querySelector('.ap-order-details-dialog')?.getBoundingClientRect();
      const facts = document.querySelector('.ap-order-details-dialog dl')?.getBoundingClientRect();
      return { scroll: scrollY, table: [table.x, table.y, table.width], dialog: dialog && [dialog.x, dialog.y, dialog.width, dialog.height], facts: facts && [facts.x, facts.y, facts.width] };
    });
    const before = await geometry();
    await page.click('.ap-order-history tbody tr:nth-child(2) td:last-child button');
    await page.waitForFunction(() => typeof window.restored.releaseOrderDetail === 'function');
    const loading = await geometry(); assert.deepEqual(loading.table, before.table); assert.equal(loading.scroll, before.scroll); assert.ok(loading.dialog); assert.ok(loading.facts, 'known order facts must be present while the read is pending');
    await page.evaluate(() => { window.restored.holdOrderDetail = false; window.restored.releaseOrderDetail(); });
    await page.waitForFunction(() => document.querySelector('.ap-order-details-dialog dd code'));
    await page.waitForFunction(() => !document.querySelector('.ap-order-details-dialog [aria-busy=true]'));
    const loaded = await geometry(); assert.deepEqual(loaded, loading);
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.deepEqual(await geometry(), before);
    }
  });
  await run('order detail read errors close in place and retired sessions withdraw the old selection', async () => {
    await fresh(); await accountTab('ordersInvoices'); await activeAccount('ordersInvoices');
    await page.evaluate(() => { window.restored.orderReadFail = true; });
    const before = await page.$eval('.ap-order-history', node => [node.getBoundingClientRect().x, node.getBoundingClientRect().y]);
    await page.click('.ap-order-history tbody tr:nth-child(2) td:last-child button');
    await page.waitForSelector('.ap-order-details-dialog [role=alert]');
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.deepEqual(await page.$eval('.ap-order-history', node => [node.getBoundingClientRect().x, node.getBoundingClientRect().y]), before);
    for (const status of [401, 403]) {
      await fresh(); await accountTab('ordersInvoices'); await activeAccount('ordersInvoices');
      await page.evaluate(status => { window.restored.orderReadFail = true; window.restored.orderReadStatus = status; }, status);
      await page.click('.ap-order-history tbody tr:nth-child(2) td:last-child button');
      await page.waitForFunction(() => !document.querySelector('.ap-order-details-dialog') && document.querySelector('[role=alert]'));
      assert.equal(await page.$eval('.ap-order-history > .ap-section-heading button', node => node.disabled), true);
      assert.equal(await page.$$eval('.ap-order-details-dialog', nodes => nodes.length), 0);
    }
    for (const change of ['epoch', 'client']) {
      await fresh(); await accountTab('ordersInvoices'); await activeAccount('ordersInvoices');
      await page.evaluate(() => { window.restored.holdOrderDetail = true; });
      await page.click('.ap-order-history tbody tr:nth-child(2) td:last-child button');
      await page.waitForFunction(() => typeof window.restored.releaseOrderDetail === 'function');
      await page.evaluate(change => {
        window.restored.holdOrderDetail = false;
        if (change === 'epoch') window.restored.client.financeSessionEpoch++;
        else window.restored.reload();
        window.restored.releaseOrderDetail();
      }, change);
      await page.waitForSelector('.ap-sidebar-foot select');
      await page.select('.ap-sidebar-foot select', 'en');
      await page.waitForFunction(() => !document.querySelector('.ap-order-details-dialog'));
      assert.equal(await page.$$eval('.ap-order-details-dialog', nodes => nodes.length), 0);
    }
  });
  await run('personal startup read failure reports a workspace error without an uncaught exception', async () => {
    await fresh();
    await page.evaluate(() => { window.restored.failInitialProducts = true; window.restored.reload(); });
    await page.waitForSelector('.ap-finance-feedback-target [role=alert]');
    assert.equal(await page.$eval('.ap-finance-feedback-target [role=alert]', node => node.textContent), await page.evaluate(() => window.restored.copy().finance.unavailable));
    assert.equal(await page.$$eval('.ap-order-details-dialog', nodes => nodes.length), 0);
  });
  await run('personal original tabs, five benefits, exact balance and history navigation', async () => {
    await fresh('?section=usage');
    await activeAccount('creditsUsage');
    assert.equal(await page.$$eval('.ap-sidebar [role=tab]', nodes => nodes.length), 3);
    assert.equal(await page.$$eval('#ap-panel-creditsUsage .ap-benefit-card', nodes => nodes.length), 5);
    assert.equal(await page.$eval('.ap-balance-hero > div:first-child strong', node => node.textContent), '922,337,203,685,477.5807');
    await accountTab('ordersInvoices'); await activeAccount('ordersInvoices');
    assert.equal(await page.$$eval('.ap-order-history tbody tr', nodes => nodes.length), 6);
    await page.goBack(); await activeAccount('creditsUsage'); await page.goForward(); await activeAccount('ordersInvoices');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0, 'URL navigation wrote business data');
  });
  await run('render review: compact products and white settings cards at desktop and mobile widths', async () => {
    await fresh();
    await page.evaluate(() => { window.restored.personal = ['personal.billing.read', 'personal.recharge.create', 'personal.recharge.manage', 'personal.usage.read', 'personal.profile.update', 'personal.security.manage']; window.restored.reload(); });
    await page.waitForSelector('.ap-product-picker');
    for (const width of [1440, 390]) {
      await page.setViewport({ width, height: 1100 }); await accountTab('creditsUsage'); await activeAccount('creditsUsage');
      const box = await page.$eval('.ap-product-picker label', node => ({ height: node.getBoundingClientRect().height, text: node.textContent }));
      assert.ok(box.height < 200, 'purchase cards should stay compact without hiding facts');
      const price = await page.$eval('.ap-product-price', node => ({ size: getComputedStyle(node).fontSize, weight: getComputedStyle(node).fontWeight }));
      assert.deepEqual(price, { size: '18px', weight: '500' });
      const borders = await page.$$eval('.ap-product-picker label', nodes => nodes.map(node => ({ border: getComputedStyle(node).borderWidth, shadow: getComputedStyle(node).boxShadow })));
      assert.ok(borders.every(item => item.border === '1px' && item.shadow === 'none'));

      assert.ok(box.text.includes('110') && box.text.includes('30'));
      assert.ok(await page.$$eval('.ap-benefit-card p', nodes => nodes.every(node => node.scrollWidth <= node.clientWidth + 1)), 'exact large balances must fit inside benefit cards');
      await noOverflow();
      if (process.env.UI_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, 'render-review-personal-' + width + '.png'), fullPage: true });
      await accountTab('ordersInvoices'); await activeAccount('ordersInvoices'); await noOverflow();
      assert.ok(await page.$$eval('.ap-order-table-wrap td:last-child button', nodes => nodes.every(node => node.getBoundingClientRect().width >= 60)));
      await accountTab('accountSettings'); await activeAccount('accountSettings');
      for (const selector of ['.ap-account-overview', '.ap-profile-rows', '.ap-security-actions']) {
        assert.equal(await page.$eval('#ap-panel-accountSettings > ' + selector, node => getComputedStyle(node).backgroundColor), 'rgb(255, 255, 255)');
      }
      await noOverflow();
      if (process.env.UI_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, 'render-review-settings-' + width + '.png'), fullPage: true });
    }
  });
  await run('personal unknown recharge survives tab changes with the original key and facts', async () => {
    await fresh(); await page.evaluate(() => { window.restored.unknown = true; });
    await page.click('.ap-recharge-controls > button');
    await page.waitForFunction(() => window.restored.calls.some(call => call.method === 'POST' && call.url.endsWith('/recharge/orders')) && document.querySelector('.ap-finance-feedback button'));
    await accountTab('ordersInvoices'); await accountTab('accountSettings'); await accountTab('creditsUsage');
    await click('.ap-finance-feedback button', await page.evaluate(() => window.restored.copy().finance.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/recharge/orders')).length === 2);
    const values = await page.evaluate(() => ({ calls: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/recharge/orders')), effects: window.restored.effects.size }));
    assert.equal(values.calls[0].key, values.calls[1].key); assert.equal(values.calls[0].body, values.calls[1].body); assert.equal(values.effects, 1);
  });
  await run('profile deep link and field dialogs retain keyboard and return focus', async () => {
    await fresh('?section=profile&profile=username');
    await page.waitForSelector('[role=dialog] input[autocomplete=username]');
    assert.equal(new URL(page.url()).searchParams.get('section'), 'accountSettings');
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.equal(new URL(page.url()).searchParams.has('profile'), false);
    await click('.ap-profile-rows .ap-profile-row:first-of-type button', await page.evaluate(() => window.restored.copy().accountLayout.edit));
    await page.waitForSelector('[role=dialog] input[autocomplete=username]');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    await page.waitForFunction(() => document.activeElement.closest('.ap-profile-row'));
    assert.ok(await page.evaluate(() => Boolean(document.activeElement.closest('.ap-profile-row'))), `focus did not return to edit trigger: ${await page.evaluate(() => document.activeElement.outerHTML.slice(0, 300))}`);
  });
  await run('account center facts, invoice removal, page-scoped usage filters and profile rows', async () => {
    await fresh();
    await page.evaluate(() => {
      window.restored.usage = [
        { usage_request_id: 'usage-paid', model: 'Sample A', started_at: '2026-11-01T12:00:00Z', execution_status: 'SUCCEEDED', settlement_status: 'SETTLED', charged_microcredits: '10001', display_status: 'SETTLED', input_tokens: 10, output_tokens: 5, cached_input_tokens: 0 },
        { usage_request_id: 'usage-failed', model: 'Sample B', started_at: '2026-11-02T12:00:00Z', execution_status: 'FAILED', settlement_status: 'RELEASED', charged_microcredits: '0', display_status: 'RELEASED' },
        { usage_request_id: 'usage-pending', model: 'Sample A', started_at: '2026-11-03T12:00:00Z', execution_status: 'SETTLEMENT_PENDING', settlement_status: 'USAGE_PENDING', charged_microcredits: null, display_status: 'SETTLEMENT_PENDING' },
      ]; window.restored.reload();
    });
    await page.waitForSelector('.ap-usage-table tbody tr');
    assert.equal(await page.$$eval('.ap-usage-table tbody tr', nodes => nodes.length), 3);
    const usageLayout = () => page.$$eval('.ap-usage-table tbody tr:first-child td', nodes => nodes.map(node => { const r = node.getBoundingClientRect(); return [r.x, r.width, r.height]; }));
    const beforeDetails = await usageLayout();
    await page.click('.ap-usage-table tbody tr:first-child td:last-child button');
    await page.waitForSelector('.ap-restored-dialog');
    assert.ok((await page.$eval('.ap-restored-dialog', node => node.textContent)).includes('usage-paid'));
    assert.deepEqual(await usageLayout(), beforeDetails, 'usage dialog must not resize any table cell');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('.ap-restored-dialog'));
    assert.ok(await page.evaluate(() => Boolean(document.activeElement.closest('.ap-usage-table'))), 'detail focus returns to its table trigger');

    assert.ok((await page.$eval('.ap-usage-summary', node => node.textContent)).includes('1.0001'));
    assert.ok((await page.$eval('.ap-usage-workspace', node => node.textContent)).includes(await page.evaluate(() => window.restored.copy().center.usageScope)));
    assert.equal(await page.$eval('.ap-product-picker dd', node => node.textContent), '110');
    await page.type('.ap-usage-filters label:nth-child(3) input', 'Sample B');
    assert.equal(await page.$$eval('.ap-usage-table tbody tr', nodes => nodes.length), 1);
    const result = await page.$eval('.ap-usage-table tbody tr', node => node.textContent);
    assert.equal(await page.$eval('.ap-usage-table .ap-status', node => node.textContent), await page.evaluate(() => window.restored.copy().center.callFailedNoCharge));
    await page.click('.ap-usage-table tbody tr td:last-child button');
    await page.waitForSelector('.ap-restored-dialog');
    assert.ok((await page.$eval('.ap-restored-dialog', node => node.textContent)).includes(await page.evaluate(() => window.restored.copy().usage.executionStatuses.FAILED || 'FAILED')));
    await page.keyboard.press('Escape');
    assert.ok(!result.includes('usage-paid'));
    await page.$eval('.ap-usage-filters label:nth-child(3) input', node => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(node, ''); node.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.$eval('.ap-usage-filters input[type=date]', node => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(node, '2026-11-03'); node.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.waitForFunction(() => document.querySelectorAll('.ap-usage-table tbody tr').length === 1);
    if (process.env.UI_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, 'personal-credits-desktop.png'), fullPage: true });
    await accountTab('ordersInvoices');
    assert.equal(await page.$eval('.ap-order-history tbody tr:nth-child(2) td:nth-child(4)', node => node.textContent), '—', 'unpaid order must not borrow its created timestamp');
    assert.equal(await page.$$eval('.ap-invoice-notice', nodes => nodes.length), 0);
    assert.equal(await page.$eval('.ap-sidebar [data-section=ordersInvoices]', node => node.textContent), await page.evaluate(() => window.restored.copy().account.sections.ordersInvoices));
    const id = await page.$eval('.ap-order-history tbody .ap-copy-identifier code', node => ({ title: node.title, text: node.textContent }));
    assert.ok(id.title.length > id.text.length);
    await page.evaluate(() => { window.copiedIdentifier = ''; Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async value => { window.copiedIdentifier = value; } } }); });
    await page.click('.ap-order-history tbody .ap-copy-identifier button');
    await page.waitForFunction(id => window.copiedIdentifier === id, {}, id.title);
    await page.evaluate(() => { Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('CONTROL_CLIPBOARD_DENIED'); } } }); });
    await page.click('.ap-order-history tbody .ap-copy-identifier button');
    assert.equal(await page.$eval('.ap-order-history tbody .ap-copy-identifier code', node => node.textContent), id.title, 'failed copying exposes the selectable full identifier');
    await accountTab('accountSettings');
    assert.equal(await page.$$eval('.ap-account-information dl', nodes => nodes.length), 0, 'editable profile is the single field owner');
    assert.equal(await page.$$eval('.ap-profile-rows > .ap-profile-row', nodes => nodes.length), 2);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('period totals cross pages, include older-only models and retire a delayed old session', async () => {
    await fresh();
    await page.evaluate(() => {
      const row = index => ({ usage_request_id: `00000000-0000-4000-8000-${String(index).padStart(12, '0')}`, model: index === 21 ? 'Older model' : 'Current model', started_at: '2026-11-04T12:00:00Z', charged_microcredits: '10000', execution_status: 'SUCCEEDED', settlement_status: 'SETTLED', display_status: 'SETTLED' });
      const cursor = 'turn-v1.' + btoa('2026-11-04T12:00:00Z|00000000-0000-4000-8000-000000000020').replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
      window.restored.usagePages = [{ items: Array.from({ length: 20 }, (_, index) => row(index + 1)), next_cursor: cursor }, { items: [row(21)], next_cursor: null }];
      window.restored.reload();
    });
    await page.waitForFunction(() => document.querySelectorAll('.ap-usage-table tbody tr').length === 20);
    for (const [index, value] of [[1, '2026-11-01'], [2, '2026-11-07']]) await page.$eval(`.ap-usage-filters label:nth-child(${index}) input`, (node, value) => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(node, value); node.dispatchEvent(new Event('input', { bubbles: true })); }, value);
    await page.click('.ap-period-summary .ap-primary');
    await page.waitForSelector('[data-period-total]');
    assert.equal(await page.$eval('[data-period-total]', node => node.textContent), await page.evaluate(() => window.restored.copy().center.periodTotal('21')));
    await page.click('.ap-period-summary .ap-disclosure');
    assert.equal(await page.$eval('.ap-period-body', node => node.hidden), true);
    assert.equal(await page.$eval('.ap-period-summary', node => node.dataset.periodState), 'complete');
    await page.click('.ap-period-summary .ap-disclosure');
    assert.equal(await page.$eval('[data-period-total]', node => node.textContent), await page.evaluate(() => window.restored.copy().center.periodTotal('21')));
    await page.type('.ap-usage-filters label:nth-child(3) input', 'Older model');
    assert.equal(await page.$('[data-period-total]'), null);
    await page.click('.ap-period-summary .ap-primary');
    await page.waitForSelector('[data-period-total]');
    assert.equal(await page.$eval('[data-period-total]', node => node.textContent), await page.evaluate(() => window.restored.copy().center.periodTotal('1')));
    await page.evaluate(() => { window.restored.holdUsage = true; });
    await page.click('.ap-period-summary .ap-primary');
    await page.waitForFunction(() => typeof window.restored.releaseUsage === 'function');
    await page.evaluate(() => { window.restored.client.financeSessionEpoch++; window.restored.releaseUsage(); });
    await page.waitForFunction(() => !document.querySelector('[data-period-total]') && !document.querySelector('.ap-period-summary [role=status]'));
    const usageReads = () => page.evaluate(() => window.restored.calls.filter(call => /\/usage-(requests|turns)(?:[/?]|$)/.test(call.url)).length);
    const before = await usageReads();
    await page.click('.ap-period-summary .ap-primary');
    assert.equal(await usageReads(), before, 'stale parent epoch cannot dispatch a new usage query');
  });
  await run('period bounded continuation and failed batches never present a partial or doubled total', async () => {
    await fresh();
    await page.evaluate(() => {
      const pages = [], index = { '': 0 };
      for (let page = 0; page < 21; page++) {
        const items = Array.from({ length: 20 }, (_, row) => ({ usage_request_id: `00000000-0000-4000-8000-${String(1000 - page * 20 - row).padStart(12, '0')}`, model: 'A', started_at: '2026-11-04T12:00:00Z', charged_microcredits: '10000', execution_status: 'SUCCEEDED', settlement_status: 'SETTLED', display_status: 'SETTLED' }));
        const cursor = 'turn-v1.' + btoa('2026-11-04T12:00:00Z|' + items.at(-1).usage_request_id).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
        pages.push({ items, next_cursor: page === 20 ? null : cursor }); index[cursor] = page + 1;
      }
      window.restored.usagePages = pages; window.restored.usageCursorPages = index; window.restored.reload();
    });
    await page.waitForSelector('.ap-usage-table tbody tr');
    for (const [index, value] of [[1, '2026-11-01'], [2, '2026-11-07']]) await page.$eval(`.ap-usage-filters label:nth-child(${index}) input`, (node, value) => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(node, value); node.dispatchEvent(new Event('input', { bubbles: true })); }, value);
    await page.evaluate(() => { window.restored.failUsageOnce = true; });
    await page.click('.ap-period-summary .ap-primary');
    await page.waitForSelector('.ap-period-summary[data-period-state=error]');
    assert.equal(await page.$('[data-period-total]'), null);
    await click('.ap-period-summary .ap-period-body > button', await page.evaluate(() => window.restored.copy().account.retry));
    await page.waitForSelector('.ap-period-summary[data-period-state=partial]');
    assert.equal(await page.$('[data-period-total]'), null);
    await page.evaluate(() => {
      const node = [...document.querySelectorAll('.ap-period-summary .ap-period-body > button')].find(node => node.textContent === window.restored.copy().center.periodContinue);
      window.oldPeriodContinue = node[Object.keys(node).find(key => key.startsWith('__reactProps$'))].onClick;
    });
    await page.type('.ap-usage-filters label:nth-child(3) input', 'B');
    const retiredUsageReads = () => page.evaluate(() => window.restored.calls.filter(call => /\/usage-(requests|turns)(?:[/?]|$)/.test(call.url)).length);
    const retiredReadCount = await retiredUsageReads();
    await page.evaluate(() => window.oldPeriodContinue());
    assert.equal(await retiredUsageReads(), retiredReadCount, 'retired Continue cannot revive an old checkpoint under new filters');
    assert.equal(await page.$('[data-period-total]'), null);
    await page.$eval('.ap-usage-filters label:nth-child(3) input', node => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(node, ''); node.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.evaluate(() => { window.restored.holdUsage = true; });
    await page.click('.ap-period-summary .ap-primary');
    await page.waitForFunction(() => typeof window.restored.releaseUsage === 'function');
    await page.evaluate(() => {
      window.oldUsageRelease = window.restored.releaseUsage;
      const node = [...document.querySelectorAll('.ap-period-summary .ap-period-body > button')].find(node => node.textContent === window.restored.copy().center.periodCancel);
      window.oldPeriodCancel = node[Object.keys(node).find(key => key.startsWith('__reactProps$'))].onClick;
    });
    await click('.ap-period-summary .ap-period-body > button', await page.evaluate(() => window.restored.copy().center.periodCancel));
    await page.click('.ap-period-summary .ap-primary');
    await page.waitForFunction(() => window.restored.releaseUsage !== window.oldUsageRelease);
    await page.evaluate(() => window.oldPeriodCancel());
    assert.equal(await page.evaluate(() => window.restored.heldUsageSignal.aborted), false, 'retired Cancel cannot abort a newer query');
    await page.evaluate(() => { window.restored.holdUsage = false; window.oldUsageRelease(); window.restored.releaseUsage(); });
    await page.waitForSelector('.ap-period-summary[data-period-state=partial]');
    await click('.ap-period-summary .ap-period-body > button', await page.evaluate(() => window.restored.copy().center.periodContinue));
    await page.waitForSelector('[data-period-total]');
    assert.equal(await page.$eval('[data-period-total]', node => node.textContent), await page.evaluate(() => window.restored.copy().center.periodTotal('420')));
  });
  const organizationCenter = async language => {
    await fresh();
    await page.evaluate(language => {
      window.restored.language = language; window.restored.centerOrganization = true;
      window.restored.personal = ['personal.context.switch', 'personal.profile.update', 'personal.invitations.read', 'personal.invitations.accept'];
      window.restored.context = { context_type: 'ORGANIZATION', organization_id: '33333333-3333-4333-8333-333333333333', context_version: 1, membership_version: 1, organization_authorization_version: 1, permissions: ['org.profile.read', 'org.units.read', 'org.units.manage', 'org.members.read', 'org.members.manage', 'org.roles.read', 'org.roles.assign', 'org.wallet.read', 'org.quota.read.self', 'org.quota.allocate', 'org.models.read', 'org.models.manage'] };
      window.restored.reload();
    }, language);
    await page.waitForFunction(() => document.querySelector('.ap-identity-card strong')?.textContent === 'Sample organization');
    await accountTab('myOrganization');
    await page.waitForSelector('.ap-org-tabs');
  };
  await run('organization order details keep rows stable while a paid read refreshes history', async () => {
    await organizationCenter('en');
    await page.evaluate(() => { window.restored.context.permissions.push('org.entitlements.purchase'); window.restored.reload(); });
    await page.waitForSelector('.ap-org-tabs'); await page.click('[data-org-section=billing]');
    await page.waitForSelector('.ap-organization-orders tbody tr');
    await page.setViewport({ width: 1440, height: 900 });
    await page.$eval('.ap-organization-orders tbody tr:first-child td:last-child button', node => node.scrollIntoView({ block: 'center' }));
    await page.evaluate(() => { window.restored.holdOrderDetail = true; window.restored.detailPaid = true; window.restored.historyPaid = true; });
    const geometry = () => page.evaluate(() => {
      const table = document.querySelector('.ap-organization-orders table').getBoundingClientRect();
      const dialog = document.querySelector('.ap-order-details-dialog')?.getBoundingClientRect();
      return { scroll: scrollY, table: [table.x, table.y, table.width], dialog: dialog && [dialog.x, dialog.y, dialog.width, dialog.height] };
    });
    const before = await geometry();
    await page.click('.ap-organization-orders tbody tr:first-child td:last-child button');
    await page.waitForFunction(() => typeof window.restored.releaseOrderDetail === 'function');
    const loading = await geometry(); assert.deepEqual(loading.table, before.table); assert.equal(loading.scroll, before.scroll); assert.ok(loading.dialog);
    await page.evaluate(() => { window.restored.holdOrderDetail = false; window.restored.releaseOrderDetail(); });
    await page.waitForFunction(() => !document.querySelector('.ap-order-details-body[aria-busy=true]') && !document.querySelector('.ap-organization-orders[aria-busy=true]'));
    assert.ok(await page.$eval('.ap-order-details-body dd code', node => node.textContent.length > 20));
    assert.equal(await page.$eval('.ap-organization-orders tbody tr:first-child .ap-status', node => node.dataset.status), 'COMPLETED');
    assert.deepEqual(await geometry(), loading);
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.deepEqual(await geometry(), before);
  });
  await run('organization detail retries preserve later pages and payment queries use the displayed order', async () => {
    await organizationCenter('en');
    await page.evaluate(() => { window.restored.context.permissions.push('org.entitlements.purchase'); window.restored.reload(); });
    await page.waitForSelector('.ap-org-tabs'); await page.click('[data-org-section=billing]');
    await page.waitForSelector('.ap-organization-orders tbody tr');
    await page.click('.ap-organization-orders > .ap-action-group button');
    await page.waitForFunction(() => document.querySelectorAll('.ap-organization-orders tbody tr').length === 3);
    const rows = () => page.$$eval('.ap-organization-orders tbody .ap-copy-identifier code', nodes => nodes.map(node => node.title));
    const ids = await rows();
    await page.$eval('.ap-organization-orders tbody tr:last-child td:last-child button', node => node.scrollIntoView({ block: 'center' }));
    const before = await page.$eval('.ap-organization-orders table', node => [node.getBoundingClientRect().x, node.getBoundingClientRect().y, node.getBoundingClientRect().height]);
    await page.evaluate(() => { window.restored.orgOrderReadStatus = 503; });
    await page.click('.ap-organization-orders tbody tr:last-child td:last-child button');
    await page.waitForSelector('.ap-order-details-dialog [role=alert]');
    await page.evaluate(() => { window.restored.orgOrderReadStatus = 0; window.restored.detailPaid = true; });
    await page.click('.ap-order-details-body > button');
    await page.waitForFunction(() => document.querySelector('.ap-organization-orders tbody tr:last-child .ap-status')?.dataset.status === 'COMPLETED');
    assert.ok((await page.$eval('.ap-order-details-body', node => node.textContent)).includes(ids.at(-1)));
    assert.deepEqual(await rows(), ids);
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.deepEqual(await page.$eval('.ap-organization-orders table', node => [node.getBoundingClientRect().x, node.getBoundingClientRect().y, node.getBoundingClientRect().height]), before);
    await page.evaluate(() => { window.restored.detailPaid = false; });
    await page.click('.ap-organization-orders tbody tr:first-child td:last-child button');
    await page.waitForFunction(() => document.querySelector('.ap-order-details-body .ap-primary') && !document.querySelector('.ap-order-details-body[aria-busy=true]'));
    await page.click('.ap-order-details-body .ap-primary'); await page.waitForSelector('.ap-payment-dialog');
    await page.waitForFunction(() => !document.querySelector('.ap-payment-dialog footer button:disabled'));
    await page.$eval('.ap-order-lookup input', node => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(node, '00000000-0000-4000-8000-000000000002'); node.dispatchEvent(new Event('input', { bubbles: true })); });
    const count = await page.evaluate(() => window.restored.calls.length);
    await page.click('.ap-payment-dialog footer button:last-child');
    await page.waitForFunction(count => window.restored.calls.slice(count).some(call => call.method === 'GET' && call.url.endsWith('/recharge/orders/00000000-0000-4000-8000-000000000001')), {}, count);
    assert.equal(await page.evaluate(count => window.restored.calls.slice(count).some(call => call.url.endsWith('/recharge/orders/00000000-0000-4000-8000-000000000002')), count), false);
  });
  await run('organization detail authorization denial withdraws its dialog and retires reads', async () => {
    for (const status of [401, 403]) {
      await organizationCenter('en');
      await page.evaluate(status => { window.restored.context.permissions.push('org.entitlements.purchase'); window.restored.orgOrderReadStatus = status; window.restored.reload(); }, status);
      await page.waitForSelector('.ap-org-tabs'); await page.click('[data-org-section=billing]');
      await page.waitForSelector('.ap-organization-orders tbody tr');
      await page.click('.ap-organization-orders tbody tr:first-child td:last-child button');
      await page.waitForFunction(() => !document.querySelector('.ap-order-details-dialog') && document.querySelector('[role=alert]'));
      assert.equal(await page.$$eval('.ap-order-details-dialog', nodes => nodes.length), 0);
      await accountTab('accountSettings');
      assert.equal(await page.$eval('.ap-account-information .ap-section-action', node => node.disabled), false);
    }
  });
  await run('render review: model forms, owner removal and responsive control geometry', async () => {
    await organizationCenter('en');
    await page.evaluate(() => { window.restored.includeOwner = true; window.restored.context.permissions.push('org.owner.transfer'); window.restored.reload(); });
    await page.waitForSelector('.ap-sidebar [data-section=myOrganization]');
    await accountTab('myOrganization'); await page.click('[data-org-section=members]');
    await page.waitForFunction(() => document.querySelectorAll('.ap-member-table tbody tr').length === 2);
    assert.equal(await page.$eval('.ap-member-table tbody tr:first-child button.ap-danger', node => node.disabled), true);
    assert.ok((await page.$eval('.ap-member-table tbody tr:first-child', node => node.textContent)).includes(await page.evaluate(() => window.restored.copy().center.noDepartment)));
    for (const width of [1908, 1440, 768, 390]) {
      await page.setViewport({ width, height: 1000 });
      await page.waitForFunction(vertical => document.querySelector('.ap-sidebar [role=tablist]').getAttribute('aria-orientation') === (vertical ? 'vertical' : 'horizontal'), {}, width > 850);
      await page.click('[data-org-section=models]'); await page.waitForSelector('.ap-policy-form');
      for (const mode of ['ALLOW_ALL', 'ALLOW_LIST', 'DENY_LIST']) {
        await page.select('.ap-policy-form > fieldset > label select', mode);
        const controls = await page.$$eval('.ap-policy-form > fieldset > label', nodes => nodes.map(node => { const r = node.getBoundingClientRect(); const control = node.querySelector('select,input'); return { left: r.left, right: r.right, top: r.top, bottom: r.bottom, controlHeight: control.getBoundingClientRect().height }; }));
        assert.equal(controls.length, 2); assert.equal(controls[0].controlHeight, controls[1].controlHeight);
        assert.ok(controls[0].right <= controls[1].left || controls[0].bottom <= controls[1].top, 'policy fields must never overlap');
        if (mode !== 'ALLOW_ALL') assert.equal(await page.$$eval('.ap-model-checklist input[type=checkbox]', nodes => nodes.length), 1);
        await noOverflow();
      }
      if (process.env.UI_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `render-review-models-${width}.png`), fullPage: true });
      await page.click('[data-org-section=roles]'); await page.waitForSelector('.ap-role-table tbody tr'); await noOverflow();
      assert.ok((await page.$eval('.ap-role-table tbody tr', node => node.textContent)).includes(await page.evaluate(() => window.restored.copy().center.permanent)));
      assert.ok((await page.$eval('.ap-role-table tbody tr', node => node.textContent)).includes(await page.evaluate(() => window.restored.copy().center.roleDescriptions.ORG_ADMIN)));
      const actions = await page.$$eval('#ap-org-panel-roles > .ap-action-group > button', nodes => nodes.map(node => { const r = node.getBoundingClientRect(); return { left: r.left, right: r.right, top: r.top }; }));
      assert.equal(actions.length, 2); assert.ok(actions[1].top > actions[0].top || actions[1].left - actions[0].right >= 8);
    }
    await click('#ap-org-panel-roles button', await page.evaluate(() => window.restored.copy().organization.grantRole));
    await page.waitForSelector('[role=dialog] select');
    await page.select('[role=dialog] label:nth-child(2) select', 'ORG_MODEL_ADMIN');
    assert.ok((await page.$eval('[role=dialog]', node => node.textContent)).includes(await page.evaluate(() => window.restored.copy().center.roleDescriptions.ORG_MODEL_ADMIN)));
    await page.keyboard.press('Escape');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => ['POST', 'PUT', 'DELETE'].includes(call.method) && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('render review: organization purchase facts, history pagination, details and stale reads', async () => {
    await organizationCenter('en');
    await page.evaluate(() => { window.restored.zeroBalance = true; window.restored.context.permissions.push('org.entitlements.purchase'); window.restored.reload(); });
    await page.waitForSelector('.ap-sidebar [data-section=creditsUsage]'); await accountTab('creditsUsage');
    await click('#ap-panel-creditsUsage button', await page.evaluate(() => window.restored.copy().center.buyOrganization));
    await page.waitForFunction(() => document.activeElement?.hasAttribute('data-organization-purchase'));
    const purchase = '[data-org-action=organization-purchase]';
    await page.waitForSelector(purchase + ' select');
    assert.equal(await page.$eval(purchase + ' button[type=submit]', node => node.disabled), true);
    await page.select(purchase + ' select:first-of-type', 'sample-points');
    await page.waitForFunction(selector => document.querySelector(selector).textContent.includes('110'), {}, purchase);
    assert.ok((await page.$eval(purchase, node => node.textContent)).includes('30'));
    assert.equal(await page.$eval(purchase + ' button[type=submit]', node => node.disabled), true);
    await page.select(purchase + ' label:nth-child(2) select', 'ALIPAY');
    assert.equal(await page.$eval(purchase + ' button[type=submit]', node => node.disabled), false);
    const history = '.ap-organization-orders';
    await page.waitForFunction(selector => document.querySelector(selector)?.querySelectorAll('tbody tr').length === 2, {}, history);
    await page.evaluate(() => { window.restored.historyFail = true; });
    await page.click(history + ' > .ap-action-group button'); await page.waitForSelector(history + ' [role=alert]');
    assert.equal(await page.$$eval(history + ' tbody tr', nodes => nodes.length), 2, 'failed continuation keeps loaded orders');
    await page.evaluate(() => { window.restored.historyFail = false; });
    await page.click(history + ' > .ap-action-group button');
    await page.waitForFunction(selector => document.querySelector(selector)?.querySelectorAll('tbody tr').length === 3, {}, history);
    assert.equal(await page.$(history + ' > .ap-action-group button'), null);
    for (const width of [1440, 768, 390]) {
      await page.setViewport({ width, height: 1100 }); await noOverflow();
      assert.ok(await page.$$eval(history + ' tbody td:last-child button', nodes => nodes.every(node => node.getBoundingClientRect().width >= 60)));
      if (process.env.UI_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, 'render-review-billing-' + width + '.png'), fullPage: true });
    }
    await page.click(history + ' tbody tr:first-child td:last-child button'); await page.waitForSelector('[role=dialog]');
    assert.ok((await page.$eval('[role=dialog]', node => node.textContent)).includes('9.90'));
    await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().finance.continuePayment));
    await page.waitForSelector('.ap-payment-dialog [data-qr-state=loaded]');
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    await page.evaluate(() => { window.restored.historyPaid = true; });
    await page.click(history + ' > .ap-section-heading button');
    await page.waitForFunction(text => document.querySelector('.ap-organization-orders tbody tr:first-child')?.textContent.includes(text), {}, await page.evaluate(() => window.restored.copy().accountLayout.statuses.COMPLETED));
    await page.evaluate(() => { window.restored.holdHistory = true; });
    await page.click(history + ' > .ap-section-heading button');
    await page.waitForFunction(() => typeof window.restored.releaseHistory === 'function');
    await page.evaluate(() => { window.restored.context = { context_type: 'PERSONAL', context_version: 2, permissions: [] }; window.restored.reload(); window.restored.releaseHistory(); });
    await page.waitForFunction(() => !document.querySelector('.ap-organization-orders'));
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.includes('/recharge/orders')).length), 0);
  });
  await run('render review: zero quota members receive administrator guidance without a purchase action', async () => {
    for (const language of ['zh', 'en', 'ja']) {
      await organizationCenter(language);
      await page.evaluate(() => { window.restored.organizationRoles = ['MEMBER']; window.restored.context.permissions = ['org.profile.read', 'org.quota.read.self']; window.restored.reload(); });
      await page.waitForSelector('.ap-sidebar [data-section=creditsUsage]'); await accountTab('creditsUsage');
      await page.waitForFunction(text => document.querySelector('#ap-panel-creditsUsage')?.textContent.includes(text), {}, await page.evaluate(() => window.restored.copy().center.contactOrganizationAdmin));
      assert.equal(await page.evaluate(() => [...document.querySelectorAll('#ap-panel-creditsUsage button')].some(button => button.textContent === window.restored.copy().center.buyOrganization)), false);
    }
  });
  await run('organization directory refreshes same-context names and role changes', async () => {
    await organizationCenter('en');
    await page.evaluate(() => {
      window.restored.organizationName = 'Renamed organization'; window.restored.organizationRoles = ['MEMBER'];
      window.restored.context = { ...window.restored.context, organization_authorization_version: 2 };
      dispatchEvent(new Event('focus'));
    });
    await page.waitForFunction(() => document.querySelector('.ap-identity-card strong')?.textContent === 'Renamed organization');
    assert.ok(!(await page.$eval('.ap-identity-card', node => node.textContent)).includes(await page.evaluate(() => window.restored.copy().organization.enumRole_OWNER)));
    await page.evaluate(() => { window.restored.organizationName = 'Renamed again'; });
    await accountTab('accountSettings'); await page.click('.ap-account-information .ap-section-action');
    await page.waitForFunction(() => document.querySelector('.ap-identity-card strong')?.textContent === 'Renamed again');
    assert.equal(await page.evaluate(() => window.restored.context.context_version), 1);
    await accountTab('myOrganization');
    await page.evaluate(() => { window.restored.organizationName = 'Renamed through organization refresh'; });
    await page.click('.ap-organization-workspace > .ap-section-heading button');
    await page.waitForFunction(() => document.querySelector('.ap-identity-card strong')?.textContent === 'Renamed through organization refresh');
    await accountTab('accountSettings');
    await click('.ap-invitations button', await page.evaluate(() => window.restored.copy().center.enterInvitation));
    await page.waitForSelector('[role=dialog] input[type=password]');
    await page.type('[role=dialog] input[type=password]', 'CONTROLLED_INVITATION_DRAFT');
    const reads = await page.evaluate(() => window.restored.calls.filter(call => call.url.endsWith('/v1/me/organizations')).length);
    await page.evaluate(() => dispatchEvent(new Event('focus')));
    await page.waitForFunction(reads => window.restored.calls.filter(call => call.url.endsWith('/v1/me/organizations')).length > reads && !document.querySelector('[role=dialog] input[type=password]')?.disabled, {}, reads);
    assert.equal(await page.$eval('[role=dialog] input[type=password]', node => node.value), 'CONTROLLED_INVITATION_DRAFT', 'ordinary focus refresh preserves the invitation intent');
    await page.keyboard.press('Escape');
    assert.equal(await page.evaluate(() => window.restored.calls.some(call => call.url.endsWith('/v1/invitations/accept'))), false);
  });
  await run('organization center tabs and member actions select the row and retain UNKNOWN after removal', async () => {
    await organizationCenter('en');
    await page.click('[data-org-section=members]');
    await page.waitForSelector('.ap-org-row-actions');
    await page.evaluate(() => { window.restored.unknown = true; });
    await click('.ap-org-row-actions button', await page.evaluate(() => window.restored.copy().organization.remove));
    await page.waitForSelector('[role=dialog] form[data-org-action=remove-member]');
    assert.equal(await page.$eval('[data-org-action=remove-member] select', node => node.value), '22222222-2222-4222-8222-222222222222');
    await page.$eval('[data-org-action=remove-member]', node => node.requestSubmit());
    await page.waitForSelector('.ap-org-confirm-dialog');
    assert.ok((await page.$eval('.ap-org-confirm-dialog', node => node.textContent)).includes('Sample member'));
    assert.equal(await page.evaluate(() => window.restored.calls.some(call => call.method === 'DELETE')), false);
    await page.click('.ap-org-confirm-dialog footer button:last-child');
    await page.waitForFunction(() => document.querySelector('[data-org-action=remove-member] fieldset')?.disabled);
    await page.evaluate(() => document.querySelector('.ap-organization-workspace .ap-section-heading button').click());
    await page.waitForFunction(() => document.querySelectorAll('.ap-org-row-actions').length === 0);
    await accountTab('creditsUsage'); await accountTab('myOrganization');
    await page.evaluate(() => { window.restored.unknown = false; });
    await click('[data-org-action=remove-member] > button', await page.evaluate(() => window.restored.copy().organization.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.method === 'DELETE').length === 2);
    const calls = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'DELETE'));
    assert.equal(calls[0].key, calls[1].key); assert.equal(calls[0].url, calls[1].url);
    assert.ok(calls[0].url.endsWith('/members/22222222-2222-4222-8222-222222222222'));
  });
  await run('organization center point allocation derives sources, confirms decimals and preserves same-key retry', async () => {
    await organizationCenter('en');
    await page.click('[data-org-section=billing]');
    const form = '[data-org-action=quota-allocation]'; await page.waitForSelector(form);
    const fields = await page.$$(`${form} select`);
    await fields[0].select('22222222-2222-4222-8222-222222222222');
    await fields[1].select('44444444-4444-4444-8444-444444444444');
    await page.type(`${form} input`, '1.2345');
    await page.evaluate(() => { window.restored.unknown = true; });
    await page.$eval(form, node => node.requestSubmit());
    await page.waitForSelector('.ap-org-confirm-dialog');
    assert.ok((await page.$eval('.ap-org-confirm-dialog', node => node.textContent)).includes('1.2345'));
    await page.click('.ap-org-confirm-dialog footer button:last-child');
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.endsWith('/quota-grants:batch')));
    await page.click('[data-org-section=members]'); await page.click('[data-org-section=billing]');
    await page.evaluate(() => { window.restored.unknown = false; window.restored.noSources = true; });
    await click(`${form} > button`, await page.evaluate(() => window.restored.copy().organization.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.url.endsWith('/quota-grants:batch')).length === 2);
    const calls = await page.evaluate(() => window.restored.calls.filter(call => call.url.endsWith('/quota-grants:batch')));
    assert.equal(calls[0].key, calls[1].key); assert.equal(calls[0].body, calls[1].body);
    const item = JSON.parse(calls[0].body).items[0];
    assert.equal(item.amount_microcredits, 12345); assert.equal(item.component_type, 'BASE'); assert.equal(item.membership_version, 1);
    await page.waitForFunction(() => document.querySelector('[data-org-action=quota-allocation]')?.textContent.includes(window.restored.copy().center.noSources));
    assert.equal(await page.$(`${form} > button`), null, 'resolved empty allocation has no executable submit control');
    assert.ok((await page.$eval(form, node => node.textContent)).includes(await page.evaluate(() => window.restored.copy().center.noSources)));
  });
  await run('allocation lifecycle replay closes UNKNOWN and permits the next versioned allocation', async () => {
    await organizationCenter('en'); await page.click('[data-org-section=billing]');
    const form = '[data-org-action=quota-allocation]'; await page.waitForSelector(form);
    await page.select(`${form} label:nth-child(1) select`, '22222222-2222-4222-8222-222222222222');
    await page.select(`${form} label:nth-child(2) select`, '44444444-4444-4444-8444-444444444444');
    await page.type(`${form} input`, '1');
    await page.evaluate(() => { window.restored.unknown = true; });
    await page.$eval(form, node => node.requestSubmit()); await page.waitForSelector('.ap-org-confirm-dialog'); await page.click('.ap-org-confirm-dialog footer button:last-child');
    await page.waitForFunction(() => [...document.querySelectorAll('[data-org-action=quota-allocation] > button')].some(node => node.textContent === window.restored.copy().organization.retry && !node.disabled));
    await page.evaluate(() => { window.restored.unknown = false; window.restored.membershipVersion = 2; window.restored.replayVersion = 2; });
    await click(`${form} > button`, await page.evaluate(() => window.restored.copy().organization.retry));
    await page.waitForFunction(() => !document.querySelector('[data-org-action=quota-allocation] fieldset').disabled && document.querySelector('[data-org-action=quota-allocation]')?.textContent.includes(window.restored.copy().organization.saved));
    await page.$eval(`${form} input`, node => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(node, '2'); node.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.$eval(form, node => node.requestSubmit()); await page.waitForSelector('.ap-org-confirm-dialog'); await page.click('.ap-org-confirm-dialog footer button:last-child');
    await page.waitForFunction(() => window.restored.calls.filter(call => call.url.endsWith('/quota-grants:batch')).length === 3);
    const calls = await page.evaluate(() => window.restored.calls.filter(call => call.url.endsWith('/quota-grants:batch')));
    assert.equal(calls[0].key, calls[1].key); assert.equal(calls[0].body, calls[1].body); assert.notEqual(calls[1].key, calls[2].key);
    assert.equal(JSON.parse(calls[2].body).items[0].membership_version, 2); assert.equal(JSON.parse(calls[2].body).items[0].amount_microcredits, 20000);
  });
  await run('organization center responsive hierarchy, keyboard tabs and three languages', async () => {
    for (const language of ['zh', 'en', 'ja']) {
      await organizationCenter(language);
      for (const width of [1440, 768, 390]) {
        await page.setViewport({ width, height: 1000 });
        for (const section of ['overview', 'members', 'roles', 'billing', 'models']) {
          await page.click(`[data-org-section=${section}]`); await noOverflow();
          if (process.env.UI_ARTIFACT_DIR && language === 'zh' && width !== 768) await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `organization-${section}-${width}.png`), fullPage: true });
        }
      }
      await page.focus('[data-org-section=overview]'); await page.keyboard.press('ArrowRight');
      assert.equal(await page.$eval('[data-org-section=members]', node => node.getAttribute('aria-selected')), 'true');
      assert.equal(await page.evaluate(() => window.restored.calls.some(call => call.method === 'DELETE' || call.url.endsWith('/quota-grants:batch'))), false);
    }
  });
  await run('admin original groups, canonical aliases, and back/forward', async () => {
    await fresh('?mode=admin&section=users');
    await page.waitForSelector('tbody tr');
    assert.equal(await page.$$eval('.adm-nav-group', nodes => nodes.length), 5);
    await adminTab('payments'); await page.waitForSelector('[data-workspace=orders]:not([hidden])');
    await page.goBack(); await page.waitForSelector('[data-workspace=users]:not([hidden])');
    await page.goForward(); await page.waitForSelector('[data-workspace=finance]:not([hidden])');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('admin unknown adjustment keeps the leaf owner across inner and outer navigation', async () => {
    await fresh('?mode=admin&section=billing&finance_tab=wallets');
    const form = '[data-workspace=wallets] form:not(.adm-filter-row)'; await page.waitForSelector(form);
    await page.type(`${form} input:first-of-type`, '11111111-1111-4111-8111-111111111111');
    const inputs = await page.$$(`${form} input`); await inputs[1].type('10000');
    await page.type(`${form} textarea`, 'Controlled adjustment reason');
    await page.$eval(form, node => node.requestSubmit());
    await page.waitForFunction(() => window.restored.calls.some(call => call.method === 'POST' && call.url.endsWith('/adjustments')));
    await adminTab('payments'); await adminTab('users'); await adminTab('billing');
    await click('[data-workspace=finance] .adm-tabs button', await page.evaluate(() => window.restored.copy().finance.wallet));
    await page.waitForFunction(() => [...document.querySelectorAll('[data-workspace=wallets] button')].some(node => node.textContent.trim() === window.restored.copy().finance.retry && !node.disabled));
    await click('[data-workspace=wallets] button', await page.evaluate(() => window.restored.copy().finance.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/adjustments')).length === 2);
    const values = await page.evaluate(() => ({ calls: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/adjustments')), effects: window.restored.effects.size }));
    assert.equal(values.calls[0].key, values.calls[1].key); assert.equal(values.calls[0].body, values.calls[1].body); assert.equal(values.effects, 1);
  });
  await run('model IA gate, permission denial, and missing data never fabricates counts', async () => {
    await fresh('?mode=admin&section=overview');
    await page.evaluate(() => { window.restored.flags.admin_model_token_ia = true; window.restored.permissions = ['platform.tokens.read']; window.restored.calls.length = 0; window.restored.reload(); });
    await page.waitForFunction(() => document.querySelectorAll('.adm-sidebar nav button').length === 3);
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.url.includes('/admin/account/') || call.url.includes('/admin/billing/'))));
    await page.evaluate(() => { history.pushState({}, '', '?mode=admin&section=users'); dispatchEvent(new PopStateEvent('popstate')); });
    await page.waitForFunction(() => document.querySelector('.adm-page [role=alert]')?.textContent === window.restored.copy().adminLayout.unauthorized);
    assert.ok(await page.evaluate(() => !document.querySelector('[data-workspace=users]') && !window.restored.calls.some(call => call.url.includes('/admin/v1/users'))));
  });
  await run('three overview domains fail independently and keep exact monetary digits', async () => {
    await fresh('?mode=admin&section=overview');
    await page.waitForFunction(() => document.querySelectorAll('[data-overview-domain] dl').length === 3);
    assert.ok(await page.$eval('[data-overview-domain=billing]', node => node.textContent.includes('900,719,925,474.0993')));
    await page.evaluate(() => { window.restored.failDomain = 'account'; window.restored.reload(); });
    await page.waitForSelector('[data-overview-domain=account] [role=alert]');
    assert.equal(await page.$$eval('[data-overview-domain=account] dl', nodes => nodes.length), 0);
    await page.waitForFunction(() => document.querySelectorAll('[data-overview-domain=billing] dl,[data-overview-domain=token] dl').length === 2);
  });
  await run('user detail deep links survive reload and current administrator cannot self-disable', async () => {
    await fresh('?mode=admin&section=users&user_q=sample');
    await page.waitForSelector('[data-workspace=users] tbody tr');
    assert.equal(await page.$eval('[data-workspace=users] tbody tr td:last-child button', node => node.disabled), true);
    await page.click('[data-workspace=users] .adm-name-link');
    await page.waitForSelector('.adm-user-details');
    assert.equal(new URL(page.url()).searchParams.get('user_id'), '11111111-1111-4111-8111-111111111111');
    await page.reload(); await page.waitForSelector('.adm-user-details');
    assert.ok(await page.$eval('.adm-user-details', node => !node.textContent.includes('@')));
    await click('[data-workspace=users] button', await page.evaluate(() => window.restored.copy().adminLayout.backToDirectory));
    await page.waitForSelector('[data-workspace=users] tbody tr');
    assert.equal(new URL(page.url()).searchParams.get('user_q'), 'sample');
  });
  await run('organization detail has four original URL sections, server member search and directory return', async () => {
    await fresh('?mode=admin&section=organizations&organization_q=sample');
    await page.waitForSelector('.adm-organization-directory .adm-name-link'); await page.click('.adm-organization-directory .adm-name-link');
    await page.waitForSelector('.adm-organization-header');
    assert.equal(await page.$$eval('.adm-organization-directory .adm-tabs button', nodes => nodes.length), 4);
    await page.reload(); await page.waitForSelector('.adm-organization-header');
    await click('.adm-organization-directory .adm-tabs button', await page.evaluate(() => window.restored.copy().adminLayout.organization.members));
    await page.waitForSelector('.adm-organization-directory [data-workspace=members] tbody tr');
    await page.type('.adm-organization-directory [data-workspace=members] form input', 'remote member');
    await page.$eval('.adm-organization-directory [data-workspace=members] form', form => form.requestSubmit());
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/members?') && call.url.includes('q=remote+member')));
    await page.waitForFunction(() => document.querySelector('.adm-organization-directory [data-workspace=members] tbody')?.textContent.includes('remote member'));
    await click('.adm-organization-directory>button', await page.evaluate(() => window.restored.copy().adminLayout.backToDirectory));
    await page.waitForSelector('.adm-organization-directory .adm-name-link');
    assert.equal(new URL(page.url()).searchParams.get('organization_q'), 'sample');
  });
  await run('organization deletion remains disabled for active or blocked resources and requires the name', async () => {
    const id = '33333333-3333-4333-8333-333333333333';
    await fresh(`?mode=admin&section=organizations&organization_id=${id}&organization_tab=lifecycle`);
    await page.waitForSelector('.adm-organization-header');
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.url.endsWith('/deletion-readiness'))));
    assert.ok(await page.evaluate(() => ![...document.querySelectorAll('.adm-organization-directory button')].some(node => node.textContent.trim() === window.restored.copy().organization.delete)));
    await page.evaluate(() => { window.restored.orgStatus = 'DISABLED'; window.restored.reload(); });
    await page.waitForFunction(() => document.querySelector('.adm-organization-directory')?.textContent.includes('ACTIVE_MEMBERS'));
    assert.ok(await page.evaluate(() => ![...document.querySelectorAll('.adm-organization-directory button')].some(node => node.textContent.trim() === window.restored.copy().organization.delete)));
    await page.evaluate(() => { window.restored.orgReady = true; window.restored.reload(); });
    await page.waitForFunction(() => [...document.querySelectorAll('.adm-organization-directory button')].some(node => node.textContent.trim() === window.restored.copy().organization.delete));
    await click('.adm-organization-directory button', await page.evaluate(() => window.restored.copy().organization.delete));
    await page.waitForSelector('[role=dialog] form.ap-org-action');
    const fields = await page.$$('[role=dialog] fieldset input');
    await fields[0].type('wrong name'); await fields[1].type('Controlled deletion reason');
    await page.click('[role=dialog] form>button[type=submit]');
    await page.waitForSelector('[role=dialog] form [role=alert]');
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.method === 'DELETE')));
    await fields[0].focus(); await page.keyboard.down('Control'); await page.keyboard.press('A'); await page.keyboard.up('Control'); await page.keyboard.press('Backspace'); await fields[0].type('Sample organization');
    assert.deepEqual(await page.$$eval('[role=dialog] fieldset input', nodes => nodes.map(node => node.value)), ['Sample organization', 'Controlled deletion reason']);
    await page.click('[role=dialog] form>button[type=submit]');
    await page.waitForSelector('[role=alertdialog]');
    await page.keyboard.press('Escape');
    await page.waitForSelector('[role=dialog] form.ap-org-action');
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.method === 'DELETE')));
    await page.keyboard.press('Escape');
  });
  await run('organization creation searches users and keeps the original unknown invocation in its modal', async () => {
    await fresh('?mode=admin&section=organizations');
    await page.evaluate(() => { window.restored.unknownOrgCreate = true; });
    await click('.adm-organization-directory button', await page.evaluate(() => window.restored.copy().organization.create));
    await page.waitForSelector('[role=dialog] .adm-user-picker');
    await page.type('[role=dialog] fieldset>label:first-child input', 'Controlled organization');
    await page.type('.adm-user-picker input', 'remote');
    await click('.adm-user-picker button', await page.evaluate(() => window.restored.copy().organization.search));
    await page.waitForFunction(() => document.querySelector('.adm-user-picker select option[value="22222222-2222-4222-8222-222222222222"]'));
    await page.select('.adm-user-picker select', '22222222-2222-4222-8222-222222222222');
    await page.click('[role=dialog] form>button[type=submit]');
    await page.waitForFunction(() => window.restored.calls.some(call => call.method === 'POST' && call.url.endsWith('/admin/v1/organizations')) && [...document.querySelectorAll('[role=dialog] form>button')].some(node => node.textContent.trim() === window.restored.copy().organization.retry && !node.disabled));
    await page.keyboard.press('Escape'); assert.ok(await page.$('[role=dialog]'));
    await page.evaluate(() => document.querySelector('.adm-sidebar [data-section=users]').click());
    assert.equal(new URL(page.url()).searchParams.get('section'), 'organizations');
    await click('[role=dialog] form>button', await page.evaluate(() => window.restored.copy().organization.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/admin/v1/organizations')).length === 2);
    const values = await page.evaluate(() => ({ calls: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/admin/v1/organizations')), effects: window.restored.effects.size }));
    assert.equal(values.calls[0].key, values.calls[1].key); assert.equal(values.calls[0].body, values.calls[1].body); assert.equal(values.effects, 1);
  });
  await run('financial read lists retain composite refund identity, filters and exact amounts without writes', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=refunds');
    await page.waitForSelector('[data-workspace=refunds] tbody tr');
    assert.equal(await page.$$eval('[data-workspace=refunds] tbody tr', nodes => nodes.length), 2);
    assert.ok((await page.$eval('[data-workspace=refunds]', node => node.textContent)).includes('90,071,992,547,409.93'));
    const details = await page.$$('[data-workspace=refunds] tbody button'); await details[1].click();
    await page.waitForFunction(() => new URL(location.href).searchParams.get('refund_id')?.startsWith('FORCED:'));
    await page.reload({ waitUntil: 'domcontentloaded' }); await page.waitForSelector('[data-workspace=refunds] dl');
    assert.ok((await page.$eval('[data-workspace=refunds] dl', node => node.textContent)).includes('强制退款'));
    await page.goBack(); await page.waitForSelector('[data-workspace=refunds] form');
    const query = await page.$('[data-workspace=refunds] form input'); await query.type('provider-trace');
    await page.$eval('[data-workspace=refunds] form', node => node.requestSubmit());
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/refunds?') && call.url.includes('q=provider-trace')));
    assert.equal(new URL(page.url()).searchParams.get('refund_q'), 'provider-trace');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('receivable read detail shows original settled outstanding facts and allocations', async () => {
    await fresh('?mode=admin&section=billing&finance_tab=receivables&receivable_id=11111111-1111-4111-8111-111111111111');
    await page.waitForSelector('[data-workspace=receivables] dl');
    const text = await page.$eval('[data-workspace=receivables]', node => node.textContent);
    assert.ok(text.includes('900,719,925,474.0993')); assert.ok(text.includes('900,719,925,473.0993'));
    assert.ok(text.includes('已结清积分')); assert.equal(await page.$$eval('[data-workspace=receivables] tbody tr', nodes => nodes.length), 1);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('profile rejection is visible inside the field modal', async () => {
    await fresh('?section=accountSettings&profile=username'); await page.waitForSelector('[role=dialog] input[autocomplete=username]');
    const input = await page.$('[role=dialog] input[autocomplete=username]'); await input.click(); await input.evaluate(node => node.select()); await input.type('controlled-new-name');
    await page.$eval('[role=dialog] form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('[role=dialog] [role=alert]')?.textContent === window.restored.copy().account.usernameAlreadyRegistered);
  });
  await run('closure authentication return selects visible account settings without a write', async () => {
    await fresh('?account_closure=confirm'); await activeAccount('accountSettings');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('personal order modal keeps history in its originating tab and exposes read failures', async () => {
    await fresh('?section=creditsUsage'); await accountTab('ordersInvoices');
    await page.waitForSelector('.ap-order-history tbody td:last-child button'); await page.click('.ap-order-history tbody td:last-child button');
    await page.waitForSelector('[role=dialog]'); await page.goBack();
    await activeAccount('ordersInvoices'); assert.ok(await page.$('[role=dialog]'));
    await page.evaluate(() => { window.restored.orderReadFail = true; });
    await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().finance.query));
    await page.waitForSelector('[role=dialog] [role=alert]');
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    await accountTab('creditsUsage'); assert.equal(await page.$('[role=dialog]'), null);
  });
  await run('personal-only deployments retain user details when organizations are unavailable', async () => {
    await fresh('?mode=admin&section=users'); await page.evaluate(() => { window.restored.failMemberships = true; });
    await page.click('.adm-table .adm-name-link'); await page.waitForSelector('.adm-user-details');
    await page.waitForSelector('[data-workspace=users] [role=alert]');
    assert.ok((await page.$eval('.adm-user-details', node => node.textContent)).includes('11111111-1111-4111-8111-111111111111'));
    await page.evaluate(() => { window.restored.failMemberships = false; });
    await page.click('[data-workspace=users] [role=alert] button');
    await page.waitForFunction(() => !document.querySelector('[data-workspace=users] [role=alert]') && document.querySelector('[data-workspace=users] ul')?.textContent.includes('Sample organization'));
  });
  await run('original payment filters and selected detail survive URL reload', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=orders&order_status=PAID&order_provider_id=controlled-provider&order_channel=ALIPAY&order_created_from=2026-10-01T00%3A00%3A12.345Z&order_limit=50&order_id=00000000-0000-4000-8000-000000000002');
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/payment-orders?') && call.url.includes('status=PAID') && call.url.includes('provider_id=controlled-provider') && call.url.includes('channel=ALIPAY') && call.url.includes('limit=50') && call.url.includes('created_from=')));
    await page.waitForSelector('[data-workspace=orders] article');
    assert.ok((await page.$eval('[data-workspace=orders] article', node => node.textContent)).includes('00000000-0000-4000-8000-000000000002'));
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-workspace=orders] article');
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/payment-orders?') && call.url.includes('status=PAID')));
    assert.equal(new URL(page.url()).searchParams.get('order_channel'), 'ALIPAY');
  });
  await run('history during a pending filtered read coalesces the latest URL without stale rows', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=orders&order_status=PENDING');
    await page.evaluate(() => { window.restored.filteredOrders = true; window.restored.reload(); });
    await page.waitForFunction(() => document.querySelector('[data-workspace=orders] tbody')?.textContent.includes('PENDING'));
    await page.waitForFunction(() => !document.querySelector('[data-workspace=orders] .adm-filter-row button').disabled);
    await page.evaluate(() => { window.restored.delayAdminOrders = true; });
    await page.select('[data-workspace=orders] .adm-filter-row select', 'PAID'); await page.$eval('[data-workspace=orders] .adm-filter-row', form => form.requestSubmit());
    await page.waitForFunction(() => typeof window.restored.releaseAdminOrders === 'function');
    const before = await page.evaluate(() => window.restored.calls.filter(call => call.url.includes('/payment-orders?') && new URL(call.url, location.origin).searchParams.get('status') === 'PENDING').length);
    await page.goBack(); await page.waitForFunction(() => new URLSearchParams(location.search).get('order_status') === 'PENDING');
    await page.evaluate(() => window.restored.releaseAdminOrders());
    await page.waitForFunction(before => window.restored.calls.filter(call => call.url.includes('/payment-orders?') && new URL(call.url, location.origin).searchParams.get('status') === 'PENDING').length === before + 1 && document.querySelector('[data-workspace=orders] tbody')?.textContent.includes('PENDING'), {}, before);
    assert.ok(!(await page.$eval('[data-workspace=orders] tbody', node => node.textContent)).includes('PAID'));
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('review detail failure and mismatched receipt never expose the previously selected case', async () => {
    const first = '22222222-2222-4222-8222-222222222222', second = '33333333-3333-4333-8333-333333333333';
    for (const [tab, prefix, endpoint] of [['late', 'late', 'late-payment-reviews'], ['anomalies', 'anomaly', 'payment-anomalies']]) {
      await fresh(`?mode=admin&section=payments&finance_tab=${tab}`);
      await page.evaluate(({ tab, prefix, first }) => { window.restored.reviewFixture = true; window.restored.permissions = ['platform.billing.wallets.read', 'platform.billing.orders.sync', 'platform.billing.repairs.request']; history.replaceState({}, '', `?mode=admin&section=payments&finance_tab=${tab}&${prefix}_id=${first}`); dispatchEvent(new PopStateEvent('popstate')); window.restored.reload(); }, { tab, prefix, first });
      await page.waitForFunction(({ tab, first }) => document.querySelector(`[data-workspace=${tab}] article h4`)?.textContent === first, {}, { tab, first });
      await page.evaluate(({ tab, prefix, second }) => { window.restored.failReviewID = second; history.pushState({}, '', `?mode=admin&section=payments&finance_tab=${tab}&${prefix}_id=${second}`); dispatchEvent(new PopStateEvent('popstate')); }, { tab, prefix, second });
      await page.waitForFunction(({ tab, endpoint, second }) => window.restored.calls.some(call => call.url.endsWith('/' + endpoint + '/' + second)) && document.querySelector(`[data-workspace=${tab}] [role=alert]`) && !document.querySelector(`[data-workspace=${tab}] .adm-filter-row button`).disabled, {}, { tab, endpoint, second });
      assert.equal(await page.$(`[data-workspace=${tab}] article`), null);
      await page.evaluate(first => { window.restored.failReviewID = ''; window.restored.reviewAckID = first; }, first);
      await click(`[data-workspace=${tab}] nav button`, await page.evaluate(() => window.restored.copy().finance.retry));
      await page.waitForFunction(({ tab, endpoint, second }) => window.restored.calls.filter(call => call.url.endsWith('/' + endpoint + '/' + second)).length === 2 && !document.querySelector(`[data-workspace=${tab}] .adm-filter-row button`).disabled, {}, { tab, endpoint, second });
      assert.equal(await page.$(`[data-workspace=${tab}] article`), null);
      await page.evaluate(() => { window.restored.reviewAckID = ''; });
      await click(`[data-workspace=${tab}] nav button`, await page.evaluate(() => window.restored.copy().finance.retry));
      await page.waitForFunction(({ tab, second }) => document.querySelector(`[data-workspace=${tab}] article h4`)?.textContent === second, {}, { tab, second });
      await click(`[data-workspace=${tab}] nav button`, await page.evaluate(() => window.restored.copy().adminLayout.backToDirectory));
      await page.waitForFunction(prefix => !new URLSearchParams(location.search).has(prefix + '_id'), {}, prefix);
      assert.equal(await page.$(`[data-workspace=${tab}] article`), null);
      assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
    }
  });
  await run('unknown command shows its original target and key after history clears the prior detail', async () => {
    const first = '22222222-2222-4222-8222-222222222222', second = '33333333-3333-4333-8333-333333333333';
    await fresh('?mode=admin&section=payments&finance_tab=late');
    await page.evaluate(first => { window.restored.reviewFixture = true; window.restored.permissions = ['platform.billing.wallets.read', 'platform.billing.orders.sync']; history.replaceState({}, '', '?mode=admin&section=payments&finance_tab=late&late_id=' + first); dispatchEvent(new PopStateEvent('popstate')); window.restored.reload(); }, first);
    await page.waitForFunction(first => document.querySelector('[data-workspace=late] article h4')?.textContent === first, {}, first);
    await click('[data-workspace=late] nav button', await page.evaluate(() => window.restored.copy().adminLayout.backToDirectory));
    await page.waitForSelector('[data-workspace=late] tbody tr:nth-child(2) button'); await page.click('[data-workspace=late] tbody tr:nth-child(2) button');
    await page.waitForFunction(second => document.querySelector('[data-workspace=late] article h4')?.textContent === second, {}, second);
    await page.type('[data-workspace=late] article textarea', 'Controlled original schedule reason');
    await page.$eval('[data-workspace=late] article input[type=datetime-local]', input => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, '2026-11-01T09:30'); input.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.evaluate(() => { window.restored.delayReviewSchedule = true; }); page.once('dialog', dialog => dialog.accept());
    await click('[data-workspace=late] article button', await page.evaluate(() => window.restored.copy().finance.schedule));
    await page.waitForFunction(() => typeof window.restored.releaseReviewSchedule === 'function'); await page.goBack();
    await page.waitForFunction(() => !new URLSearchParams(location.search).has('late_id')); await page.goBack();
    await page.waitForFunction(first => new URLSearchParams(location.search).get('late_id') === first, {}, first);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.evaluate(() => window.restored.releaseReviewSchedule());
    await page.waitForFunction(second => document.querySelector('[data-workspace=late] [data-retained-finance-command]')?.textContent.includes(second), {}, second);
    const firstWrite = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/schedule')));
    assert.equal(firstWrite.length, 1); assert.ok((await page.$eval('[data-retained-finance-command]', node => node.textContent)).includes(firstWrite[0].key));
    await page.setViewport({ width: 390, height: 1000 }); await noOverflow(); await page.setViewport({ width: 1440, height: 1000 });
    await click('[data-retained-finance-command] button', await page.evaluate(() => window.restored.copy().finance.retry));
    await page.waitForFunction(first => document.querySelector('[data-workspace=late] article h4')?.textContent === first && !document.querySelector('[data-retained-finance-command]'), {}, first);
    const writes = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/schedule')));
    assert.equal(writes.length, 2); assert.equal(writes[0].url, writes[1].url); assert.equal(writes[0].key, writes[1].key); assert.equal(writes[0].body, writes[1].body); assert.equal(await page.evaluate(() => window.restored.effects.size), 1);
  });
  await run('wallet ledger restores filters, columns and exact signed amounts', async () => {
    await fresh('?mode=admin&section=billing&finance_tab=wallets&wallet_id=11111111-1111-4111-8111-111111111111&ledger_entry_type=ADJUSTMENT&ledger_business_reference=controlled-reference');
    await page.waitForSelector('.adm-wallet-ledger tbody tr');
    assert.ok((await page.$eval('.adm-wallet-ledger', node => node.textContent)).includes('922,337,203,685,477.5807'));
    assert.ok(await page.evaluate(() => window.restored.calls.some(call => call.url.includes('/ledger?') && call.url.includes('entry_type=ADJUSTMENT') && call.url.includes('business_reference=controlled-reference'))));
  });
  await run('all four financial overviews show real time scope and effective drilldown', async () => {
    await fresh('?mode=admin&section=billing'); await page.waitForSelector('[data-finance-overview=accounts] .adm-finance-overview-card');
    assert.ok((await page.$eval('[data-finance-overview=accounts]', node => node.textContent)).includes('922,337,203,685,477.5807'));
    await page.setViewport({ width: 390, height: 1000 }); await noOverflow(); await page.setViewport({ width: 1440, height: 1000 });
    await adminTab('payments'); await page.waitForSelector('[data-finance-overview=payments] .adm-finance-overview-card');
    assert.ok((await page.$eval('[data-finance-overview=payments]', node => node.textContent)).includes('UTC'));
    await page.click('[data-finance-overview=payments] .adm-finance-overview-card');
    await page.waitForFunction(() => new Date(new URL(location.href).searchParams.get('order_created_from')).toISOString() === '2026-10-28T00:00:00.000Z');
    await adminTab('products'); await page.waitForSelector('[data-finance-overview=products] .adm-finance-overview-card');
    await page.evaluate(() => { history.pushState({}, '', '?mode=admin&section=products&finance_tab=catalog&product_q=stale-search&product_include_deleted=true'); dispatchEvent(new PopStateEvent('popstate')); });
    await page.click('[data-finance-overview=products] .adm-finance-overview-card');
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/recharge-products?') && call.url.includes('lifecycle_status=ON_SALE')));
    assert.equal(new URL(page.url()).searchParams.has('product_q'), false); assert.equal(new URL(page.url()).searchParams.has('product_include_deleted'), false);
    const priceLabel = await page.evaluate(() => window.restored.copy().adminLayout.nav.products);
    assert.ok(priceLabel); await page.evaluate(() => { history.pushState({}, '', '?mode=admin&section=products&finance_tab=prices'); dispatchEvent(new PopStateEvent('popstate')); });
    await page.waitForSelector('[data-finance-overview=pricing] .adm-finance-overview-card');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('late accepted recharge stays with its origin tab and retains its one order', async () => {
    await fresh('?section=creditsUsage'); await page.evaluate(() => { window.restored.delayCreate = true; });
    await page.click('.ap-recharge-controls > button');
    await page.waitForFunction(() => typeof window.restored.releaseCreate === 'function'); await accountTab('accountSettings');
    await page.evaluate(() => window.restored.releaseCreate()); await page.waitForFunction(() => !document.querySelector('.ap-recharge-form button').disabled);
    assert.equal(await page.$('.ap-payment-dialog'), null); await activeAccount('accountSettings');
    await accountTab('creditsUsage'); await page.waitForSelector('.ap-payment-dialog');
    assert.equal(await page.$eval('.ap-payment-dialog [role=status]', node => node.textContent), await page.evaluate(() => window.restored.copy().finance.paymentOrderCreated));
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/recharge/orders')).length), 1);
  });
  await run('submitting displayed date filters sends timezone-bearing RFC3339', async () => {
    for (const [section, tab, prefix, endpoint] of [['payments', 'orders', 'order', 'payment-orders'], ['payments', 'late', 'late', 'late-payment-reviews'], ['payments', 'anomalies', 'anomaly', 'payment-anomalies']]) {
      await fresh(`?mode=admin&section=${section}&finance_tab=${tab}&${prefix}_created_from=2026-10-01T00%3A00%3A12.345Z`);
      await page.waitForSelector(`[data-workspace=${tab}] .adm-filter-row input[type=datetime-local]`);
      await page.waitForFunction(selector => !document.querySelector(selector).disabled, {}, `[data-workspace=${tab}] .adm-filter-row button`);
      const before = await page.evaluate(endpoint => window.restored.calls.filter(call => call.url.includes('/' + endpoint + '?')).length, endpoint);
      await page.$eval(`[data-workspace=${tab}] .adm-filter-row`, form => form.requestSubmit());
      await page.waitForFunction(({ endpoint, before }) => window.restored.calls.filter(call => call.url.includes('/' + endpoint + '?')).length > before, {}, { endpoint, before });
      const value = await page.evaluate(endpoint => new URL(window.restored.calls.filter(call => call.url.includes('/' + endpoint + '?')).at(-1).url, location.origin).searchParams.get('created_from'), endpoint);
      assert.match(value, /Z$/); assert.equal(value, '2026-10-01T00:00:12.345Z');
      assert.equal(await page.$(`[data-workspace=${tab}] [role=alert]`), null);
    }
  });
  await run('product unknown modal exposes only explicit recovery of the original command', async () => {
    await fresh('?mode=admin&section=products&finance_tab=catalog');
    await page.evaluate(() => { window.restored.useCatalogFixture = true; window.restored.permissions = ['platform.products.read', 'platform.products.publish']; window.restored.reload(); });
    await page.waitForSelector('[data-workspace=catalog] tbody button'); await page.click('[data-workspace=catalog] tbody button'); await page.waitForSelector('[role=dialog] textarea');
    await page.type('[role=dialog] textarea', 'Controlled display-name correction'); page.once('dialog', dialog => dialog.accept());
    await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().finance.saveNames));
    await page.waitForFunction(() => [...document.querySelectorAll('[role=dialog] button')].some(node => node.textContent.trim() === window.restored.copy().finance.retry && !node.disabled));
    await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().finance.retry));
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    const calls = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/display-names')));
    assert.equal(calls.length, 2); assert.equal(calls[0].key, calls[1].key); assert.equal(calls[0].body, calls[1].body);
    assert.equal(await page.evaluate(() => window.restored.effects.size), 1);
  });
  await run('product creation uses the restored modal and current typed command', async () => {
    await fresh('?mode=admin&section=products&finance_tab=catalog');
    await page.evaluate(() => { window.restored.useCatalogFixture = true; window.restored.permissions = ['platform.products.read', 'platform.products.create']; window.restored.reload(); });
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.endsWith('/recharge-product-create-options')));
    await click('[data-workspace=catalog] button', await page.evaluate(() => window.restored.copy().finance.createProduct));
    await page.waitForSelector('[role=dialog] form');
    for (const [key, value] of [['productID', 'controlled-new'], ['nameZh', '测试商品'], ['nameEn', 'Controlled new'], ['nameJa', 'テスト商品'], ['base', '100'], ['validityDays', '30'], ['sku', 'CONTROLLED_NEW'], ['amountFen', '100']]) {
      const input = await page.evaluateHandle(key => [...document.querySelectorAll('[role=dialog] label')].find(node => node.firstChild?.textContent === window.restored.copy().finance[key])?.querySelector('input'), key);
      assert.ok(input.asElement(), key); await input.asElement().type(value); await input.dispose();
    }
    await page.select('[role=dialog] select', 'Service');
    const provider = await page.evaluateHandle(() => [...document.querySelectorAll('[role=dialog] label')].find(node => node.firstChild?.textContent === window.restored.copy().finance.provider)?.querySelector('select'));
    await provider.asElement().select('controlled/1'); await provider.dispose();
    await page.click('[role=dialog] input[type=checkbox]'); await page.type('[role=dialog] textarea', 'Controlled create');
    page.once('dialog', dialog => dialog.accept()); await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().finance.createProduct));
    await page.waitForFunction(() => !document.querySelector('[role=dialog]') && window.restored.catalogRecord?.lifecycle_status === 'DRAFT');
    const calls = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/recharge-products')));
    assert.equal(calls.length, 1); const body = JSON.parse(calls[0].body); assert.equal(body.product_id, 'controlled-new'); assert.equal(body.mapping.provider_id, 'controlled'); assert.deepEqual(body.mapping.channels, ['ALIPAY']);
    assert.equal(await page.evaluate(() => window.restored.effects.size), 1);
  });
  await run('admin my organization follows the same actor context generation', async () => {
    await fresh('?mode=admin&section=users');
    await page.evaluate(() => { window.restored.context = { context_type: 'ORGANIZATION', organization_id: '33333333-3333-4333-8333-333333333333', context_version: 1, membership_version: 1, organization_authorization_version: 1, permissions: ['org.profile.read'] }; history.replaceState({}, '', '?mode=admin&section=myorg'); dispatchEvent(new PopStateEvent('popstate')); window.restored.reload(); });
    await page.waitForFunction(() => document.querySelector('[data-workspace=myorg]')?.textContent.includes('33333333-3333-4333-8333-333333333333'));
    const before = await page.evaluate(() => window.restored.calls.filter(call => call.url.endsWith('/account/bootstrap')).length);
    await page.evaluate(() => { window.restored.context = { ...window.restored.context, organization_id: '66666666-6666-4666-8666-666666666666', context_version: 2 }; dispatchEvent(new Event('focus')); });
    await page.waitForFunction(before => window.restored.calls.filter(call => call.url.endsWith('/account/bootstrap')).length >= before + 2 && document.querySelector('[data-workspace=myorg]')?.textContent.includes('66666666-6666-4666-8666-666666666666'), {}, before);
    assert.ok(!(await page.$eval('[data-workspace=myorg]', node => node.textContent)).includes('33333333-3333-4333-8333-333333333333'));
  });
  const roleWords = () => page.evaluate(() => window.restored.copy().rolePresentation);
  const readyRoles = async () => { await fresh('?mode=admin&section=roles'); await page.waitForSelector('.adm-role-table tbody tr'); };
  const openRoleReview = async (target = '22222222-2222-4222-8222-222222222222', role = 'PLATFORM_BILLING_OPERATOR', expires = '') => {
    const words = await roleWords();
    await click('.adm-role-workspace-head button', words.newGrant);
    await page.waitForSelector('[role=dialog] .adm-role-grant');
    await page.type('[role=dialog] .adm-role-field input', target);
    await click('[role=dialog] button', words.next);
    await page.select('[role=dialog] select', role);
    if (expires) await page.$$eval('[role=dialog] input[type=datetime-local]', (nodes, value) => {
      const node = nodes.at(-1);
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(node, value);
      node.dispatchEvent(new Event('input', { bubbles: true }));
    }, expires);
    await page.type('[role=dialog] textarea', 'Controlled role review');
    await click('[role=dialog] button', words.next);
    await page.waitForSelector('[role=dialog] .adm-role-preview');
    return words;
  };
  await run('roles: original catalog and permission groups fit desktop and narrow viewports', async () => {
    await readyRoles();
    for (const language of ['zh', 'en', 'ja']) {
      await page.select('.adm-sidebar-foot select', language);
      const words = await roleWords();
      assert.equal(await page.$eval('.adm-role-detail h3', node => node.textContent), words.names.PLATFORM_AUDITOR);
      assert.equal(await page.$$eval('.adm-role-permission-groups details', nodes => nodes.length), 4);
      await click('.adm-role-table button', words.names.PLATFORM_SUPER_ADMIN);
      assert.equal(await page.$('.adm-role-detail>.adm-role-primary'), null, 'protected superadmin gained a grant control');
      await click('.adm-role-table button', words.names.PLATFORM_AUDITOR);
      for (const width of [1300, 768, 390]) {
        await page.setViewport({ width, height: 907 }); await noOverflow();
        const measured = await page.evaluate(() => {
          const input = document.querySelector('.adm-role-toolbar input').getBoundingClientRect();
          const table = document.querySelector('.adm-role-table').getBoundingClientRect();
          const wrap = document.querySelector('.adm-role-catalog-layout>.adm-table-wrap').getBoundingClientRect();
          return { inputHeight: input.height, tableWidth: table.width, wrapWidth: wrap.width };
        });
        assert.ok(measured.inputHeight <= 48 && measured.tableWidth <= measured.wrapWidth + 1, JSON.stringify(measured));
        if (process.env.UI_ARTIFACT_DIR && language === 'zh' && [1300, 390].includes(width)) {
          fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
          await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `roles-${width === 1300 ? 'desktop' : 'mobile'}.png`) });
        }
      }
    }
    await page.type('.adm-role-toolbar input[type=search]', 'billing.ledger');
    assert.equal(await page.$$eval('.adm-role-table tbody tr', nodes => nodes.length), 2);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('roles: long workspace navigation resets the heading below the fixed top bar', async () => {
    await readyRoles();
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await adminTab('billing'); await page.waitForSelector('[data-workspace=wallets]:not([hidden])');
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await adminTab('roles'); await page.waitForSelector('.adm-role-table tbody tr');
    const position = await page.evaluate(() => ({ scroll: scrollY, heading: document.querySelector('.adm-page-head').getBoundingClientRect().top, header: document.querySelector('.adm-topbar').getBoundingClientRect().bottom }));
    assert.equal(position.scroll, 0); assert.ok(position.heading >= position.header, JSON.stringify(position));
  });
  await run('roles: guided user lookup, reviewed grant, and cancellation preserve real HTTP boundaries', async () => {
    await readyRoles(); const words = await roleWords();
    await click('.adm-role-workspace-head button', words.newGrant);
    await page.type('[role=dialog] input[type=search]', 'remote');
    await click('[role=dialog] button', words.searchAction);
    await page.waitForFunction(() => [...document.querySelectorAll('[role=dialog] option')].some(option => option.textContent.startsWith('remote-user')));
    await page.select('[role=dialog] select', '22222222-2222-4222-8222-222222222222');
    await click('[role=dialog] button', words.next);
    assert.ok(!(await page.$$eval('[role=dialog] option', nodes => nodes.map(node => node.value))).includes('PLATFORM_SUPER_ADMIN'));
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.equal(await page.evaluate(() => document.activeElement.textContent), words.newGrant);
    assert.equal(await page.evaluate(() => window.restored.roleReceipts.size), 0);
    await openRoleReview();
    if (process.env.UI_ARTIFACT_DIR) {
      fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
      await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, 'role-grant-review.png') });
    }
    assert.ok((await page.$eval('[role=dialog] .adm-role-preview', node => node.textContent)).includes('22222222-2222-4222-8222-222222222222'));
    assert.equal(await page.$$eval('[role=dialog] .adm-role-preview>div', (nodes, label) => nodes.find(node => node.querySelector('dt').textContent === label)?.querySelector('dd').textContent, words.permissionCount), '2');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')).length), 0);
    await click('[role=dialog] button', words.confirmAndGrant);
    await page.waitForFunction(() => !document.querySelector('[role=dialog]') && new URL(location.href).searchParams.get('role_view') === 'bindings' && document.querySelector('.adm-role-binding-table tbody tr'));
    assert.equal(await page.evaluate(() => window.restored.roleReceipts.size), 1);
    assert.equal(new URL(page.url()).searchParams.get('role_user_id'), '22222222-2222-4222-8222-222222222222');
  });
  await run('roles: unknown grant retains one original command through outer navigation and recovery', async () => {
    await readyRoles(); const words = await openRoleReview();
    await page.evaluate(() => { window.restored.roleUnknown = true; });
    await click('[role=dialog] button', words.confirmAndGrant);
    await page.waitForFunction(() => !document.querySelector('[role=dialog]') && document.querySelector('.adm-role-risk-note button'));
    assert.ok(await page.$eval('.adm-role-workspace-head .adm-role-primary', button => button.disabled));
    await adminTab('users'); await page.waitForSelector('[data-workspace=users]:not([hidden])');
    await adminTab('roles'); await page.waitForSelector('.adm-role-risk-note button:not(:disabled)');
    await page.evaluate(() => { const url = new URL(location.href); url.searchParams.set('role_user_id', '11111111-1111-4111-8111-111111111111'); url.searchParams.set('role_view', 'bindings'); history.pushState({}, '', url); dispatchEvent(new PopStateEvent('popstate')); });
    await page.waitForFunction(() => document.querySelector('.adm-role-toolbar input:not([type])')?.value === '11111111-1111-4111-8111-111111111111');
    assert.ok((await page.$eval('.adm-role-risk-note>p:nth-child(2)', node => node.textContent)).includes('22222222-2222-4222-8222-222222222222'));
    await page.waitForSelector('.adm-role-risk-note button:not(:disabled)');
    await page.click('.adm-role-risk-note button');
    await page.waitForFunction(() => !document.querySelector('.adm-role-risk-note') && window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')).length === 2);
    const state = await page.evaluate(() => ({ requests: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')), effects: window.restored.roleReceipts.size }));
    assert.equal(state.requests[0].key, state.requests[1].key); assert.equal(state.requests[0].body, state.requests[1].body); assert.equal(state.effects, 1);
  });
  for (const status of ['REVOKED', 'EXPIRED']) await run(`roles: replayed grant reports its current ${status} projection`, async () => {
    await readyRoles();
    const expires = status === 'EXPIRED' ? await page.evaluate(() => { const value = new Date(Date.now() + 120000); return new Date(value.getTime() - value.getTimezoneOffset() * 60000).toISOString().slice(0, 16); }) : '';
    const words = await openRoleReview(undefined, undefined, expires);
    await page.evaluate(() => { window.restored.roleUnknown = true; });
    await click('[role=dialog] button', words.confirmAndGrant);
    await page.waitForSelector('.adm-role-risk-note button:not(:disabled)');
    await page.evaluate(status => { for (const receipt of window.restored.roleReceipts.values()) {
      if (status === 'REVOKED') { receipt.status = status; receipt.revoked_by = window.restored.user; }
      else window.restored.roleClockMs = Date.parse(receipt.expires_at) + 1;
    } }, status);
    await page.click('.adm-role-risk-note button');
    await page.waitForSelector('.adm-inline-message.ok');
    assert.equal(await page.$eval('.adm-inline-message.ok', node => node.textContent), status === 'REVOKED' ? words.revokedNotice : words.expiredNotice);
    const result = await page.evaluate(() => ({ effects: window.restored.roleReceipts.size, calls: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')) }));
    assert.equal(result.effects, 1); assert.equal(result.calls.length, 2);
    assert.equal(result.calls[0].key, result.calls[1].key); assert.equal(result.calls[0].body, result.calls[1].body);
    if (status === 'EXPIRED') assert.ok(JSON.parse(result.calls[0].body).expires_at);
  });
  await run('roles: complete user codes use the dedicated lookup contract', async () => {
    await readyRoles(); const words = await roleWords();
    await click('.adm-role-workspace-head button', words.newGrant);
    await page.type('[role=dialog] input[type=search]', 'USR-0123-4567-89AB-CDEF-MV');
    await click('[role=dialog] button', words.searchAction);
    await page.waitForFunction(() => [...document.querySelectorAll('[role=dialog] option')].some(node => node.textContent.startsWith('coded-user')));
    await page.select('[role=dialog] select', '22222222-2222-4222-8222-222222222222');
    assert.equal(await page.$eval('[role=dialog] .adm-role-field input', node => node.value), '22222222-2222-4222-8222-222222222222');
    const lookups = await page.evaluate(() => window.restored.calls.filter(call => call.url.includes('/user-code-lookups/')));
    assert.equal(lookups.length, 1); assert.ok(decodeURIComponent(lookups[0].url).endsWith('/USR-0123-4567-89AB-CDEF-MV'));
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.equal(await page.evaluate(() => window.restored.roleReceipts.size), 0);
  });
  await run('roles: target filtering follows URL history including removal and manual submission', async () => {
    await fresh('?mode=admin&section=roles&role_view=bindings&role_user_id=22222222-2222-4222-8222-222222222222');
    await page.waitForSelector('.adm-role-binding-table tbody tr');
    await page.evaluate(() => {
      window.restored.roleBindings.push({ ...window.restored.roleBindings[0], binding_id: '88888888-8888-4888-8888-888888888888', user_id: '11111111-1111-4111-8111-111111111111' });
      const url = new URL(location.href); url.searchParams.delete('role_user_id'); history.pushState({}, '', url); dispatchEvent(new PopStateEvent('popstate'));
    });
    await page.waitForFunction(() => document.querySelector('.adm-role-toolbar input:not([type])')?.value === '' && document.querySelectorAll('.adm-role-binding-table tbody tr').length === 2);
    await page.type('.adm-role-toolbar input:not([type])', '22222222-2222-4222-8222-222222222222');
    await page.click('form.adm-role-toolbar button');
    await page.waitForFunction(() => new URL(location.href).searchParams.get('role_user_id') === '22222222-2222-4222-8222-222222222222' && document.querySelectorAll('.adm-role-binding-table tbody tr').length === 1);
    await page.goBack();
    await page.waitForFunction(() => document.querySelector('.adm-role-toolbar input:not([type])')?.value === '' && document.querySelectorAll('.adm-role-binding-table tbody tr').length === 2);
    await page.goForward();
    await page.waitForFunction(() => document.querySelector('.adm-role-toolbar input:not([type])')?.value === '22222222-2222-4222-8222-222222222222' && document.querySelectorAll('.adm-role-binding-table tbody tr').length === 1);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')).length), 0);
  });
  await run('roles: user detail shortcut overrides a remembered catalog tab', async () => {
    await fresh('?mode=admin&section=roles&role_view=catalog');
    await page.waitForSelector('.adm-role-table tbody tr'); await adminTab('users');
    await page.waitForSelector('[data-workspace=users] tbody button');
    await click('[data-workspace=users] tbody button', 'sample-account');
    await page.waitForSelector('[data-workspace=users] .adm-user-details');
    await click('[data-workspace=users] .adm-panel button', await page.evaluate(() => window.restored.copy().adminLayout.nav.roles));
    await page.waitForFunction(() => new URL(location.href).searchParams.get('role_view') === 'bindings' && document.querySelector('.adm-role-toolbar input:not([type])')?.value === '11111111-1111-4111-8111-111111111111');
    assert.equal(new URL(page.url()).searchParams.get('role_user_id'), '11111111-1111-4111-8111-111111111111');
  });
  await run('roles: a confirmed grant survives a failed list refresh without a second mutation', async () => {
    await readyRoles(); const words = await openRoleReview();
    await page.evaluate(() => { window.restored.roleReadFail = true; });
    await click('[role=dialog] button', words.confirmAndGrant);
    await page.waitForFunction(() => document.querySelector('.adm-role-workspace .adm-inline-message.ok') && document.querySelector('.adm-role-workspace [role=alert]'));
    assert.equal(await page.$eval('.adm-role-workspace .adm-inline-message.ok', node => node.textContent), words.grantedNotice);
    assert.equal(await page.$eval('.adm-role-workspace [role=alert]', node => node.textContent), await page.evaluate(() => window.restored.copy().roles.loadFailed));
    assert.equal(await page.$('.adm-role-risk-note'), null, 'read failure made an acknowledged write unknown');
    await page.evaluate(() => { window.restored.roleReadFail = false; });
    await click('.adm-role-workspace-head button', await page.evaluate(() => window.restored.copy().roles.refresh));
    await page.waitForSelector('.adm-role-binding-table tbody tr');
    assert.equal(await page.evaluate(() => window.restored.roleReceipts.size), 1);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')).length), 1);
  });
  await run('roles: a late recovery ACK does not navigate away from another workspace', async () => {
    await readyRoles(); const words = await openRoleReview();
    await page.evaluate(() => { window.restored.roleUnknown = true; });
    await click('[role=dialog] button', words.confirmAndGrant);
    await page.waitForSelector('.adm-role-risk-note button:not(:disabled)');
    await page.evaluate(() => { window.restored.roleHoldGrant = true; });
    await page.click('.adm-role-risk-note button');
    await page.waitForFunction(() => typeof window.restored.releaseRoleGrantAck === 'function');
    await adminTab('users'); await page.waitForSelector('[data-workspace=users]:not([hidden])');
    await page.evaluate(() => window.restored.releaseRoleGrantAck());
    await page.waitForFunction(() => !document.querySelector('.adm-role-risk-note'));
    assert.equal(new URL(page.url()).searchParams.get('section'), 'users');
    assert.equal(await page.evaluate(() => window.restored.roleReceipts.size), 1);
  });
  await run('roles: list filter navigation cannot discard a same-session command ACK', async () => {
    await readyRoles(); const words = await openRoleReview();
    await page.evaluate(() => { window.restored.roleUnknown = true; });
    await click('[role=dialog] button', words.confirmAndGrant);
    await page.waitForSelector('.adm-role-risk-note button:not(:disabled)');
    await page.evaluate(() => { window.restored.roleHoldGrant = true; });
    await page.click('.adm-role-risk-note button');
    await page.waitForFunction(() => typeof window.restored.releaseRoleGrantAck === 'function');
    await page.evaluate(() => { const url = new URL(location.href); url.searchParams.set('role_view', 'bindings'); url.searchParams.set('role_user_id', '11111111-1111-4111-8111-111111111111'); history.pushState({}, '', url); dispatchEvent(new PopStateEvent('popstate')); });
    await page.waitForFunction(() => document.querySelector('.adm-role-toolbar input:not([type])')?.value === '11111111-1111-4111-8111-111111111111' && [...window.restored.calls].reverse().find(call => call.method === 'GET' && call.url.includes('/role-bindings'))?.url.includes('user_id=11111111-1111-4111-8111-111111111111'));
    await page.evaluate(() => window.restored.releaseRoleGrantAck());
    await page.waitForFunction(() => !document.querySelector('.adm-role-risk-note') && document.querySelector('.adm-inline-message.ok') && !document.querySelector('.adm-role-workspace-head .adm-role-primary').disabled);
    assert.equal(await page.$eval('.adm-inline-message.ok', node => node.textContent), words.grantedNotice);
    assert.equal(new URL(page.url()).searchParams.get('role_user_id'), '11111111-1111-4111-8111-111111111111');
    assert.equal(await page.$eval('.adm-role-toolbar input:not([type])', node => node.value), '11111111-1111-4111-8111-111111111111');
    const state = await page.evaluate(() => ({ calls: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')), effects: window.restored.roleReceipts.size }));
    assert.equal(state.calls.length, 2); assert.equal(state.effects, 1); assert.equal(state.calls[0].key, state.calls[1].key); assert.equal(state.calls[0].body, state.calls[1].body);
    assert.equal(await page.$$eval('.adm-role-binding-table tbody tr', nodes => nodes.length), 0);
  });
  await run('roles: missing user-read permission and lost assignment authority cannot dispatch a grant', async () => {
    await readyRoles();
    await page.evaluate(() => { window.restored.permissions = ['platform.roles.read', 'platform.roles.assign']; window.restored.reload(); });
    await page.waitForSelector('.adm-role-table tbody tr');
    const words = await openRoleReview();
    assert.equal(await page.$('[role=dialog] input[type=search]'), null);
    await page.evaluate(() => { window.restored.permissions = ['platform.roles.read']; });
    await click('[role=dialog] button', words.confirmAndGrant);
    await page.waitForFunction(() => !document.querySelector('[role=dialog]') && !document.querySelector('.adm-role-workspace-head .adm-role-primary'));
    assert.equal(await page.evaluate(() => window.restored.roleReceipts.size), 0);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')).length), 0);
  });
  await run('roles: binding detail and revoke retain the exact binding and read-only boundary', async () => {
    await readyRoles(); const words = await roleWords();
    await click('.adm-role-workspace-head button', words.bindingManagement);
    await page.waitForSelector('.adm-role-binding-table tbody tr');
    await click('.adm-role-binding-table button', words.view);
    await page.waitForSelector('[role=dialog] .adm-role-preview');
    const details = await page.$eval('[role=dialog]', node => node.textContent);
    assert.ok(details.includes('66666666-6666-4666-8666-666666666666') && details.includes('22222222-2222-4222-8222-222222222222'));
    page.once('dialog', dialog => { void dialog.accept(); });
    await click('[role=dialog] button', await page.evaluate(() => window.restored.copy().roles.revoke));
    await page.waitForFunction(() => !document.querySelector('[role=dialog]') && document.querySelector('.adm-inline-message.ok'));
    assert.equal(await page.$eval('.adm-inline-message.ok', node => node.textContent), words.revokedNotice);
    const mutations = await page.evaluate(() => window.restored.calls.filter(call => call.method === 'DELETE'));
    assert.equal(mutations.length, 1); assert.ok(mutations[0].url.endsWith('/66666666-6666-4666-8666-666666666666'));
    await page.evaluate(() => { window.restored.permissions = ['platform.roles.read']; window.restored.reload(); });
    await page.waitForSelector('.adm-role-binding-table tbody tr');
    await click('.adm-role-binding-table button', words.view);
    assert.equal(await page.$('[role=dialog] .adm-role-drawer-foot button'), null);
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'DELETE').length), 1);
  });
  await run('roles: temporary sensitive export cannot bypass expiry and reason validation in the wizard', async () => {
    await readyRoles(); const words = await roleWords();
    await click('.adm-role-workspace-head button', words.newGrant);
    await page.type('[role=dialog] .adm-role-field input', '22222222-2222-4222-8222-222222222222');
    await click('[role=dialog] button', words.next);
    await page.select('[role=dialog] select', 'PLATFORM_AUDIT_PII_EXPORT');
    await click('[role=dialog] button', words.next);
    await page.waitForSelector('[role=dialog] [role=alert]');
    assert.equal(await page.$('[role=dialog] .adm-role-preview'), null);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')).length), 0);
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
  });
  await run('roles: grant filtering and twenty-row pagination remain read-only across catalog tabs', async () => {
    await readyRoles(); const words = await roleWords();
    await page.evaluate(() => { const base = window.restored.roleBindings[0]; window.restored.roleBindings = Array.from({ length: 45 }, (_, i) => ({ ...base, binding_id: `66666666-6666-4666-8666-${String(i + 1).padStart(12, '0')}`, role_code: i < 22 ? 'PLATFORM_AUDITOR' : 'PLATFORM_BILLING_OPERATOR' })); });
    await click('.adm-role-workspace-head button', await page.evaluate(() => window.restored.copy().roles.refresh));
    await click('.adm-role-workspace-head button', words.bindingManagement);
    await page.waitForFunction(() => document.querySelectorAll('.adm-role-binding-table tbody tr').length === 20);
    await click('.adm-role-panel .adm-pagination button', words.nextPage);
    assert.equal(await page.$$eval('.adm-role-binding-table tbody tr', nodes => nodes.length), 20);
    await click('.adm-role-panel .adm-pagination button', words.nextPage);
    assert.equal(await page.$$eval('.adm-role-binding-table tbody tr', nodes => nodes.length), 5);
    await page.type('.adm-role-panel>div:not([hidden]) input[type=search][placeholder]', 'PLATFORM_AUDITOR');
    assert.equal(await page.$$eval('.adm-role-binding-table tbody tr', nodes => nodes.length), 20);
    await click('.adm-role-workspace-head button', words.catalog);
    await click('.adm-role-workspace-head button', words.bindingManagement);
    assert.equal(await page.$eval('.adm-role-panel>div:not([hidden]) input[type=search]', node => node.value), 'PLATFORM_AUDITOR');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  for (const language of ['zh', 'en', 'ja']) await run(`roles ${language}: the admin consumer shows the real 500-entry truncation boundary`, async () => {
    await readyRoles();
    await page.select('.adm-sidebar-foot select', language);
    await page.waitForSelector('.adm-role-workspace-head');
    const copy = await page.evaluate(() => window.restored.copy().roles);
    const words = await roleWords();
    await click('.adm-role-workspace-head button', words.bindingManagement);
    for (const count of [500, 501]) {
      await page.evaluate(count => {
        const base = window.restored.roleBindings[0];
        window.restored.roleBindings = Array.from({ length: count }, (_, i) => ({ ...base, binding_id: `66666666-6666-4666-8666-${String(i + 1).padStart(12, '0')}` }));
      }, count);
      await click('.adm-role-workspace-head button', copy.refresh);
      await page.waitForFunction(({ refresh, message, truncated }) => {
        const ready = [...document.querySelectorAll('.adm-role-workspace-head button')].some(node => node.textContent.trim() === refresh && !node.disabled);
        const notice = [...document.querySelectorAll('.adm-inline-message[role=status]')].some(node => node.textContent === message);
        return ready && notice === truncated && document.querySelectorAll('.adm-role-binding-table tbody tr').length === 20;
      }, {}, { refresh: copy.refresh, message: copy.bindingsTruncated, truncated: count === 501 });
      const notice = await page.$$eval('.adm-inline-message[role=status]', nodes => nodes.map(node => node.textContent));
      assert.equal(notice.includes(copy.bindingsTruncated), count === 501);
    }
    await page.evaluate(() => { window.restored.roleBindings = []; });
    await click('.adm-role-workspace-head button', copy.refresh);
    await page.waitForFunction(refresh => document.querySelector('.adm-empty') && [...document.querySelectorAll('.adm-role-workspace-head button')].some(node => node.textContent.trim() === refresh && !node.disabled), {}, copy.refresh);
    assert.equal((await page.$$eval('.adm-inline-message[role=status]', nodes => nodes.map(node => node.textContent))).includes(copy.bindingsTruncated), false);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  for (const kind of ['account', 'admin']) for (const language of ['zh', 'en', 'ja']) await run(`${kind} ${language}: responsive original frame, localized navigation`, async () => {
    await fresh(`?mode=${kind}&section=${kind === 'admin' ? 'users' : 'creditsUsage'}`);
    await page.select(kind === 'admin' ? '.adm-sidebar-foot select' : '.ap-sidebar-foot select', language);
    for (const width of [1440, 768, 390]) { await page.setViewport({ width, height: 1000 }); await noOverflow(); }
    if (process.env.UI_ARTIFACT_DIR && language === 'zh') {
      fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
      await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `${kind}-mobile.png`), fullPage: true });
      await page.setViewport({ width: 1440, height: 1000 });
      await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `${kind}-desktop.png`), fullPage: true });
    }
  });
  console.log(`RESULT ${passed} PASS 0 FAIL ${skipped} SKIP`);
} catch (error) { console.error(error); process.exitCode = 1; }
finally { if (browser) await browser.close(); await server.close(); }
