import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createServer } from './helpers/vite-fixture.mjs';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { fixtureScope } from './helpers/fixture-scope.mjs';
import { assertStableLayoutHeight } from './helpers/layout-assertions.mjs';

const root = path.resolve(import.meta.dirname, '..');
if (process.env.UI_ARTIFACT_DIR) fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
const chrome = [process.env.CHROME, 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(value => value && fs.existsSync(value));
assert.ok(chrome, 'Chromium required; validation cannot be skipped');
const server = await createServer({ root, configFile: false, logLevel: 'error', plugins: [react()], css: { postcss: { plugins: [] } }, server: { host: '127.0.0.1', port: 0 } });
let browser, passed = 0, skipped = 0;
try {
  await server.listen();
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
    catch (error) {
      const ledger = name === 'ledger points preserve exact precheck and frozen replay' ? await page.evaluate(() => {
        const form = document.querySelector('.adm-financial-repair'), amount = form?.querySelector('[data-ledger-amount]');
        return { amount: amount && { value: amount.value, disabled: amount.disabled, invalid: amount.getAttribute('aria-invalid'), required: amount.required, valid: amount.validity.valid }, buttons: [...(form?.querySelectorAll('button') || [])].map(node => ({ text: node.textContent, disabled: node.disabled, laidOut: node.getClientRects().length > 0 })), prechecks: window.restored?.calls.filter(call => call.url === '/api/admin/account/admin/v2/approval-command-prechecks').length, drafts: window.restored?.calls.filter(call => call.method === 'POST' && call.url === '/api/admin/approvals/requests').length, text: form?.textContent.slice(0, 2400) };
      }).catch(() => ({ unavailable: true })) : undefined;
      const userStatus = name === 'user status actions expose danger target and self guard' ? await page.evaluate(() => ({ buttons: [...document.querySelectorAll('[data-user-status-target],[data-user-status-confirm]')].map(node => ({ target: node.dataset.userStatusTarget, confirm: node.dataset.userStatusConfirm, disabled: node.disabled, text: node.textContent, label: node.getAttribute('aria-label'), color: getComputedStyle(node).color, background: getComputedStyle(node).backgroundColor })), writes: window.restored?.calls.filter(call => call.method === 'PATCH' && call.url.endsWith('/status')) })).catch(() => ({ unavailable: true })) : undefined;
      const userSearch = name === 'user searches refresh applied context and keep lookup kinds distinct' ? await page.evaluate(() => ({ location: location.search, inputs: [...document.querySelectorAll('.adm-user-search-toolbar input')].map(node => node.value), buttons: [...document.querySelectorAll('.adm-user-search-toolbar button')].map(node => ({ text: node.textContent, disabled: node.disabled })), error: document.querySelector('.adm-panel [role=alert]')?.textContent, calls: window.restored?.calls.filter(call => /\/admin\/v1\/(users\?|user-code-lookups\/)/.test(call.url)) })).catch(() => ({ unavailable: true })) : undefined;
      const userDetail = name === 'user detail heading keeps real identity and the applied directory context' ? await page.evaluate(() => ({ location: location.search, title: document.querySelector('.adm-page-head h2')?.textContent, identity: document.querySelector('.adm-user-detail-head')?.textContent, alert: document.querySelector('[data-workspace=users] [role=alert]')?.textContent, directory: document.querySelector('[data-user-directory-search] input')?.value, reads: window.restored?.calls.filter(call => call.method === 'GET' && call.url.includes('/admin/v1/users')) })).catch(() => ({ unavailable: true })) : undefined;
      const footer = name === 'admin footer link and language control preserve readable query context' ? await page.evaluate(() => ({ location: location.search, link: document.querySelector('.adm-sidebar-foot>a')?.textContent, href: document.querySelector('.adm-sidebar-foot>a')?.getAttribute('href'), language: document.querySelector('.adm-sidebar-foot select')?.value, draft: document.querySelector('[data-user-directory-search] input')?.value, controls: [...document.querySelectorAll('.adm-sidebar-foot>a,.adm-sidebar-foot select')].map(node => { const rect = node.getBoundingClientRect(); return { tag: node.tagName, width: rect.width, height: rect.height, left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom }; }) })).catch(() => ({ unavailable: true })) : undefined;
      console.error(JSON.stringify({ case: name, pageErrors: errors, ledger, userStatus, userSearch, userDetail, footer, dialog: await page.evaluate(() => document.querySelector('[role=dialog]')?.textContent.slice(0, 1200)) })); throw error;
    }
  };
  await run('admin footer link and language control preserve readable query context', async () => {
    const linkSelector = '.adm-sidebar-foot > a', languageSelector = '.adm-sidebar-foot select';
    const cursor = 'eyJ1cGRhdGVkX2F0IjoiMjAyNi0xMC0wMVQwMDowMDowMFoiLCJ1c2VyX2lkIjoiMzMzMzMzMzMtMzMzMy00MzMzLTgzMzMtMzMzMzMzMzMzMzMzIn0';
    const userReads = () => page.evaluate(() => window.restored.calls.filter(call => call.method === 'GET' && new URL(call.url, location.origin).pathname === '/api/admin/account/admin/v1/users').length);
    await fresh(`?mode=admin&userDetailHeader=1&section=users&user_q=${encodeURIComponent('验证')}&user_cursor=${cursor}`);
    await page.waitForFunction(() => document.querySelector('[data-user-results-refresh]') && !document.querySelector('[data-user-results-refresh]').disabled);
    await page.type('[data-user-directory-search] input', '未提交');
    const before = await userReads();
    if (process.env.UI_SCREENSHOT_DIR) fs.mkdirSync(process.env.UI_SCREENSHOT_DIR, { recursive: true });
    for (const width of [1440, 390]) {
      await page.setViewport({ width, height: 1000 }); await page.evaluate(() => scrollTo(0, 0));
      for (const [language, home] of [['zh', '个人中心'], ['en', 'Personal center'], ['ja', '個人センター'], ['zh', '个人中心']]) {
        await page.select(languageSelector, language);
        await page.waitForFunction(({ language, home }) => document.querySelector('.adm-sidebar-foot select')?.value === language && document.querySelector('.adm-sidebar-foot > a')?.textContent === home, {}, { language, home });
        await noOverflow();
        assert.equal(await page.$eval(linkSelector, node => node.getAttribute('href')), '/');
        assert.deepEqual(await page.$$eval('.adm-sidebar-foot select option', nodes => nodes.map(node => node.value)), ['zh', 'en', 'ja']);
        assert.equal(await page.$eval('[data-user-directory-search] input', node => node.value), '验证未提交');
        const url = new URL(page.url()); assert.equal(url.searchParams.get('user_q'), '验证'); assert.equal(url.searchParams.get('user_cursor'), cursor); assert.equal(url.searchParams.get('section'), 'users');
        assert.equal(await userReads(), before, 'footer locale changes do not rerun or replace the current directory search');
        const geometry = await page.$eval(linkSelector, node => {
          const footer = node.parentElement, select = footer.querySelector('select'), clip = { left: 0, top: 0, right: innerWidth, bottom: innerHeight };
          for (let ancestor = footer; ancestor; ancestor = ancestor.parentElement) {
            const style = getComputedStyle(ancestor), rect = ancestor.getBoundingClientRect(), clips = value => ['auto', 'scroll', 'hidden', 'clip', 'overlay'].includes(value);
            if (clips(style.overflowX)) { clip.left = Math.max(clip.left, rect.left + ancestor.clientLeft); clip.right = Math.min(clip.right, rect.left + ancestor.clientLeft + ancestor.clientWidth); }
            if (clips(style.overflowY)) { clip.top = Math.max(clip.top, rect.top + ancestor.clientTop); clip.bottom = Math.min(clip.bottom, rect.top + ancestor.clientTop + ancestor.clientHeight); }
          }
          const box = element => { const rect = element.getBoundingClientRect(); return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width, height: rect.height }; };
          const range = document.createRange(); range.selectNodeContents(node);
          const style = getComputedStyle(node);
          return { clip, link: box(node), select: box(select), footer: box(footer), fragments: [...range.getClientRects()].map(rect => ({ left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width, height: rect.height })), fontSize: style.fontSize, sidebarFont: getComputedStyle(node.closest('.adm-sidebar')).fontSize };
        });
        const inside = rect => Object.values(rect).every(Number.isFinite) && rect.width > 0 && rect.height > 0 && rect.left >= geometry.clip.left - 0.5 && rect.right <= geometry.clip.right + 0.5 && rect.top >= geometry.clip.top - 0.5 && rect.bottom <= geometry.clip.bottom + 0.5;
        assert.ok(Object.values(geometry.clip).every(Number.isFinite) && geometry.clip.right > geometry.clip.left && geometry.clip.bottom > geometry.clip.top);
        assert.equal(geometry.fragments.length, 1, `the home link must stay on a single readable line: ${JSON.stringify(geometry)}`);
        assert.ok(geometry.fragments.every(rect => inside(rect) && rect.left >= geometry.link.left - 0.5 && rect.right <= geometry.link.right + 0.5 && rect.top >= geometry.link.top - 0.5 && rect.bottom <= geometry.link.bottom + 0.5) && inside(geometry.link) && inside(geometry.select), `footer text or controls are clipped: ${JSON.stringify(geometry)}`);
        assert.ok(geometry.select.width >= 100, 'language control retains usable width on supported test viewports');
        assert.ok(geometry.link.right <= geometry.select.left + 0.5 || geometry.select.right <= geometry.link.left + 0.5 || geometry.link.bottom <= geometry.select.top + 0.5 || geometry.select.bottom <= geometry.link.top + 0.5, 'home and language controls do not overlap');
        assert.ok(Number.parseFloat(geometry.fontSize) >= Number.parseFloat(geometry.sidebarFont), 'readability is not achieved by shrinking the link font');
        console.log(JSON.stringify({ case: 'admin-footer-visibility', width, language, ...geometry }));
      }
      await page.focus(linkSelector); await page.keyboard.press('Tab'); await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
      assert.ok(await page.$eval(linkSelector, node => node.matches(':focus-visible') && Number.parseFloat(getComputedStyle(node).outlineWidth) > 0 && getComputedStyle(node).outlineStyle !== 'none'), 'native keyboard focus remains visible');
      if (process.env.UI_SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.UI_SCREENSHOT_DIR, `admin-footer-${width}.png`) });
    }
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('user detail heading keeps real identity and the applied directory context', async () => {
    const target = '22222222-2222-4222-8222-222222222222', name = '用于验证用户详情标题和返回入口布局的测试用户名称 WWWWWWWWWWWWWWWWWWWW';
    const cursor = 'eyJ1cGRhdGVkX2F0IjoiMjAyNi0xMC0wMVQwMDowMDowMFoiLCJ1c2VyX2lkIjoiMzMzMzMzMzMtMzMzMy00MzMzLTgzMzMtMzMzMzMzMzMzMzMzIn0';
    const directoryCalls = () => page.evaluate(() => window.restored.calls.filter(call => call.method === 'GET' && new URL(call.url, location.origin).pathname === '/api/admin/account/admin/v1/users'));
    await fresh(`?mode=admin&userDetailHeader=1&section=users&user_q=${encodeURIComponent('验证')}&user_cursor=${cursor}`);
    await page.waitForFunction(name => document.querySelector('[data-workspace=users] tbody button')?.textContent === name, {}, name);
    assert.equal(await page.$eval('.adm-page-head h2', node => node.textContent), '用户管理');
    const before = (await directoryCalls()).length;
    await page.type('[data-user-directory-search] input', '未提交'); assert.equal((await directoryCalls()).length, before);
    await page.click('[data-workspace=users] tbody button');
    await page.waitForFunction(name => document.querySelector('.adm-user-detail-head h3')?.textContent === name, {}, name);
    assert.equal(new URL(page.url()).searchParams.get('user_id'), target);
    assert.ok((await page.$eval('.adm-user-details', node => node.textContent)).includes(target));
    const profileReads = await page.evaluate(target => window.restored.calls.filter(call => call.method === 'GET' && call.url === `/api/admin/account/admin/v1/users/${target}`).length, target);
    for (const [language, title, back] of [['zh', '用户详情', '返回用户列表'], ['en', 'User details', 'Back to user list'], ['ja', 'ユーザー詳細', 'ユーザー一覧に戻る'], ['zh', '用户详情', '返回用户列表']]) {
      await page.select('.adm-sidebar-foot select', language);
      await page.waitForFunction(({ title, back }) => document.querySelector('.adm-page-head h2')?.textContent === title && document.querySelector('.adm-user-detail-head > button')?.textContent === back, {}, { title, back });
      assert.equal(await page.$eval('.adm-user-detail-head h3', node => node.textContent), name);
      assert.deepEqual(await page.$$eval('.adm-user-profile-facts .adm-identifier code', nodes => nodes.map(node => node.textContent)), [target, 'USR-1111-1111-1111-1111-DP']);
      assert.deepEqual(await page.$$eval('.adm-user-profile-facts .adm-identifier-copy', nodes => nodes.map(node => node.getAttribute('aria-label'))), await page.evaluate(() => { const words = window.restored.copy(); return [words.adminLayout.identifierActions.copy(words.adminLayout.userID), words.adminLayout.identifierActions.copy(words.admin.userManagement.exactCode)]; }));
      assert.equal(await page.$eval('.adm-user-profile-status', node => node.textContent), await page.evaluate(() => window.restored.copy().accountLayout.statuses.ACTIVE));
      assert.equal(await page.$eval('.adm-breadcrumbs [aria-current=page]', node => node.textContent), title);
    }
    assert.equal(await page.evaluate(target => window.restored.calls.filter(call => call.method === 'GET' && call.url === `/api/admin/account/admin/v1/users/${target}`).length, target), profileReads, 'language changes do not duplicate profile reads');
    assert.equal(await page.$eval('.adm-user-membership-facts', node => node.textContent), '成员状态有效角色所有者', 'the current membership presentation retains distinct localized facts');
    if (process.env.UI_SCREENSHOT_DIR) fs.mkdirSync(process.env.UI_SCREENSHOT_DIR, { recursive: true });
    for (const width of [1440, 390]) {
      await page.setViewport({ width, height: 1000 }); await page.evaluate(() => scrollTo(0, 0));
      for (const toggle of await page.$$('.adm-user-profile-facts .adm-identifier-toggle[aria-expanded=false]')) await toggle.click();
      await noOverflow();
      assert.equal(await page.$$eval('.adm-user-profile-facts .adm-identifier-toggle', nodes => nodes.every(node => node.getAttribute('aria-expanded') === 'true')), true);
      const identity = await page.$eval('.adm-user-detail-head h3', node => {
        const clip = { left: 0, top: 0, right: innerWidth, bottom: innerHeight };
        for (let ancestor = node.parentElement; ancestor; ancestor = ancestor.parentElement) {
          const style = getComputedStyle(ancestor), rect = ancestor.getBoundingClientRect(), clipping = value => ['auto', 'scroll', 'hidden', 'clip', 'overlay'].includes(value);
          if (clipping(style.overflowX)) { clip.left = Math.max(clip.left, rect.left + ancestor.clientLeft); clip.right = Math.min(clip.right, rect.left + ancestor.clientLeft + ancestor.clientWidth); }
          if (clipping(style.overflowY)) { clip.top = Math.max(clip.top, rect.top + ancestor.clientTop); clip.bottom = Math.min(clip.bottom, rect.top + ancestor.clientTop + ancestor.clientHeight); }
        }
        const range = document.createRange(); range.selectNodeContents(node);
        const back = node.closest('.adm-user-detail-head').querySelector(':scope > button').getBoundingClientRect(), heading = node.getBoundingClientRect();
        return { clip, back: { left: back.left, right: back.right, top: back.top, bottom: back.bottom }, headingTop: heading.top, fontSize: getComputedStyle(node).fontSize, bodyFontSize: getComputedStyle(node.closest('.adm-panel')).fontSize, fragments: [...range.getClientRects()].map(rect => ({ left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width, height: rect.height })) };
      });
      assert.ok(Number.parseFloat(identity.fontSize) > Number.parseFloat(identity.bodyFontSize), 'the real identity has a heading hierarchy above body text');
      assert.ok(identity.headingTop - identity.back.bottom >= 7.5, 'return remains separated from the identity heading by at least the 8px spacing step');
      assert.ok(Object.values(identity.clip).every(Number.isFinite) && identity.clip.right > identity.clip.left && identity.clip.bottom > identity.clip.top);
      assert.ok(Object.values(identity.back).every(Number.isFinite) && identity.back.right > identity.back.left && identity.back.bottom > identity.back.top && identity.back.left >= identity.clip.left - 0.5 && identity.back.right <= identity.clip.right + 0.5 && identity.back.top >= identity.clip.top - 0.5 && identity.back.bottom <= identity.clip.bottom + 0.5, 'return control remains fully inside the same visible header area');
      assert.ok(identity.fragments.length && identity.fragments.every(rect => Object.values(rect).every(Number.isFinite) && rect.width > 0 && rect.height > 0 && rect.left >= identity.clip.left - 0.5 && rect.right <= identity.clip.right + 0.5 && rect.top >= identity.clip.top - 0.5 && rect.bottom <= identity.clip.bottom + 0.5), `real identity text is clipped: ${JSON.stringify(identity)}`);
      if (width === 390) assert.ok(identity.fragments.length >= 2, 'long real display name naturally wraps on the narrow screen');
      console.log(JSON.stringify({ case: 'user-detail-identity-visibility', width, ...identity }));
      if (process.env.UI_SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.UI_SCREENSHOT_DIR, `user-detail-header-${width}.png`) });
    }
    await page.setViewport({ width: 1440, height: 1000 });
    await click('[data-workspace=users] .adm-panel button', '查看该用户的平台授权');
    await page.waitForFunction(target => new URL(location.href).searchParams.get('role_user_id') === target && new URL(location.href).searchParams.get('role_view') === 'bindings' && document.querySelector('.adm-role-toolbar input:not([type])')?.value === target, {}, target);
    await page.goBack(); await page.waitForFunction(({ name, target }) => new URL(location.href).searchParams.get('user_id') === target && document.querySelector('.adm-page-head h2')?.textContent === '用户详情' && document.querySelector('.adm-user-detail-head h3')?.textContent === name, {}, { name, target });
    await page.evaluate(() => { window.restored.headerReadStatus = 404; window.restored.reload(); });
    await page.waitForSelector('.adm-user-detail-head + [role=alert]');
    assert.equal(await page.$('.adm-user-detail-head h3'), null, 'a failed profile read does not leave the old identity heading');
    assert.equal(await page.evaluate(name => document.querySelector('[data-workspace=users]').textContent.includes(name), name), false);
    await page.click('.adm-user-detail-head > button');
    await page.waitForFunction(() => !new URL(location.href).searchParams.has('user_id') && document.querySelector('[data-user-directory-search] input')?.value === '验证' && !document.querySelector('[data-user-results-refresh]')?.disabled);
    const returned = new URL(page.url()); assert.equal(returned.searchParams.get('user_q'), '验证'); assert.equal(returned.searchParams.get('user_cursor'), cursor);
    assert.equal(await page.$eval('.adm-page-head h2', node => node.textContent), '用户管理'); assert.equal(await page.$('.adm-user-detail-breadcrumbs'), null);
    const last = new URL((await directoryCalls()).at(-1).url, 'https://fixture.invalid'); assert.equal(last.searchParams.get('q'), '验证'); assert.equal(last.searchParams.get('cursor'), cursor);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('user searches refresh applied context and keep lookup kinds distinct', async () => {
    const general = '[data-user-directory-search] input', exact = '[data-user-code-search] input', refresh = '[data-user-results-refresh]';
    const alice = 'alice-slow', bob = 'bob-user', code = 'USR-1111-1111-1111-1111-DP';
    const queries = () => page.evaluate(() => window.restored.calls.filter(call => call.method === 'GET' && /\/admin\/v1\/(users\?|user-code-lookups\/)/.test(call.url)).map(call => call.url));
    const codeSettled = () => page.evaluate(() => ({ index: window.restored.userSearchClients.length - 1, count: window.restored.userSearchClients.at(-1).userCodeReadsSettled }));
    const waitCodeSettled = snapshot => page.waitForFunction(({ index, count }) => window.restored.userSearchClients[index].userCodeReadsSettled === count + 1 && !document.querySelector('.adm-panel [role=alert] button')?.disabled, {}, snapshot);
    const replace = async (selector, value) => {
      await page.focus(selector); const modifier = process.platform === 'darwin' ? 'Meta' : 'Control';
      await page.keyboard.down(modifier); await page.keyboard.press('KeyA'); await page.keyboard.up(modifier); await page.keyboard.press('Backspace');
      if (value) await page.type(selector, value);
      assert.equal(await page.$eval(selector, node => node.value), value);
    };
    const result = name => page.waitForFunction(name => document.querySelector('.adm-table tbody tr td')?.textContent === name && !document.querySelector('[data-user-results-refresh]')?.disabled, {}, name);
    const assertDirectory = (url, q) => { const parsed = new URL(url, 'https://fixture.invalid'); assert.equal(parsed.pathname, '/api/admin/account/admin/v1/users'); assert.equal(parsed.searchParams.get('q'), q); assert.equal(parsed.searchParams.get('limit'), '25'); assert.equal(parsed.searchParams.get('cursor'), null); };
    await fresh('?mode=admin&userSearchTools=1&section=users&user_q=alice'); await result(alice);
    for (const [language, search, update, lookup] of [['zh', '搜索', '刷新当前结果', '精确查询'], ['en', 'Search', 'Refresh results', 'Exact lookup'], ['ja', '検索', '検索結果を更新', '完全一致で検索'], ['zh', '搜索', '刷新当前结果', '精确查询']]) {
      await page.select('.adm-sidebar-foot select', language);
      await page.waitForFunction(({ search, update, lookup }) => document.querySelector('[data-user-directory-search] button')?.textContent === search && document.querySelector('[data-user-results-refresh]')?.textContent === update && document.querySelector('[data-user-code-search] button')?.textContent === lookup, {}, { search, update, lookup });
      const acceptedSummary = await page.$eval('.adm-user-result-summary', node => node.textContent);
      const batchLabels = await page.evaluate(() => { const words = window.restored.copy().adminLayout.userResults; return [words.directory, words.returned(1), words.condition('alice'), words.capacity]; });
      assert.ok(batchLabels.every(label => acceptedSummary.includes(label)), 'batch information describes accepted URL conditions, never draft or invented totals');
      assert.equal(await page.$eval('.adm-user-list tbody .adm-identifier-value code', node => node.textContent), 'USR-0000-0000-0000-0000-Q3', 'the current row keeps its exact complete code');
      const codeSizes = await page.$$eval('.adm-user-list tbody .adm-identifier-value code', nodes => nodes.map(node => Number.parseFloat(getComputedStyle(node).fontSize)));
      assert.ok(codeSizes.length > 0 && codeSizes.every(size => size >= 12), 'list codes use the shared readable size');
    }
    let before = (await queries()).length;
    await replace(general, 'bob'); assert.equal((await queries()).length, before, 'typing is not a search');
    await page.click(refresh); await result(alice);
    let urls = await queries(); assert.equal(urls.length, before + 1); assertDirectory(urls.at(-1), 'alice'); assert.equal(await page.$eval(general, node => node.value), 'bob');
    before = urls.length; await page.focus(general); await page.keyboard.press('Enter'); await result(bob);
    urls = await queries(); assert.equal(urls.length, before + 1); assertDirectory(urls.at(-1), 'bob');
    assert.equal(await page.evaluate(() => new URLSearchParams(location.search).get('user_q')), 'bob');
    await page.goBack(); await result(alice); assert.equal(await page.$eval(general, node => node.value), 'alice');
    await page.goForward(); await result(bob); assert.equal(await page.$eval(general, node => node.value), 'bob');
    before = (await queries()).length;
    await replace(exact, code.toLowerCase()); assert.equal((await queries()).length, before);
    assert.equal(await page.$eval(refresh, node => node.disabled), true, 'editing a code clears the old result and disables refresh');
    await page.keyboard.press('Enter'); await result(bob); urls = await queries();
    assert.equal(urls.length, before + 1); assert.equal(urls.at(-1), `/api/admin/account/admin/v1/user-code-lookups/${code.toLowerCase()}`);
    await replace(general, 'alice'); before = (await queries()).length;
    await page.click(refresh); await result(bob); urls = await queries();
    assert.equal(urls.length, before + 1); assert.equal(urls.at(-1), `/api/admin/account/admin/v1/user-code-lookups/${code.toLowerCase()}`, 'dirty general draft cannot turn an exact refresh into directory search');
    before = urls.length; await replace(exact, 'bad code!'); await page.keyboard.press('Enter');
    await page.waitForSelector('.adm-panel [role=alert]'); assert.equal((await queries()).length, before, 'invalid code is rejected by the actual client without a broad fallback');
    let codeRead = await codeSettled(); await page.click('.adm-panel [role=alert] button'); await waitCodeSettled(codeRead);
    assert.equal((await queries()).length, before, 'retry keeps the rejected exact lookup kind');
    const missing = 'USR-2222-2222-2222-2222-8X'; await replace(exact, missing); await page.keyboard.press('Enter');
    await page.waitForSelector('.adm-panel [role=alert]'); urls = await queries(); assert.equal(urls.length, before + 1); assert.equal(urls.at(-1), `/api/admin/account/admin/v1/user-code-lookups/${missing}`);
    codeRead = await codeSettled(); await page.click('.adm-panel [role=alert] button'); await waitCodeSettled(codeRead);
    urls = await queries(); assert.equal(urls.length, before + 2); assert.equal(urls.at(-1), `/api/admin/account/admin/v1/user-code-lookups/${missing}`);
    await page.evaluate(() => { window.restored.holdUserSearchRead = true; });
    const held = await page.evaluate(() => ({ index: window.restored.userSearchClients.length - 1, settled: window.restored.userSearchClients.at(-1).userReadsSettled }));
    await replace(general, 'slow'); await page.keyboard.press('Enter'); await page.waitForFunction(() => window.restored.pendingUserSearchReads.length === 1);
    await replace(exact, code); await page.keyboard.press('Enter'); await result(bob);
    await page.evaluate(() => window.restored.releaseUserSearchRead());
    await page.waitForFunction(({ index, settled }) => window.restored.userSearchClients[index].userReadsSettled === settled + 1, {}, held);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(resolve))); await result(bob);
    assert.equal((await queries()).at(-1), `/api/admin/account/admin/v1/user-code-lookups/${code}`, 'late directory reply cannot overwrite the accepted exact result');
    before = (await queries()).length; await replace(general, ''); assert.equal((await queries()).length, before);
    await page.keyboard.press('Enter'); await page.waitForFunction(() => document.querySelectorAll('.adm-table tbody tr').length === 2 && !document.querySelector('[data-user-results-refresh]').disabled);
    assertDirectory((await queries()).at(-1), null); assert.equal(await page.evaluate(() => new URLSearchParams(location.search).get('user_q')), null, 'explicit empty search returns to the unfiltered directory');
    await replace(general, 'bob'); await page.keyboard.press('Enter'); await result(bob);
    if (process.env.UI_SCREENSHOT_DIR) fs.mkdirSync(process.env.UI_SCREENSHOT_DIR, { recursive: true });
    for (const width of [1440, 390]) {
      await page.setViewport({ width, height: 1000 }); await page.evaluate(() => scrollTo(0, 0)); await noOverflow();
      const controls = await page.$eval('.adm-user-search-toolbar', node => ({ box: { left: node.getBoundingClientRect().left, right: node.getBoundingClientRect().right }, children: [...node.querySelectorAll('input,button')].map(child => { const rect = child.getBoundingClientRect(); return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width, height: rect.height }; }) }));
      assert.ok(controls.children.every(rect => Object.values(rect).every(Number.isFinite) && rect.width > 0 && rect.height > 0 && rect.left >= controls.box.left - 0.5 && rect.right <= controls.box.right + 0.5), `search control outside coherent toolbar: ${JSON.stringify(controls)}`);
      if (process.env.UI_SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.UI_SCREENSHOT_DIR, `user-search-toolbar-${width}.png`) });
    }
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0, 'search and refresh never write');
  });
  await run('user status actions expose danger target and self guard', async () => {
    const actor = '11111111-1111-4111-8111-111111111111', target = '22222222-2222-4222-8222-222222222222', other = '33333333-3333-4333-8333-333333333333';
    const targetCode = 'USR-1111-1111-1111-1111-DP', otherCode = 'USR-2222-2222-2222-2222-8X';
    const selector = id => `[data-user-status-target="${id}"]`, confirm = '[data-user-status-confirm]';
    const writes = () => page.evaluate(() => window.restored.calls.filter(call => call.method === 'PATCH' && call.url.endsWith('/status')));
    const colors = selector => page.$eval(selector, node => { const style = getComputedStyle(node); return { color: style.color, background: style.backgroundColor, outline: style.outlineColor, outlineWidth: style.outlineWidth, focused: node.matches(':focus-visible') }; });
    const settledAnimations = selector => page.$eval(selector, async node => { await Promise.all(node.getAnimations().filter(animation => animation.playState === 'running' && Number.isFinite(animation.effect?.getComputedTiming().endTime)).map(animation => animation.finished.catch(() => {}))); });
    await fresh('?mode=admin&userStatusRisk=1&section=users'); await page.waitForSelector(selector(target));
    for (const [language, title, reason] of [
      ['zh', '禁用用户', '不能停用当前登录的管理员。'],
      ['en', 'Disable user', 'You cannot disable the signed-in administrator.'],
      ['ja', 'ユーザーを無効化', 'ログイン中の管理者は無効にできません。'],
      ['zh', '禁用用户', '不能停用当前登录的管理员。'],
    ]) {
      await page.evaluate(language => { window.restored.language = language; window.restored.reload(); }, language);
      await page.waitForFunction(({ selector, title }) => { const node = document.querySelector(selector); return node?.getAttribute('aria-label')?.startsWith(title) && !node.disabled; }, {}, { selector: selector(target), title });
      assert.ok((await page.$eval(selector(target), node => node.getAttribute('aria-label'))).includes(targetCode), 'accessible action disambiguates equal display names with the canonical code');
      assert.equal(await page.$eval(selector(actor), node => node.disabled), true);
      assert.equal(await page.$eval('.adm-user-status-blocked', node => node.textContent), reason);
      assert.ok(await page.$eval('.adm-user-status-blocked', node => { const style = getComputedStyle(node); return node.getClientRects().length > 0 && style.display !== 'none' && style.visibility !== 'hidden'; }), 'self reason is rendered, not title-only');
      await page.click(selector(actor)); assert.equal(await page.$('[role=dialog]'), null, 'native self guard cannot open confirmation');
      assert.equal((await writes()).length, 0);
    }
    const normal = await colors(selector(target)); assert.equal(normal.color, 'rgb(195, 59, 70)');
    await page.hover(selector(target)); await settledAnimations(selector(target));
    assert.equal((await colors(selector(target))).background, 'rgb(255, 240, 241)');
    await page.focus(selector(target)); await page.keyboard.press('Tab'); await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
    const focused = await colors(selector(target)); assert.equal(focused.focused, true); assert.equal(focused.outlineWidth, '3px'); assert.ok(focused.outline.startsWith('rgba(195, 59, 70,'));
    if (process.env.UI_SCREENSHOT_DIR) fs.mkdirSync(process.env.UI_SCREENSHOT_DIR, { recursive: true });
    for (const width of [1440, 390]) {
      await page.setViewport({ width, height: 1000 }); await noOverflow();
      await page.$eval(selector(actor), node => node.scrollIntoView({ block: 'center', inline: 'nearest' }));
      await page.$eval(selector(actor), node => { const wrap = node.closest('.adm-table-wrap'); wrap.scrollLeft = wrap.scrollWidth - wrap.clientWidth; });
      await page.waitForFunction(selector => { const wrap = document.querySelector(selector)?.closest('.adm-table-wrap'); return wrap && Math.abs(wrap.scrollLeft - (wrap.scrollWidth - wrap.clientWidth)) <= 0.5; }, {}, selector(actor));
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(resolve)));
      const visibility = await page.$eval('.adm-user-status-blocked', node => {
        const clip = { left: 0, top: 0, right: innerWidth, bottom: innerHeight };
        const clipping = new Set(['auto', 'scroll', 'hidden', 'clip', 'overlay']);
        for (let ancestor = node.parentElement; ancestor; ancestor = ancestor.parentElement) {
          const style = getComputedStyle(ancestor), rect = ancestor.getBoundingClientRect();
          const left = rect.left + ancestor.clientLeft, top = rect.top + ancestor.clientTop;
          if (clipping.has(style.overflowX)) { clip.left = Math.max(clip.left, left); clip.right = Math.min(clip.right, left + ancestor.clientWidth); }
          if (clipping.has(style.overflowY)) { clip.top = Math.max(clip.top, top); clip.bottom = Math.min(clip.bottom, top + ancestor.clientHeight); }
        }
        const range = document.createRange(); range.selectNodeContents(node);
        const fragments = [...range.getClientRects()].map(rect => ({ left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom, width: rect.width, height: rect.height }));
        const wrap = node.closest('.adm-table-wrap');
        return { text: node.textContent, clip, fragments, scrollLeft: wrap.scrollLeft, maxScroll: wrap.scrollWidth - wrap.clientWidth };
      });
      assert.equal(visibility.text, '不能停用当前登录的管理员。');
      assert.ok(Object.values(visibility.clip).every(Number.isFinite) && visibility.clip.right > visibility.clip.left && visibility.clip.bottom > visibility.clip.top);
      assert.ok(visibility.fragments.length > 0 && visibility.fragments.every(rect => Object.values(rect).every(Number.isFinite) && rect.width > 0 && rect.height > 0 && rect.left >= visibility.clip.left - 0.5 && rect.right <= visibility.clip.right + 0.5 && rect.top >= visibility.clip.top - 0.5 && rect.bottom <= visibility.clip.bottom + 0.5), `complete reason is clipped after maximum native scroll: ${JSON.stringify(visibility)}`);
      console.log(JSON.stringify({ case: 'user-status-reason-visibility', width, ...visibility }));
      if (process.env.UI_SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.UI_SCREENSHOT_DIR, `user-status-actions-${width}.png`) });
    }
    await page.setViewport({ width: 1440, height: 1000 });
    await page.click(selector(target)); await page.waitForSelector('[role=dialog]');
    assert.equal(await page.$eval('[role=dialog]', node => node.closest('.adm-restored-shell') === null), true, 'real body portal is outside the admin shell');
    let dialog = await page.$eval('[role=dialog]', node => node.textContent);
    assert.ok(dialog.includes('禁用用户') && dialog.includes('Status target') && dialog.includes(targetCode) && dialog.includes(target));
    assert.ok(!dialog.includes(otherCode) && !dialog.includes(other), 'confirmation does not confuse the other equal display name');
    assert.equal((await writes()).length, 0, 'opening a confirmation is not a write');
    const danger = await colors(confirm); assert.equal(danger.background, 'rgb(195, 59, 70)'); assert.equal(danger.color, 'rgb(255, 255, 255)');
    await page.keyboard.press('Tab'); const dialogFocus = await colors(confirm); assert.equal(dialogFocus.focused, true); assert.equal(dialogFocus.outlineWidth, '3px'); assert.ok(dialogFocus.outline.startsWith('rgba(195, 59, 70,'));
    await page.hover(confirm); assert.equal(await page.$eval(confirm, node => getComputedStyle(node).filter), 'brightness(0.95)');
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert.equal((await writes()).length, 0, 'cancel is zero-write');
    assert.equal(await page.evaluate(id => document.activeElement?.dataset.userStatusTarget === id, target), true, 'cancel restores opener focus');
    await page.click(selector(target)); await page.waitForSelector('[role=dialog]');
    if (process.env.UI_SCREENSHOT_DIR) {
      for (const width of [1440, 390]) {
        await page.setViewport({ width, height: 1000 }); await noOverflow();
        await page.screenshot({ path: path.join(process.env.UI_SCREENSHOT_DIR, `user-status-confirm-${width}.png`) });
      }
      await page.setViewport({ width: 1440, height: 1000 });
    }
    await page.evaluate(() => { window.restored.holdUserStatusWrite = true; });
    await page.click(confirm); await page.waitForFunction(() => window.restored.pendingUserStatusWrites.length === 1);
    await page.waitForFunction(() => document.querySelector('[data-user-status-confirm]')?.disabled && document.querySelector('[role=dialog] > header button')?.disabled);
    await page.click(confirm); await page.keyboard.press('Escape');
    assert.equal((await writes()).length, 1, 'busy native controls and mutation guard prevent a duplicate write');
    assert.ok(await page.$('[role=dialog]'), 'busy confirmation cannot close as if cancelled');
    await page.evaluate(() => window.restored.releaseUserStatusWrite());
    await page.waitForFunction(({ selector }) => !document.querySelector('[role=dialog]') && document.querySelector(selector)?.textContent === window.restored.copy().admin.accounts.enable && !document.querySelector(selector).disabled, {}, { selector: selector(target) });
    const first = (await writes())[0]; assert.equal(first.url, `/api/admin/account/admin/v1/users/${target}/status`); assert.deepEqual(JSON.parse(first.body), { status: 'DISABLED' });
    assert.notEqual((await colors(selector(target))).color, 'rgb(195, 59, 70)', 'enable trigger remains neutral');
    await page.click(selector(other)); await page.waitForSelector('[role=dialog]');
    dialog = await page.$eval('[role=dialog]', node => node.textContent);
    assert.ok(dialog.includes('启用用户') && dialog.includes(otherCode) && dialog.includes(other));
    assert.ok(!dialog.includes(targetCode) && !dialog.includes(target), 'equal display names keep distinct canonical targets');
    assert.equal(await page.$(`${confirm}.adm-user-disable-action`), null, 'enable confirmation is not a danger action');
    assert.notEqual((await colors(confirm)).background, 'rgb(195, 59, 70)');
    await page.evaluate(() => { window.restored.holdUserStatusWrite = false; });
    await page.click(confirm);
    await page.waitForFunction(selector => !document.querySelector('[role=dialog]') && document.querySelector(selector)?.textContent === window.restored.copy().admin.accounts.disable && !document.querySelector(selector).disabled, {}, selector(other));
    const second = (await writes())[1]; assert.equal(second.url, `/api/admin/account/admin/v1/users/${other}/status`); assert.deepEqual(JSON.parse(second.body), { status: 'ACTIVE' });
    await page.evaluate(() => { window.restored.permissions = ['platform.users.read']; window.restored.reload(); });
    await page.waitForFunction(() => !document.querySelector('[data-user-status-target]') && document.querySelectorAll('.adm-table tbody tr').length === 3);
    assert.equal(await page.$('[role=dialog]'), null); assert.equal((await writes()).length, 2, 'read-only role has no status action or extra write');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 2, 'only two explicitly confirmed status writes occurred');
  });
  await run('ledger points preserve exact precheck and frozen replay', async () => {
    const form = '.adm-financial-repair', amount = `${form} [data-ledger-amount]`;
    const precheckURL = '/api/admin/account/admin/v2/approval-command-prechecks';
    const draftURL = '/api/admin/approvals/requests';
    const copy = async () => (await page.evaluate(() => window.restored.copy())).ledger;
    const targetFields = ['target_resource_id', 'target_journal_entry_id', 'wrong_ledger_account_id', 'correct_ledger_account_id'];
    const fill = async (selector, value) => {
      const modifier = process.platform === 'darwin' ? 'Meta' : 'Control';
      await page.focus(selector); await page.keyboard.down(modifier); await page.keyboard.press('KeyA'); await page.keyboard.up(modifier); await page.keyboard.press('Backspace');
      assert.equal(await page.$eval(selector, node => node.value), '', 'keyboard replacement clears the complete prior input');
      if (value) await page.type(selector, value);
      assert.equal(await page.$eval(selector, node => node.value), value, 'actual input text equals the requested keyboard entry');
    };
    const read = async (expectedPoints, expectedMicrocredits) => {
      const ready = await page.$eval(`${form} .adm-financial-repair-target`, node => ({ value: node.querySelector('[data-ledger-amount]').value, invalid: node.querySelector('[data-ledger-amount]').getAttribute('aria-invalid'), buttonText: node.querySelector('button').textContent, disabled: node.querySelector('button').disabled, hint: node.ownerDocument.getElementById(node.querySelector('[data-ledger-amount]').getAttribute('aria-describedby').split(' ')[0]).textContent }));
      assert.equal(ready.value, expectedPoints);
      assert.equal(ready.invalid, null);
      assert.equal(ready.buttonText, (await copy()).ledgerWorkflow.load);
      assert.ok(ready.hint.includes((await copy()).amountPointsEquivalent.replace('{amount}', expectedMicrocredits)));
      assert.equal(ready.disabled, false, `valid read must be enabled: ${JSON.stringify(ready)}`);
      await click(`${form} .adm-financial-repair-target button`, (await copy()).ledgerWorkflow.load);
    };
    const calls = url => page.evaluate(url => window.restored.calls.filter(call => call.url === url && call.method === 'POST'), url);
    const settleRead = () => page.waitForFunction(selector => { const node = document.querySelector(selector); return node && !node.disabled; }, {}, amount);
    await fresh('?mode=admin&ledgerPoints=1&section=billing&finance_tab=ledger');
    await page.waitForSelector(amount);
    for (const language of ['zh', 'en', 'ja', 'zh']) {
      await page.evaluate(language => { window.restored.language = language; window.restored.reload(); }, language);
      await page.waitForFunction(({ selector, language }) => document.querySelector(selector)?.parentElement.textContent.includes(window.restored.copy().ledger.amountPointsLabel) && window.restored.language === language, {}, { selector: amount, language });
      assert.ok(await page.$eval(form, (node, hint) => node.textContent.includes(hint), (await copy()).amountPointsHint));
    }
    const ids = ['11111111-1111-4111-8111-111111111111', '22222222-2222-4222-8222-222222222222', '33333333-3333-4333-8333-333333333333', '44444444-4444-4444-8444-444444444444'];
    for (let index = 0; index < ids.length; index++) await fill(`${form} .adm-financial-repair-target input[name="${targetFields[index]}"]`, ids[index]);
    await fill(amount, '1.2345');
    await page.evaluate(() => { window.restored.ledgerPlan = { amount: '12345', deferred: false }; });
    await read('1.2345', '12345'); await page.waitForSelector(`${form} .adm-financial-repair-draft`);
    const first = (await calls(precheckURL)).at(-1);
    assert.match(first.body, /"amount_microcredits":12345[,}]/, 'business points reach actual precise microcredit wire');
    assert.equal(JSON.parse(first.body).target_resource_id, ids[0]);
    assert.ok((await page.$eval(form, node => node.textContent)).includes((await copy()).amountPointsEquivalent.replace('{amount}', '12345')));
    const beforeInvalid = (await calls(precheckURL)).length;
    for (const invalid of ['1.23456', '0', '-1', '1e2', '922337203685477.5808', '']) {
      await fill(amount, invalid); await page.keyboard.press('Tab');
      assert.equal(await page.$(`${form} .adm-financial-repair-draft`), null, 'editing invalidates the previously accepted projection');
      if (invalid) {
        assert.equal(await page.$eval(amount, node => node.getAttribute('aria-invalid')), 'true');
        assert.ok((await page.$eval(form, node => node.textContent)).includes((await copy()).amountPointsInvalid));
        assert.equal(await page.$eval(`${form} .adm-financial-repair-target button`, node => node.disabled), true);
      } else {
        await page.$eval(`${form} .adm-financial-repair-target`, node => node.requestSubmit());
      }
      assert.equal((await calls(precheckURL)).length, beforeInvalid, 'invalid current amount cannot dispatch the previous amount');
      assert.equal((await calls(draftURL)).length, 0);
    }
    await fill(amount, '1.2345');
    await page.evaluate(() => { window.restored.ledgerPlan = { amount: '12344', deferred: false }; });
    await read('1.2345', '12345'); await settleRead();
    assert.equal(await page.$(`${form} .adm-financial-repair-draft`), null, 'one microcredit mismatch is rejected');
    assert.ok((await page.$eval(form, node => node.textContent)).includes((await copy()).factsFailed));
    await page.evaluate(() => { window.restored.ledgerPlan = { amount: '12345', deferred: true }; });
    const heldClient = await page.evaluate(() => ({ index: window.restored.ledgerClients.length - 1, settled: window.restored.ledgerClients.at(-1).ledgerReadsSettled }));
    await read('1.2345', '12345'); await page.waitForFunction(() => window.restored.pendingLedgerReads.length === 1);
    assert.equal(await page.$$eval(`${form} .adm-financial-repair-target input`, nodes => nodes.every(node => node.disabled)), true, 'held read locks editable inputs');
    await page.evaluate(() => window.restored.reload());
    await settleRead();
    await page.evaluate(() => window.restored.releaseLedgerRead());
    await page.waitForFunction(({ index, settled }) => window.restored.ledgerClients[index].ledgerReadsSettled === settled + 1, {}, heldClient);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(resolve)));
    assert.equal(await page.$(`${form} .adm-financial-repair-draft`), null, 'retired client response cannot restore an accepted projection');
    // Refreshing the real parent can remount its children. Re-enter references.
    for (let index = 0; index < ids.length; index++) await fill(`${form} .adm-financial-repair-target input[name="${targetFields[index]}"]`, ids[index]);
    await fill(amount, '922337203685477.5807');
    await page.evaluate(() => { window.restored.ledgerPlan = { amount: '9223372036854775807', deferred: false }; });
    await read('922337203685477.5807', '9223372036854775807'); await page.waitForSelector(`${form} .adm-financial-repair-draft`);
    assert.match((await calls(precheckURL)).at(-1).body, /"amount_microcredits":9223372036854775807[,}]/);
    await fill(`${form} .adm-financial-repair-draft textarea`, 'Controlled ledger correction justification');
    await fill(`${form} .adm-financial-repair-draft input`, 'controlled-ledger-key');
    if (process.env.UI_SCREENSHOT_DIR) {
      fs.mkdirSync(process.env.UI_SCREENSHOT_DIR, { recursive: true });
      for (const width of [1440, 390]) {
        await page.setViewport({ width, height: 1000 }); await noOverflow();
        await page.$eval(form, node => node.scrollIntoView({ block: 'start' }));
        await page.screenshot({ path: path.join(process.env.UI_SCREENSHOT_DIR, `ledger-points-${width}.png`), fullPage: true });
      }
      await page.setViewport({ width: 1440, height: 1000 });
    }
    await click(`${form} .adm-financial-repair-draft button`, (await copy()).createDraft);
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes(window.restored.copy().ledger.recoveryPending), {}, form);
    const original = (await calls(draftURL))[0];
    assert.match(original.body, /"amount_microcredits":9223372036854775807[,}]/);
    assert.equal(JSON.parse(original.body).idempotency_key, 'approval:controlled-ledger-key');
    const proof = await page.evaluate(() => {
      const entry = Object.entries(document.defaultView.sessionStorage).find(([key]) => key.startsWith('pinvou.ledger-repair.intent.v1:'));
      if (!entry) throw new Error('Original ledger proof missing'); return entry;
    });
    assert.equal(JSON.parse(proof[1]).facts.projection.normalized_command.amount_microcredits, '9223372036854775807');
    await page.reload({ waitUntil: 'domcontentloaded', timeout: 60000 }); await page.waitForSelector(amount);
    await page.waitForFunction(selector => document.querySelector(selector)?.value === '922337203685477.5807' && document.querySelector(selector).disabled, {}, amount);
    assert.equal(await page.evaluate(key => document.defaultView.sessionStorage.getItem(key), proof[0]), proof[1], 'refresh retains exact original proof bytes');
    assert.equal((await calls(precheckURL)).length, 0, 'restoring unknown does not derive new facts');
    assert.equal((await calls(draftURL)).length, 0, 'refresh does not automatically repeat a write');
    await page.evaluate(() => { window.restored.loseLedgerDraft = false; window.restored.ledgerPlan.amount = '1'; });
    await click(`${form} .adm-financial-repair-draft button`, (await copy()).retry);
    await page.waitForFunction(() => !Object.keys(document.defaultView.sessionStorage).some(key => key.startsWith('pinvou.ledger-repair.intent.v1:')));
    const replay = (await calls(draftURL))[0];
    assert.equal(replay.body, original.body, 'explicit replay retains original amount, complete frozen facts and key');
    assert.equal((await calls(precheckURL)).length, 0, 'retry never re-prechecks with a different current amount');
    assert.equal(await page.evaluate(() => window.restored.effects.size), 1);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap') && call.url !== '/api/admin/approvals/requests').length), 0, 'no approval submit, ledger mutation or unrelated write');
  });
  await run('closure directory feedback separates pending empty error permission and retained recovery', async () => {
    const actor = '11111111-1111-4111-8111-111111111111', target = '22222222-2222-4222-8222-222222222222';
    const directory = '.adm-closure-directory';
    const row = { closure_request_id: target, status: 'REPAIR_REQUIRED', stage: 'FINANCIAL_CLEARANCE', blockers: [], sla_expires_at: '2030-01-01T00:00:00Z' };
    const plan = async (body = { items: [], next_cursor: null }, status = 200, deferred = false) => page.evaluate(({ body, status, deferred }) => { window.restored.closurePlan = { body, status, deferred }; }, { body, status, deferred });
    const settled = () => page.waitForFunction(selector => document.querySelector(selector)?.getAttribute('aria-busy') === 'false', {}, directory);
    const text = () => page.$eval(directory, node => node.textContent);
    const copy = () => page.evaluate(() => window.restored.copy());
    const refresh = async () => click(`${directory} > button`, (await copy()).finance.refresh);
    await fresh('?mode=admin&closureFeedback=1&section=operations&operations_page=closures');
    await page.waitForFunction(() => window.restored.pendingClosureReads?.length === 1);
    assert.ok((await text()).includes((await copy()).closure.listLoading), 'unresolved GET is visibly loading');
    assert.ok(!(await text()).includes((await copy()).closure.listEmpty), 'unresolved GET cannot be successful empty');
    await page.evaluate(() => window.restored.releaseClosureRead()); await settled();
    assert.ok((await text()).includes((await copy()).closure.listEmpty), 'confirmed empty page is explained');
    if (process.env.UI_SCREENSHOT_DIR) {
      fs.mkdirSync(process.env.UI_SCREENSHOT_DIR, { recursive: true });
      for (const width of [1440, 390]) {
        await page.setViewport({ width, height: 900 }); await noOverflow();
        await page.evaluate(async () => { await Promise.all(document.getAnimations().filter(animation => Number.isFinite(animation.effect?.getComputedTiming().endTime)).map(animation => animation.finished.catch(() => {}))); });
        await page.screenshot({ path: path.join(process.env.UI_SCREENSHOT_DIR, `closure-empty-${width}.png`), fullPage: true });
      }
      await page.setViewport({ width: 1440, height: 1000 });
    }
    for (const language of ['en', 'ja', 'zh']) {
      await plan();
      await page.evaluate(language => { window.restored.language = language; window.restored.reload(); }, language);
      await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes(window.restored.copy().closure.listEmpty), {}, directory);
      assert.equal(await page.$eval(`${directory} [role=status]`, node => node.textContent), (await copy()).closure.listEmpty);
    }
    await plan({ code: 'ACCOUNT_CLOSURE_UNAVAILABLE' }, 503); await refresh(); await settled();
    assert.ok((await text()).includes((await copy()).closure.listUnavailable));
    assert.ok(!(await text()).includes((await copy()).closure.listEmpty));
    assert.ok(!(await text()).includes((await copy()).finance.unavailable), 'directory error names its own context');
    await plan(); await click(`${directory} > [role=alert] button`, (await copy()).finance.retry); await settled();
    assert.ok((await text()).includes((await copy()).closure.listEmpty), 'explicit read retry recovers');
    await plan({ code: 'PERMISSION_DENIED' }, 403); await refresh(); await settled();
    assert.ok((await text()).includes((await copy()).closure.listPermissionDenied));
    assert.ok(!(await text()).includes((await copy()).closure.listEmpty));
    await plan({ items: [row], next_cursor: 'controlled-next' }); await refresh(); await settled();
    assert.ok((await text()).includes(target)); assert.ok(!(await text()).includes((await copy()).closure.listEmpty));
    await plan(); await click(`${directory} > button`, (await copy()).finance.next); await settled();
    assert.ok((await text()).includes((await copy()).closure.listEmpty));
    assert.equal(await page.evaluate(() => new URL(window.restored.calls.filter(call => call.method === 'GET' && call.url.includes('/account-closure-requests')).at(-1).url, location.origin).searchParams.get('cursor')), 'controlled-next');
    const proof = await page.evaluate(() => window.restored.seedClosureRecovery());
    await plan(undefined, 200, true); await refresh();
    await page.waitForFunction(() => window.restored.pendingClosureReads.length === 1);
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes(window.restored.copy().closure.recoveryPending), {}, directory);
    assert.ok((await text()).includes((await copy()).closure.listLoading));
    assert.ok(!(await text()).includes((await copy()).closure.listEmpty));
    await page.evaluate(() => window.restored.releaseClosureRead()); await settled();
    assert.ok((await text()).includes(target)); assert.ok(!(await text()).includes((await copy()).closure.listEmpty));
    assert.equal(await page.$$eval(`${directory} tr[data-closure-source="server"]`, nodes => nodes.length), 0, 'the confirmed empty result stays separate from local recovery');
    assert.equal(await page.$$eval(`${directory} tr[data-closure-source="local"]`, nodes => nodes.length), 1, 'the original local recovery remains visible');
    assert.ok(await page.$(`${directory} [data-closure-empty] [role=status]`), 'successful server empty has a separate state');
    await page.waitForFunction(selector => { const textarea = document.querySelector(`${selector} textarea`); return textarea?.disabled && textarea.value === 'Controlled original recovery'; }, {}, directory);
    assert.deepEqual(await page.evaluate(() => window.restored.readClosureRecovery()), proof);
    await plan({ code: 'ACCOUNT_CLOSURE_UNAVAILABLE' }, 503); await refresh(); await settled();
    assert.ok((await text()).includes((await copy()).closure.listUnavailable) && (await text()).includes((await copy()).closure.recoveryPending));
    assert.deepEqual(await page.evaluate(() => window.restored.readClosureRecovery()), proof, 'read failure retains exact original facts/key');
    const stored = await page.evaluate(proof => {
      const entry = Object.entries(document.defaultView.sessionStorage).find(([, value]) => value === JSON.stringify(proof));
      if (!entry) throw new Error('Original proof storage entry missing');
      document.defaultView.sessionStorage.setItem(entry[0], '{'); return entry;
    }, proof);
    const beforeCorrupt = await page.evaluate(() => window.restored.calls.filter(call => call.url.includes('/account-closure-requests')).length);
    await refresh(); await settled();
    assert.ok((await text()).includes((await copy()).closure.recoveryUnavailable) && !(await text()).includes((await copy()).closure.listEmpty));
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.url.includes('/account-closure-requests')).length), beforeCorrupt, 'unsafe recovery read stops before GET');
    assert.equal(await page.evaluate(key => document.defaultView.sessionStorage.getItem(key), stored[0]), '{', 'failed recovery read never deletes evidence');
    await page.evaluate(([key, value]) => document.defaultView.sessionStorage.setItem(key, value), stored);
    await plan({ items: [row], next_cursor: null }); await refresh(); await settled();
    assert.equal(await page.$$eval(`${directory} tr[data-closure-id]`, nodes => nodes.length), 1, 'server duplicate and local intent share one action');
    assert.deepEqual(await page.evaluate(() => window.restored.readClosureRecovery()), proof);
    await plan({ items: [row], next_cursor: null }, 200, true); await refresh();
    await page.waitForFunction(() => window.restored.pendingClosureReads.length === 1);
    const oldClient = await page.evaluate(() => window.restored.closureClients.length - 1);
    await plan();
    await page.evaluate(() => { window.restored.user = '33333333-3333-4333-8333-333333333333'; window.restored.reload(); });
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes(window.restored.copy().closure.listEmpty), {}, directory);
    await page.evaluate(() => window.restored.releaseClosureRead());
    await page.waitForFunction(index => window.restored.closureClients[index].inflightReads.size === 0, {}, oldClient);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(resolve)));
    assert.ok(!(await text()).includes(target), 'retired actor result/recovery cannot enter current directory');
    assert.deepEqual(await page.evaluate(actor => window.restored.readClosureRecovery(actor), actor), proof);
    const beforeDenied = await page.evaluate(() => window.restored.calls.filter(call => call.url.includes('/account-closure-requests')).length);
    await page.evaluate(() => { window.restored.permissions = ['platform.registration-bonus.read']; window.restored.reload(); });
    await page.waitForFunction(() => [...document.querySelectorAll('[role=alert]')].some(node => node.textContent === window.restored.copy().closure.listPermissionDenied));
    assert.equal(await page.$(directory), null, 'real parent permission gate does not mount unauthorized directory');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.url.includes('/account-closure-requests')).length), beforeDenied, 'no unauthorized list GET');
    await page.setViewport({ width: 390, height: 900 }); await noOverflow();
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0, 'all feedback/retry paths are read-only');
    if (process.env.UI_SCREENSHOT_DIR) { fs.mkdirSync(process.env.UI_SCREENSHOT_DIR, { recursive: true }); await page.screenshot({ path: path.join(process.env.UI_SCREENSHOT_DIR, 'closure-permission-390.png'), fullPage: true }); }
  });
  await run('audit long operations stay inside their cells and retain canonical detail targets', async () => {
    errors.length = 0;
    await page.setViewport({ width: 1440, height: 1000 });
    await page.goto(`${origin}/tests/account_platform_audit_fixture.html?section=audit&readOnly=1&longAction=1`, { waitUntil: 'domcontentloaded', timeout: 60000 });
    await page.waitForSelector('.adm-table tbody tr:nth-child(3)');
    const expected = ['APPROVAL_EXISTING_BROKER_INITIALIZATION', `UNKNOWN_${'X'.repeat(160)}`, 'user.active_context.set'];
    const displayed = await page.evaluate(codes => codes.map(code => window.f17.auditLabels[code] || code), expected);
    const measure = async () => {
      const rows = await page.$$eval('.adm-table tbody tr', rows => rows.map(row => {
        const cell = row.cells[0], action = cell.querySelector('.adm-audit-action'), box = cell.getBoundingClientRect();
        const range = document.createRange(); range.selectNodeContents(action);
        const fragments = [...range.getClientRects()], actionBox = action.getBoundingClientRect();
        const lineTops = [];
        for (const fragment of fragments) if (!lineTops.some(top => Math.abs(top - fragment.top) <= 0.5)) lineTops.push(fragment.top);
        const style = getComputedStyle(cell), contentWidth = box.width - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight) - parseFloat(style.borderLeftWidth) - parseFloat(style.borderRightWidth);
        const valid = rect => [rect.left, rect.top, rect.right, rect.bottom, rect.width, rect.height].every(Number.isFinite) && rect.width > 0 && rect.height > 0;
        const contained = rect => valid(rect) && rect.left >= box.left - 0.5 && rect.right <= box.right + 0.5 && rect.top >= box.top - 0.5 && rect.bottom <= box.bottom + 0.5;
        return { text: action.textContent, lines: lineTops.length, cellWidth: box.width, contentWidth, validCell: valid(box) && Number.isFinite(contentWidth) && contentWidth > 0, actionFits: contained(actionBox), textFits: fragments.length > 0 && fragments.every(contained), beforeResource: fragments.every(rect => rect.right <= row.cells[1].getBoundingClientRect().left + 0.5) };
      }));
      assert.deepEqual(rows.map(row => row.text), displayed, 'known operations use their localized label; unknown operation codes remain complete');
      assert.ok(rows.every(row => row.validCell && row.actionFits && row.textFits && row.beforeResource), `audit text stays within its own cell: ${JSON.stringify(rows)}`);
      assert.ok(rows.every((row, index) => row.lines <= [2, 8, 1][index]), `fixed audit samples remain readable without text pillars: ${JSON.stringify(rows)}`);
      console.log('AUDIT_LAYOUT', JSON.stringify({ viewport: await page.evaluate(() => innerWidth), rows }));
    };
    for (const width of [1440, 390]) {
      await page.setViewport({ width, height: 1000 }); await measure(); await noOverflow();
      if (width === 390) {
        assert.equal(await page.$eval('.adm-table-wrap', node => node.scrollWidth > node.clientWidth), true, 'narrow table keeps its internal horizontal scroll');
        await page.$eval('.adm-table-wrap', node => { node.scrollLeft = 120; }); await measure();
        await page.$eval('.adm-table-wrap', node => { node.scrollLeft = 0; });
      }
      if (process.env.UI_ARTIFACT_DIR) {
        fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `admin-audit-actions-${width}.png`), fullPage: true });
      }
      for (const [index, suffix] of [[0, '17'], [1, '18']]) {
        const action = await page.$(`.adm-table tbody tr:nth-child(${index + 1}) button.adm-audit-row-detail`);
        if (index === 0) await action.click();
        else { await action.focus(); await page.keyboard.press('Enter'); }
        await action.dispose();
        await page.waitForSelector('.adm-detail-drawer');
        assert.equal(await page.evaluate(({ action, suffix }) => {
          const reads = window.f17.calls.filter(call => /\/audit-logs\/[^/?]+$/.test(call.url));
          return reads.at(-1)?.method === 'GET' && reads.at(-1).url.endsWith(`/audit-logs/00000000-0000-4000-8000-0000000000${suffix}`) && [...document.querySelectorAll('.adm-audit-action-code code')].some(node => node.textContent === action);
        }, { action: expected[index], suffix }), true, 'pointer and keyboard open the original canonical audit record');
        await page.keyboard.press('Escape'); await page.waitForSelector('.adm-table tbody tr:nth-child(3)');
      }
      assert.equal(await page.evaluate(() => window.f17.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0, 'reading audit details creates no business mutation');
    }
  });
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
    await page.click(history + ' tbody tr:first-child td:last-child button');
    await page.waitForFunction(() => document.querySelector('[role=dialog] .ap-order-details-body')?.textContent.includes('9.90'));
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
    await click('[data-org-action=remove-member] button[type=submit]', await page.evaluate(() => window.restored.copy().organization.retry));
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
    await click(`${form} button[type=submit]`, await page.evaluate(() => window.restored.copy().organization.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.url.endsWith('/quota-grants:batch')).length === 2);
    const calls = await page.evaluate(() => window.restored.calls.filter(call => call.url.endsWith('/quota-grants:batch')));
    assert.equal(calls[0].key, calls[1].key); assert.equal(calls[0].body, calls[1].body);
    const item = JSON.parse(calls[0].body).items[0];
    assert.equal(item.amount_microcredits, 12345); assert.equal(item.component_type, 'BASE'); assert.equal(item.membership_version, 1);
    await page.waitForFunction(() => document.querySelector('[data-org-action=quota-allocation]')?.textContent.includes(window.restored.copy().center.noSources));
    assert.equal(await page.$(`${form} button[type=submit]`), null, 'resolved empty allocation has no executable submit control');
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
    await page.waitForFunction(() => [...document.querySelectorAll('[data-org-action=quota-allocation] button[type=submit]')].some(node => node.textContent === window.restored.copy().organization.retry && !node.disabled));
    await page.evaluate(() => { window.restored.unknown = false; window.restored.membershipVersion = 2; window.restored.replayVersion = 2; });
    await click(`${form} button[type=submit]`, await page.evaluate(() => window.restored.copy().organization.retry));
    await page.waitForFunction(() => document.querySelector('[data-org-action=quota-allocation] fieldset')?.disabled === false && document.querySelector('[data-org-action=quota-allocation]')?.textContent.includes(window.restored.copy().organization.saved));
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
    await click('[data-workspace=users] button', await page.evaluate(() => window.restored.copy().adminLayout.backToUserList));
    await page.waitForSelector('[data-workspace=users] tbody tr');
    assert.equal(new URL(page.url()).searchParams.get('user_q'), 'sample');
  });
  await run('organization detail has four original URL sections, server member search and directory return', async () => {
    await fresh('?mode=admin&section=organizations&organization_q=sample');
    await page.waitForSelector('.adm-organization-directory .adm-name-link');
    assert.deepEqual(await page.$eval('.adm-organization-directory .adm-name-link', node => {
      const target = new URL(node.href);
      return { tag: node.tagName, pathname: target.pathname, section: target.searchParams.get('section'), query: target.searchParams.get('organization_q'), idMatchesRow: target.searchParams.get('organization_id') === node.closest('tr').querySelector('.adm-identifier code').textContent, tab: target.searchParams.get('organization_tab'), memberCursor: target.searchParams.get('member_cursor') };
    }), { tag: 'A', pathname: new URL(page.url()).pathname, section: 'organizations', query: 'sample', idMatchesRow: true, tab: 'overview', memberCursor: null });
    await page.click('.adm-organization-directory .adm-name-link');
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
  await run('organization member removal has a named action and cancellable target dialog in all locales', async () => {
    for (const language of ['zh', 'en', 'ja']) for (const width of [1440, 390]) {
      await fresh('?mode=admin&section=organizations&organization_id=33333333-3333-4333-8333-333333333333&organization_tab=members');
      await page.evaluate(empty => { window.restored.noUnitMembers = empty; window.restored.reload(); }, width === 390);
      await page.setViewport({ width, height: 1000 });
      await page.select('.adm-sidebar-foot select', language);
      const words = await page.evaluate(() => window.restored.copy().organization);
      await page.waitForFunction(label => [...document.querySelectorAll('.adm-member-action-menu button')].some(button => button.textContent.trim() === label && !button.disabled), {}, words.remove);
      const toolbar = await page.$eval('[data-workspace=members] .adm-member-toolbar', node => {
        const add = node.querySelector(':scope>button'), table = node.parentElement.querySelector('table');
        const [name, role] = [...node.querySelectorAll('form>label')].map(item => item.getBoundingClientRect());
        const query = node.querySelector('form>button').getBoundingClientRect(), bounds = node.getBoundingClientRect();
        return { beforeTable: Boolean(add.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING), addBottom: add.getBoundingClientRect().bottom, tableTop: table.getBoundingClientRect().top, nameRoleGap: role.left - name.right, roleQueryGap: query.left - role.right, controlsFit: [...node.querySelectorAll('input,select,button')].every(item => { const box = item.getBoundingClientRect(); return box.left >= bounds.left - 1 && box.right <= bounds.right + 1; }) };
      });
      assert.ok(toolbar.beforeTable && toolbar.addBottom + 8 <= toolbar.tableTop && toolbar.controlsFit, `member toolbar precedes the table without control overflow: ${JSON.stringify({language,width,toolbar})}`);
      if (width === 1440) assert.ok(toolbar.nameRoleGap >= 8 && toolbar.nameRoleGap <= 16 && toolbar.roleQueryGap >= 8 && toolbar.roleQueryGap <= 16, 'name, role and search remain adjacent');
      const expectedUnits = width === 390 ? words.unassignedUnits : 'Sample unit';
      assert.equal(await page.$eval('[data-workspace=members] tbody tr td:nth-child(3)', node => node.textContent.trim()), expectedUnits, 'member rows show assignment data rather than a repeated rule');
      assert.deepEqual(await page.$$eval('[data-workspace=members] th>.ap-member-units-help', nodes => nodes.map(node => node.textContent)), [words.unscopedUnits], 'scope rule is stated once in the column header');
      assert.equal(await page.$eval('[data-workspace=members] tbody', (node, rule) => node.textContent.includes(rule), words.unscopedUnits), false);
      await page.$eval('[data-workspace=members] details summary', node => node.scrollIntoView({ block: 'center', inline: 'nearest' }));
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(resolve)));
      const height = await page.$eval('[data-workspace=members] tbody tr', row => row.getBoundingClientRect().height);
      await page.click('[data-workspace=members] details summary');
      await page.waitForFunction(() => {
        const menu = document.querySelector('.adm-member-action-menu');
        return menu && getComputedStyle(menu).visibility !== 'hidden' && menu.getClientRects().length;
      });
      assertStableLayoutHeight(height, await page.$eval('[data-workspace=members] tbody tr', row => row.getBoundingClientRect().height), 'opening member actions does not grow its table row');
      const geometry = await page.$eval('.adm-member-action-menu', menu => { const box = menu.getBoundingClientRect(); return { left: box.left, right: box.right, top: box.top, bottom: box.bottom, viewportWidth: innerWidth, viewportHeight: innerHeight, position: getComputedStyle(menu).position, pageTransform: getComputedStyle(document.querySelector('.adm-page')).transform }; });
      assert.ok(geometry.left >= 7 && geometry.right <= geometry.viewportWidth - 7 && geometry.top >= 7 && geometry.bottom <= geometry.viewportHeight - 7, `action overlay fits the viewport: ${JSON.stringify({ language, width, geometry })}`);
      assert.equal(await page.$eval('.adm-member-action-menu button', node => node === document.activeElement), true, 'opening the portal moves focus into its actions');
      assert.equal(await page.$eval('.adm-member-action-menu button:last-child', node => getComputedStyle(node).color), 'rgb(195, 59, 70)', 'member removal has danger styling');
      await page.keyboard.press('Tab'); await page.keyboard.press('Tab'); await page.keyboard.press('Tab');
      await page.waitForFunction(() => !document.querySelector('[data-workspace=members] details')?.open);
      assert.equal(await page.$eval('[data-workspace=members] details summary', node => node === document.activeElement), true, 'when the summary is the last page control, Tab closes its menu and returns to it');
      await page.keyboard.press('Tab');
      assert.equal(await page.$eval('[data-workspace=members] details summary', node => node === document.activeElement), false, 'the next Tab leaves the last summary rather than trapping focus');
      await page.$eval('[data-workspace=members] details summary', node => node.focus({ preventScroll: true }));
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => document.querySelector('.adm-member-action-menu button') === document.activeElement);
      await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
      await page.waitForFunction(() => !document.querySelector('[data-workspace=members] details')?.open);
      assert.equal(await page.$eval('[data-workspace=members] details summary', node => node === document.activeElement), true, 'Shift-Tab from the first action returns to the summary');
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => document.querySelector('.adm-member-action-menu button') === document.activeElement);
      if (process.env.UI_ARTIFACT_DIR && language === 'zh') {
        fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `admin-member-menu-${width}.png`), fullPage: false });
      }
      await click('.adm-member-action-menu button', words.remove);
      await page.waitForSelector('[role=dialog] .ap-org-action-target');
      assert.equal(await page.$eval('[role=dialog] h2', node => node.textContent), words.remove);
      const target = await page.$eval('[role=dialog] .ap-org-action-target', node => node.textContent);
      assert.ok(target.includes('Sample member') && target.includes('22222222-2222-4222-8222-222222222222'));
      const resizingDialog = language === 'zh' && width === 1440;
      if (resizingDialog) {
        await page.setViewport({ width, height: 500 });
        await page.waitForFunction(() => {
          const box = document.querySelector('.adm-member-action-menu').getBoundingClientRect();
          return box.top >= 7 && box.bottom <= innerHeight - 7;
        });
        assert.equal(await page.$eval('[role=dialog]', node => node.contains(document.activeElement)), true, 'resizing preserves the active dialog and its focus');
      }
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
      if (resizingDialog) {
        const bounds = await page.$eval('.adm-member-action-menu', node => { const box = node.getBoundingClientRect(); return { top: box.top, bottom: box.bottom, viewport: innerHeight, open: node.closest('.adm-member-menu-portal').dataset.open, focused: node.contains(document.activeElement) }; });
        assert.ok(bounds.top >= 7 && bounds.bottom <= bounds.viewport - 7 && bounds.open === 'true' && bounds.focused, `cancel returns focus to the visible menu after resize: ${JSON.stringify(bounds)}`);
      }
      assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => !document.querySelector('[data-workspace=members] details')?.open);
      assert.equal(await page.$eval('[data-workspace=members] details summary', node => node === document.activeElement), true);
      await click('[data-workspace=members] .adm-member-toolbar>button', words.addMember);
      await page.waitForSelector('[role=dialog] .ap-org-action-target');
      assert.equal(await page.$eval('[role=dialog] h2', node => node.textContent), words.addMember);
      await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
      assert.equal(await page.$eval('[data-workspace=members] .adm-member-toolbar>button', node => node === document.activeElement), true, 'cancelling returns focus to the toolbar action');
      assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
    }
  });
  await run('organization rename footer cancellation discards edits and reopens the current name', async () => {
    for (const kind of ['organization', 'unit']) for (const language of ['zh', 'en', 'ja']) {
      await fresh(`?mode=admin&section=organizations&organization_id=33333333-3333-4333-8333-333333333333&organization_tab=${kind === 'unit' ? 'units' : 'overview'}`);
      await page.select('.adm-sidebar-foot select', language);
      const words = await page.evaluate(() => window.restored.copy().organization);
      const label = kind === 'unit' ? words.renameUnit : words.rename;
      const initial = kind === 'unit' ? 'Sample unit' : 'Sample organization';
      await page.waitForFunction(label => [...document.querySelectorAll('.adm-organization-directory button')].some(node => node.textContent.trim() === label && !node.disabled), {}, label);
      await click('.adm-organization-directory button', label);
      await page.waitForSelector('[role=dialog] input');
      assert.equal(await page.$eval('[role=dialog] input', node => node.value), initial);
      assert.equal(await page.$eval('[role=dialog] .ap-org-action-footer>button[type=submit]', node => node.textContent), words.saveChanges);
      assert.equal(await page.$eval('[role=dialog] .ap-org-action-footer>button[type=submit]', node => getComputedStyle(node).backgroundColor), 'rgb(49, 89, 232)');
      assert.equal(await page.$eval('[role=dialog] .ap-org-action-footer>button[type=button]', node => getComputedStyle(node).backgroundColor), 'rgb(255, 255, 255)');
      await page.type('[role=dialog] input', ' changed');
      await click('[role=dialog] .ap-org-action-footer>button', words.cancelConfirmation);
      await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
      assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
      await click('.adm-organization-directory button', label);
      await page.waitForSelector('[role=dialog] input');
      assert.equal(await page.$eval('[role=dialog] input', node => node.value), initial, 'reopening starts from the current server name');
      await page.keyboard.press('Escape');
    }
  });
  await run('organization fieldless dialogs identify targets before confirmation and retain unknown writes', async () => {
    const orgID = '33333333-3333-4333-8333-333333333333', unitID = '44444444-4444-4444-8444-444444444444';
    for (const language of ['zh', 'en', 'ja']) for (const operation of ['deleteUnit', 'disable', 'enable']) {
      const tab = operation === 'deleteUnit' ? 'units' : 'lifecycle';
      await fresh(`?mode=admin&section=organizations&organization_id=${orgID}&organization_tab=${tab}`);
      await page.evaluate(({ language, operation }) => {
        window.restored.language = language;
        window.restored.orgStatus = operation === 'enable' ? 'DISABLED' : 'ACTIVE';
        window.restored.noUnitMembers = operation === 'deleteUnit';
        window.restored.reload();
      }, { language, operation });
      const words = await page.evaluate(() => window.restored.copy().organization);
      await page.waitForFunction(({ title }) => [...document.querySelectorAll('.adm-organization-directory button')].some(node => node.textContent.trim() === title && node.getClientRects().length), {}, { title: words[operation] });
      const open = () => click('.adm-organization-directory button', words[operation]);
      const writes = () => page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')));
      await open(); await page.waitForSelector('[role=dialog] .ap-org-action-target');
      const target = await page.$eval('[role=dialog] .ap-org-action-target', node => node.textContent);
      assert.ok(target.includes('Sample organization') && target.includes(orgID), 'first dialog identifies the organization');
      if (operation === 'deleteUnit') assert.ok(target.includes('Sample unit') && target.includes(unitID), 'unit deletion identifies both parent and unit');
      assert.equal(await page.$$eval('[role=dialog] fieldset', nodes => nodes.length), 0, 'fieldless actions must not show an empty fieldset');
      const description = words[operation === 'deleteUnit' ? 'deleteUnitDescription' : operation === 'disable' ? 'disableOrganizationDescription' : 'enableOrganizationDescription'];
      const precheckInfo = operation === 'disable' ? await page.evaluate(() => window.restored.copy().adminLayout.organization.lifecycleTasks.disablePrecheckInfo) : '';
      const initialDescription = [description, precheckInfo].filter(Boolean).join(' ');
      assert.equal(await page.$eval('[role=dialog] .ap-org-action-description', node => node.textContent), initialDescription);
      assert.deepEqual(await page.$$eval('[role=dialog] .ap-org-action-footer>button', nodes => nodes.map(node => node.textContent)), [words.cancelConfirmation, words[operation]]);
      const actionColor = operation === 'enable' ? 'rgb(49, 89, 232)' : 'rgb(195, 59, 70)';
      assert.equal(await page.$eval('[role=dialog] .ap-org-action-footer>button[type=submit]', node => getComputedStyle(node).backgroundColor), actionColor);
      assert.equal(await page.$eval('[role=dialog] .ap-org-action-footer>button[type=button]', node => getComputedStyle(node).backgroundColor), 'rgb(255, 255, 255)');
      if (process.env.UI_ARTIFACT_DIR && language === 'zh') {
        fs.mkdirSync(process.env.UI_ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.UI_ARTIFACT_DIR, `admin-${operation}-target-dialog.png`), fullPage: true });
      }
      await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
      assert.equal((await writes()).length, 0, 'opening and cancelling never mutates');
      await open(); await page.waitForSelector('[role=dialog] .ap-org-action-footer');
      await click('[role=dialog] .ap-org-action-footer>button', words.cancelConfirmation);
      await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
      assert.equal((await writes()).length, 0, 'explicit footer cancellation never mutates');
      await open(); await page.click('[role=dialog] .ap-org-action-footer>button[type=submit]');
      await page.waitForSelector('[role=alertdialog]');
      const confirmLabel = await page.evaluate(operation => { const copy = window.restored.copy().organization; return copy.confirmActionTemplate.replace('{action}', () => copy[operation]); }, operation);
      assert.ok(confirmLabel.includes(words[operation]), 'final action label identifies the operation');
      assert.equal(await page.$eval('[role=alertdialog] footer>button:last-child', node => node.textContent), confirmLabel);
      assert.equal(await page.$eval('[role=alertdialog]', node => getComputedStyle(node).fontSize), '14px', 'body-portalled confirmation uses the standard dialog font');
      assert.equal(await page.$eval('[role=alertdialog] footer>button:last-child', node => getComputedStyle(node).backgroundColor), actionColor);
      assert.equal(await page.$eval('[role=alertdialog] footer>button:first-child', node => getComputedStyle(node).backgroundColor), 'rgb(255, 255, 255)');
      const finalText = await page.$eval('[role=alertdialog]', node => node.textContent);
      if (operation === 'deleteUnit') {
        const impact = await page.evaluate(() => window.restored.copy().adminLayout.organization.unitDeleteImpact);
        assert.ok(finalText.includes(impact.confirmRequest), 'unit deletion confirms a request with server-side resource checks');
        const facts = await page.$eval('[role=alertdialog] .ap-admin-unit-delete-facts', node => node.textContent);
        assert.ok(facts.includes(impact.children) && facts.includes(impact.scope), 'unit deletion retains the direct-child facts and deletion scope');
        const finalTarget = await page.$eval('[role=alertdialog] dd', node => node.textContent);
        assert.ok(finalTarget.includes(orgID) && finalTarget.includes(unitID), 'unit deletion identifies both canonical targets at final confirmation');
      } else assert.ok(finalText.includes(description), 'final confirmation retains impact explanation');
      assert.ok(finalText.includes(description), 'final confirmation retains the original impact explanation alongside the request facts');
      assert.ok((await page.$eval('[role=alertdialog] dd', node => node.textContent)).includes('Sample organization'));
      assert.equal((await writes()).length, 0, 'first submit still requires confirmation');
      await page.keyboard.press('Escape'); await page.waitForSelector('[role=dialog] .ap-org-action-target');
      assert.equal((await writes()).length, 0, 'cancelling final confirmation never mutates');
      await page.click('[role=dialog] .ap-org-action-footer>button[type=submit]'); await page.waitForSelector('[role=alertdialog]');
      await page.evaluate(() => { window.restored.unknownOrgAction = true; });
      await click('[role=alertdialog] button', confirmLabel);
      await page.waitForFunction(retry => [...document.querySelectorAll('[role=dialog] .ap-org-action-footer>button')].some(node => node.textContent.trim() === retry && !node.disabled), {}, words.retry);
      const original = await writes(); assert.equal(original.length, 1);
      assert.equal(typeof original[0].key, 'string');
      assert.ok(original[0].key.trim().length > 0, 'accepted write carries an idempotency key');
      assert.equal(original[0].method, operation === 'deleteUnit' ? 'DELETE' : 'PATCH');
      assert.equal(original[0].url, `/api/admin/account/admin/v1/organizations/${orgID}${operation === 'deleteUnit' ? `/units/${unitID}` : '/status'}`);
      assert.deepEqual(JSON.parse(original[0].body), operation === 'deleteUnit' ? {} : { status: operation === 'disable' ? 'DISABLED' : 'ACTIVE' });
      await page.keyboard.press('Escape'); assert.ok(await page.$('[role=dialog]'), 'unknown result stays recoverable');
      assert.equal(await page.$eval('[role=dialog] .ap-org-action-footer>button[type=button]', button => button.disabled), true, 'accepted unknown writes cannot be cancelled');
      await click('[role=dialog] .ap-org-action-footer>button', words.retry);
      await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
      const replay = await writes(); assert.equal(replay.length, 2);
      assert.equal(replay[0].key, replay[1].key); assert.equal(replay[0].body, replay[1].body);
      assert.equal(await page.evaluate(() => window.restored.effects.size), 1, 'controlled service retains one request identity');
      if (operation !== 'deleteUnit') {
        const next = operation === 'disable' ? words.enable : words.disable;
        await page.waitForFunction(label => [...document.querySelectorAll('.adm-organization-directory button')].some(node => node.textContent.trim() === label && !node.disabled), {}, next);
        await click('.adm-organization-directory button', next);
        await page.waitForSelector('[role=dialog] .ap-org-action-footer');
        await page.click('[role=dialog] .ap-org-action-footer>button[type=submit]');
        await page.waitForSelector('[role=alertdialog]');
        await click('[role=alertdialog] button', await page.evaluate(title => window.restored.copy().organization.confirmActionTemplate.replace('{action}', () => title), next));
        await page.waitForFunction(() => !document.querySelector('[role=dialog], [role=alertdialog]'));
        const subsequent = await writes(); assert.equal(subsequent.length, 3);
        assert.equal(typeof subsequent[2].key, 'string'); assert.notEqual(subsequent[2].key, original[0].key);
        assert.deepEqual(JSON.parse(subsequent[2].body), { status: operation === 'disable' ? 'ACTIVE' : 'DISABLED' });
      }
    }
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
    await page.click('[role=dialog] .ap-org-action-footer>button[type=submit]');
    await page.waitForSelector('[role=dialog] form [role=alert]');
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.method === 'DELETE')));
    await fields[0].focus(); await page.keyboard.down('Control'); await page.keyboard.press('A'); await page.keyboard.up('Control'); await page.keyboard.press('Backspace'); await fields[0].type('Sample organization');
    assert.deepEqual(await page.$$eval('[role=dialog] fieldset input', nodes => nodes.map(node => node.value)), ['Sample organization', 'Controlled deletion reason']);
    await page.click('[role=dialog] .ap-org-action-footer>button[type=submit]');
    await page.waitForSelector('[role=alertdialog]');
    await page.keyboard.press('Escape');
    await page.waitForSelector('[role=dialog] form.ap-org-action');
    assert.ok(await page.evaluate(() => !window.restored.calls.some(call => call.method === 'DELETE')));
    await page.keyboard.press('Escape');
  });
  await run('organization creation searches users and keeps the original unknown invocation in its modal', async () => {
    await fresh('?mode=admin&section=organizations');
    await page.evaluate(() => { window.restored.unknownOrgCreate = true; });
    assert.equal(await page.$eval('.adm-organization-directory .ap-admin-org-primary-trigger', node => getComputedStyle(node).backgroundColor), 'rgb(49, 89, 232)', 'create uses the primary action style');
    await click('.adm-organization-directory button', await page.evaluate(() => window.restored.copy().organization.create));
    await page.waitForSelector('[role=dialog] .adm-user-picker');
    await page.type('[role=dialog] fieldset>label:first-child input', 'Controlled organization');
    await page.type('.adm-user-picker input', 'remote');
    await click('.adm-user-picker button', await page.evaluate(() => window.restored.copy().organization.search));
    await page.waitForFunction(() => document.querySelector('.adm-user-picker select option[value="22222222-2222-4222-8222-222222222222"]'));
    await page.select('.adm-user-picker select', '22222222-2222-4222-8222-222222222222');
    await page.click('[role=dialog] .ap-org-action-footer>button[type=submit]');
    await page.waitForFunction(() => window.restored.calls.some(call => call.method === 'POST' && call.url.endsWith('/admin/v1/organizations')) && [...document.querySelectorAll('[role=dialog] .ap-org-action-footer>button')].some(node => node.textContent.trim() === window.restored.copy().organization.retry && !node.disabled));
    await page.keyboard.press('Escape'); assert.ok(await page.$('[role=dialog]'));
    await page.evaluate(() => document.querySelector('.adm-sidebar [data-section=users]').click());
    assert.equal(new URL(page.url()).searchParams.get('section'), 'organizations');
    await click('[role=dialog] .ap-org-action-footer>button', await page.evaluate(() => window.restored.copy().organization.retry));
    await page.waitForFunction(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/admin/v1/organizations')).length === 2);
    const values = await page.evaluate(() => ({ calls: window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/admin/v1/organizations')), effects: window.restored.effects.size }));
    assert.equal(values.calls[0].key, values.calls[1].key); assert.equal(values.calls[0].body, values.calls[1].body); assert.equal(values.effects, 1);
  });
  await run('financial read lists retain composite refund identity, filters and exact amounts without writes', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=refunds');
    await page.waitForSelector('[data-workspace=refunds] tbody tr');
    assert.equal(await page.$$eval('[data-workspace=refunds] tbody tr', nodes => nodes.length), 2);
    assert.ok((await page.$eval('[data-workspace=refunds]', node => node.textContent)).includes('90,071,992,547,409.93'));
    const details = await page.$$('[data-workspace=refunds] tbody tr > td:last-child > button'); assert.equal(details.length, 2); await details[1].click();
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
  await run('admin order summary keeps lifecycle payment delivery and attention independent', async () => {
    for (const language of ['zh', 'en', 'ja']) {
      await fresh('?mode=admin&section=payments&finance_tab=orders');
      await page.select('.adm-sidebar-foot select', language);
      for (const facts of [
        { status: 'EXPIRED', fulfillment_status: 'PENDING', payment_summary: { paid_count: 0 }, attention_summary: 'NORMAL', paymentWord: 'noRecordedPayment' },
        { status: 'PAID', fulfillment_status: 'CREDITED', payment_summary: { paid_count: 1 }, attention_summary: 'RECEIVABLE_OPEN', paymentWord: 'paymentRecorded' },
      ]) {
        await page.evaluate(facts => {
          const id = '00000000-0000-4000-8000-000000000002';
          window.restored.adminOrderDetails = { [id]: { order_id: id, amount_fen: '990', ...facts, transactions: [], entitlement_packages: [] } };
          const url = new URL(location.href); url.searchParams.set('order_id', id);
          history.pushState({}, '', url); dispatchEvent(new PopStateEvent('popstate'));
        }, facts);
        await page.waitForSelector('[data-workspace=orders] .adm-order-status-summary');
        const actual = await page.$eval('.adm-order-status-summary', node => [...node.children].map(item => item.textContent));
        const expected = await page.evaluate(facts => {
          const copy = window.restored.copy(), words = copy.adminLayout.orderStatusSummary;
          return [words.order, copy.accountLayout.statuses[facts.status], words.payment, words[facts.paymentWord], words.fulfillment, words.fulfillmentStates[facts.fulfillment_status], words.attention, words.attentionStates[facts.attention_summary]];
        }, facts);
        assert.deepEqual(actual, expected, 'all four labels and independent values must render in order');
        assert.equal(await page.$$eval('.adm-order-status-summary + p', nodes => nodes.length), facts.status === 'EXPIRED' ? 1 : 0);
        await page.evaluate(() => { const url = new URL(location.href); url.searchParams.delete('order_id'); history.pushState({}, '', url); dispatchEvent(new PopStateEvent('popstate')); });
        await page.waitForFunction(() => !document.querySelector('.adm-order-status-summary'));
      }
    }
  });
  await run('admin order detail retires late A before showing B and can revisit A', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=orders');
    const first = '00000000-0000-4000-8000-000000000002', second = '00000000-0000-4000-8000-000000000003';
    const navigate = async id => page.evaluate(id => { const url = new URL(location.href); if (id) url.searchParams.set('order_id', id); else url.searchParams.delete('order_id'); history.pushState({}, '', url); dispatchEvent(new PopStateEvent('popstate')); }, id);
    await page.evaluate(({first,second}) => {
      window.restored.delayAdminOrderDetail = [first, second];
      window.restored.adminOrderDetails = {
        [first]: { order_id: first, amount_fen: '101', status: 'EXPIRED', fulfillment_status: 'PENDING', payment_summary: { paid_count: 0 }, attention_summary: 'NORMAL', transactions: [], entitlement_packages: [] },
        [second]: { order_id: second, amount_fen: '202', status: 'PAID', fulfillment_status: 'CREDITED', payment_summary: { paid_count: 1 }, attention_summary: 'RECEIVABLE_OPEN', transactions: [], entitlement_packages: [] },
      };
    }, {first,second});
    await navigate(first);
    await page.waitForFunction(id => typeof window.restored.pendingAdminOrderDetails?.[id] === 'function', {}, first);
    await navigate(null);
    await page.waitForFunction(() => !new URL(location.href).searchParams.has('order_id'));
    await navigate(second);
    await page.waitForFunction(id => new URL(location.href).searchParams.get('order_id') === id, {}, second);
    await page.evaluate(id => window.restored.pendingAdminOrderDetails[id](), first);
    await page.waitForFunction(id => typeof window.restored.pendingAdminOrderDetails?.[id] === 'function', {}, second);
    assert.equal(await page.$('[data-workspace=orders] article'), null, 'late A must not populate the pending B visit');
    await page.evaluate(id => window.restored.pendingAdminOrderDetails[id](), second);
    await page.waitForSelector('[data-workspace=orders] .adm-order-status-summary');
    assert.equal(await page.$eval('[data-workspace=orders] .adm-order-detail-header code',node=>node.textContent),second,'the accepted B visit owns the separate detail header');
    const detail = await page.$eval('[data-workspace=orders] article', node => node.textContent);
    assert.ok(!detail.includes(first));
    assert.ok(await page.$eval('[data-workspace=orders] .adm-order-detail-header',node=>node.textContent.includes('2.02')&&!node.textContent.includes('1.01')),'the displayed amount comes from B, not the retired A response');
    await page.evaluate(() => { window.restored.delayAdminOrderDetail = []; });
    await navigate(first);
    await page.waitForFunction(id => document.querySelector('[data-workspace=orders] .adm-order-detail-header code')?.textContent===id&&!!document.querySelector('[data-workspace=orders] article'), {}, first);
    assert.ok(await page.$eval('[data-workspace=orders] .adm-order-detail-header',node=>node.textContent.includes('1.01')&&!node.textContent.includes('2.02')),'a fresh A visit accepts only A facts');
    assert.equal(await page.$$eval('.adm-order-status-summary dd', nodes => nodes.length), 4, 'a new visit remains usable after retiring the old response');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0, 'detail navigation performs no business writes');
  });
  for (const [index,kind] of [[0,'wallet'],[1,'requester']]) await run(`adjustment lookup ${kind} retains a failed continuation and clears denied directory facts`, async () => {
    await fresh('?mode=admin&section=billing&finance_tab=adjustments&lookupPaging=1');
    await page.waitForSelector('.adm-adjustment-lookup');
    const lookup = await page.$$('.adm-adjustment-lookup');
    assert.equal(lookup.length,2);
    // Tag the actual field without depending on other filter fieldset positions.
    await lookup[index].evaluate(node=>{node.dataset.testLookup='current';});
    for (const handle of lookup) await handle.dispose();
    const selector='[data-test-lookup=current]',results=selector+' .adm-adjustment-lookup-results';
    const words=await page.evaluate(()=>window.restored.copy().adminLayout.adjustmentLookup);
    const originalID='33333333-3333-4333-8333-333333333333';
    await page.type(selector+' > label input',originalID);
    await click(selector+' > button',kind==='wallet'?words.findWallet:words.findRequester);
    await page.type(results+' input[type=search]','controlled-lookup');
    await click(results+' > button',words.search);
    await page.waitForFunction(selector=>document.querySelectorAll(selector+' li').length===1,{},results);
    const first=await page.$eval(results+' li',node=>node.textContent);
    await page.evaluate(()=>{window.restored.lookupFailure=503;});
    await click(results+' > button',words.more);
    await page.waitForSelector(results+' [role=alert]');
    assert.equal(await page.$eval(results+' [role=alert]',node=>node.textContent),words.error);
    assert.deepEqual(await page.$$eval(results+' li',nodes=>nodes.map(node=>node.textContent)),[first],'continuation failure retains the accepted first page');
    await page.evaluate(()=>{window.restored.lookupFailure=0;});
    await click(results+' > button',words.more);
    await page.waitForFunction(selector=>document.querySelectorAll(selector+' li').length===2,{},results);
    assert.equal(await page.$(results+' [role=alert]'),null);
    const continuations=await page.evaluate(kind=>window.restored.calls.filter(call=>call.method==='GET'&&call.url.includes(kind==='wallet'?'/wallets?':'/users?')&&new URL(call.url,location.origin).searchParams.get('cursor')==='controlled-lookup-next').map(call=>call.url),kind);
    assert.equal(continuations.length,2);assert.equal(continuations[0],continuations[1],'retry preserves the failed cursor');
    await click(results+' > button',words.search);
    await page.waitForFunction(selector=>document.querySelectorAll(selector+' li').length===1,{},results);
    await page.evaluate(()=>{window.restored.lookupFailure=403;});
    await click(results+' > button',words.more);
    await page.waitForFunction(selector=>document.querySelectorAll(selector+' li').length===0,{},results);
    await page.waitForFunction(({selector,words})=>[...document.querySelectorAll(selector+' [role=alert],'+selector+' small')].some(node=>node.textContent===words.forbidden||node.textContent===words.directoryUnavailable),{},{selector,words});
    assert.equal(await page.$eval(selector+' > label input',node=>node.value),originalID,'directory denial does not replace the independent exact filter ID');
    assert.equal(await page.evaluate(()=>window.restored.calls.filter(call=>call.method!=='GET'&&!call.url.endsWith('/account/bootstrap')).length),0,'lookup and retries perform no business writes');
  });
  await run('original payment filters and selected detail survive URL reload', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=orders&order_status=PAID&order_provider_id=controlled-provider&order_channel=ALIPAY&order_created_from=2026-10-01T00%3A00%3A12.345Z&order_limit=50&order_id=00000000-0000-4000-8000-000000000002');
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/payment-orders?') && call.url.includes('status=PAID') && call.url.includes('provider_id=controlled-provider') && call.url.includes('channel=ALIPAY') && call.url.includes('limit=50') && call.url.includes('created_from=')));
    await page.waitForSelector('[data-workspace=orders] article');
    assert.equal(await page.$eval('[data-workspace=orders] .adm-order-detail-header code', node => node.textContent), '00000000-0000-4000-8000-000000000002');
    assert.ok(await page.$eval('[data-workspace=orders] article', node => Boolean(node.getAttribute('aria-labelledby')) && node.getAttribute('aria-labelledby') === node.closest('[data-workspace=orders]').querySelector('.adm-order-detail-header h3').id));
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-workspace=orders] article');
    await page.waitForFunction(() => window.restored.calls.some(call => call.url.includes('/payment-orders?') && call.url.includes('status=PAID')));
    assert.equal(await page.$eval('[data-workspace=orders] .adm-order-detail-header code', node => node.textContent), '00000000-0000-4000-8000-000000000002');
    assert.equal(new URL(page.url()).searchParams.get('order_channel'), 'ALIPAY');
  });
  await run('history during a pending filtered read coalesces the latest URL without stale rows', async () => {
    await fresh('?mode=admin&section=payments&finance_tab=orders&order_status=PENDING');
    await page.evaluate(() => { window.restored.filteredOrders = true; window.restored.reload(); });
    const expected = await page.evaluate(() => ({ orderID: '00000000-0000-4000-8000-000000000002', pending: window.restored.copy().accountLayout.statuses.PENDING, paid: window.restored.copy().accountLayout.statuses.PAID }));
    await page.waitForFunction(({ orderID, pending }) => {
      const rows = document.querySelectorAll('[data-workspace=orders] tbody tr');
      return rows.length === 1 && rows[0].cells[0]?.querySelector('code')?.textContent === orderID && rows[0].cells[6]?.textContent === pending;
    }, {}, expected);
    await page.waitForFunction(() => !document.querySelector('[data-workspace=orders] .adm-filter-row button').disabled);
    await page.evaluate(() => { window.restored.delayAdminOrders = true; });
    await page.select('[data-workspace=orders] .adm-filter-row select', 'PAID'); await page.$eval('[data-workspace=orders] .adm-filter-row', form => form.requestSubmit());
    await page.waitForFunction(() => typeof window.restored.releaseAdminOrders === 'function');
    const before = await page.evaluate(() => window.restored.calls.filter(call => call.url.includes('/payment-orders?') && new URL(call.url, location.origin).searchParams.get('status') === 'PENDING').length);
    await page.goBack(); await page.waitForFunction(() => new URLSearchParams(location.search).get('order_status') === 'PENDING');
    await page.evaluate(() => window.restored.releaseAdminOrders());
    await page.waitForFunction(({ before, orderID, pending }) => {
      const rows = document.querySelectorAll('[data-workspace=orders] tbody tr');
      return window.restored.calls.filter(call => call.url.includes('/payment-orders?') && new URL(call.url, location.origin).searchParams.get('status') === 'PENDING').length === before + 1 && rows.length === 1 && rows[0].cells[0]?.querySelector('code')?.textContent === orderID && rows[0].cells[6]?.textContent === pending;
    }, {}, { ...expected, before });
    assert.notEqual(await page.$eval('[data-workspace=orders] tbody tr td:nth-child(7)', node => node.textContent), expected.paid);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method !== 'GET' && !call.url.endsWith('/account/bootstrap')).length), 0);
  });
  await run('review detail failure and mismatched receipt never expose the previously selected case', async () => {
    const first = '22222222-2222-4222-8222-222222222222', second = '33333333-3333-4333-8333-333333333333';
    const failedDetailReady = ({ tab, endpoint, second, count }) => {
      const retry = [...document.querySelectorAll(`[data-workspace=${tab}] nav button`)].find(node => node.textContent.trim() === window.restored.copy().finance.retry);
      return window.restored.calls.filter(call => call.url.endsWith('/' + endpoint + '/' + second)).length === count && document.querySelector(`[data-workspace=${tab}] [role=alert]`) && Boolean(retry && retry.getClientRects().length && !retry.disabled);
    };
    for (const [tab, prefix, endpoint] of [['late', 'late', 'late-payment-reviews'], ['anomalies', 'anomaly', 'payment-anomalies']]) {
      await fresh(`?mode=admin&section=payments&finance_tab=${tab}`);
      await page.evaluate(({ tab, prefix, first }) => { window.restored.reviewFixture = true; window.restored.permissions = ['platform.billing.wallets.read', 'platform.billing.orders.sync', 'platform.billing.repairs.request']; history.replaceState({}, '', `?mode=admin&section=payments&finance_tab=${tab}&${prefix}_id=${first}`); dispatchEvent(new PopStateEvent('popstate')); window.restored.reload(); }, { tab, prefix, first });
      await page.waitForFunction(({ tab, first }) => document.querySelector(`[data-workspace=${tab}] article h4`)?.textContent === first, {}, { tab, first });
      await page.evaluate(({ tab, prefix, second }) => { window.restored.failReviewID = second; history.pushState({}, '', `?mode=admin&section=payments&finance_tab=${tab}&${prefix}_id=${second}`); dispatchEvent(new PopStateEvent('popstate')); }, { tab, prefix, second });
      await page.waitForFunction(failedDetailReady, {}, { tab, endpoint, second, count: 1 });
      assert.equal(await page.$(`[data-workspace=${tab}] article`), null);
      await page.evaluate(first => { window.restored.failReviewID = ''; window.restored.reviewAckID = first; }, first);
      await click(`[data-workspace=${tab}] nav button`, await page.evaluate(() => window.restored.copy().finance.retry));
      await page.waitForFunction(failedDetailReady, {}, { tab, endpoint, second, count: 2 });
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
      const formSelector = `[data-workspace=${tab}] form`;
      await page.waitForSelector(`${formSelector} input[type=datetime-local]`);
      assert.equal(await page.$$eval(formSelector, forms => forms.length), 1, 'the read workspace exposes one filter form');
      await page.waitForFunction(selector => {
        const submit = document.querySelector(selector)?.querySelector('button[type=submit], button:not([type])');
        return Boolean(submit && !submit.matches(':disabled'));
      }, {}, formSelector);
      const before = await page.evaluate(endpoint => window.restored.calls.filter(call => call.url.includes('/' + endpoint + '?')).length, endpoint);
      await page.$eval(formSelector, form => form.requestSubmit());
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
  const typeGrantTarget = async target => {
    const closedAdvanced = '[role=dialog] details.adm-grant-manual-target:not([open]) > summary';
    if (await page.$(closedAdvanced)) await page.click(closedAdvanced);
    const input = '[role=dialog] .adm-role-field input';
    await page.waitForSelector(input, { visible: true });
    await page.type(input, target);
  };
  const openRoleReview = async (target = '22222222-2222-4222-8222-222222222222', role = 'PLATFORM_BILLING_OPERATOR', expires = '') => {
    const words = await roleWords();
    await click('.adm-role-workspace-head button', words.newGrant);
    await page.waitForSelector('[role=dialog] .adm-role-grant');
    await typeGrantTarget(target);
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
  await run('roles: grant drawer keeps three steps reachable with native focus at normal and short heights', async () => {
    for (const sample of [{ language: 'zh', width: 1440, height: 900 }, { language: 'en', width: 390, height: 844 }, { language: 'ja', width: 1024, height: 420 }]) {
      await readyRoles();
      await page.evaluate(language => { window.restored.language = language; window.restored.reload(); }, sample.language);
      await page.waitForSelector('.adm-role-workspace-head .adm-role-primary');
      await page.setViewport({ width: sample.width, height: sample.height });
      const words = await roleWords();
      const trigger = '.adm-role-workspace-head .adm-role-primary';
      await page.focus(trigger); await page.keyboard.press('Enter');
      await page.waitForSelector('.adm-role-grant-drawer .adm-grant-step');
      const measure = async step => {
        await page.$eval('.adm-grant-step .adm-role-drawer-foot button:last-child', node => node.scrollIntoView({ block: 'nearest' }));
        const geometry = await page.evaluate(() => {
          const drawer = document.querySelector('.adm-role-grant-drawer'), body = drawer.querySelector('.adm-grant-step-body'), footer = drawer.querySelector('.adm-role-drawer-foot');
          const box = footer.getBoundingClientRect(), outer = drawer.getBoundingClientRect();
          const clip = { top: 0, bottom: innerHeight, left: 0, right: innerWidth };
          let scrollHost = null;
          for (let node = footer.parentElement; node; node = node.parentElement) {
            const style = getComputedStyle(node), rect = node.getBoundingClientRect();
            if (!scrollHost && ['auto', 'scroll', 'overlay'].includes(style.overflowY)) scrollHost = node;
            if (['auto', 'scroll', 'hidden', 'clip', 'overlay'].includes(style.overflowY)) {
              clip.top = Math.max(clip.top, rect.top + node.clientTop);
              clip.bottom = Math.min(clip.bottom, rect.top + node.clientTop + node.clientHeight);
            }
            if (['auto', 'scroll', 'hidden', 'clip', 'overlay'].includes(style.overflowX)) {
              clip.left = Math.max(clip.left, rect.left + node.clientLeft);
              clip.right = Math.min(clip.right, rect.left + node.clientLeft + node.clientWidth);
            }
          }
          const controls = [...footer.querySelectorAll('button')].map(node => {
            const rect = node.getBoundingClientRect(), range = document.createRange(); range.selectNodeContents(node);
            const label = range.getBoundingClientRect(), hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
            return { top: rect.top, bottom: rect.bottom, left: rect.left, right: rect.right, width: rect.width, height: rect.height, label: { top: label.top, bottom: label.bottom, left: label.left, right: label.right, width: label.width, height: label.height }, hit: hit === node || node.contains(hit) };
          });
          return { top: box.top, bottom: box.bottom, left: box.left, right: box.right, outerTop: outer.top, outerBottom: outer.bottom, bodyOverflow: getComputedStyle(body).overflowY, footerFlex: getComputedStyle(footer).flexShrink, height: innerHeight, width: innerWidth, clip, controls, outerScrollHost: scrollHost === drawer };
        });
        assert.ok(geometry.top >= -1 && geometry.bottom <= geometry.height + 1 && geometry.left >= -1 && geometry.right <= geometry.width + 1, JSON.stringify({ sample, step, geometry }));
        const inside = (rect, tolerance) => rect.width > 0 && rect.height > 0 && rect.top >= geometry.clip.top - tolerance && rect.bottom <= geometry.clip.bottom + tolerance && rect.left >= geometry.clip.left - tolerance && rect.right <= geometry.clip.right + tolerance;
        assert.ok(geometry.controls.length > 0 && geometry.controls.every(control => inside(control, 1) && inside(control.label, 0.5) && control.hit), 'footer controls and labels must remain visible and hittable within their actual scroll host: ' + JSON.stringify({ sample, step, geometry }));
        if (sample.height > 580) {
          assert.equal(geometry.bodyOverflow, 'auto'); assert.equal(geometry.footerFlex, '0');
          const before = geometry.top;
          await page.$eval('.adm-grant-step-body', node => { node.scrollTop = node.scrollHeight; });
          assert.ok(Math.abs(await page.$eval('.adm-role-drawer-foot', node => node.getBoundingClientRect().top) - before) <= 1, 'body scrolling keeps the footer in its slot');
        } else {
          assert.equal(geometry.bodyOverflow, 'visible', 'short-height mode keeps one outer scroll area');
          assert.equal(geometry.outerScrollHost, true, 'short-height controls scroll in the drawer itself');
        }
        console.log('GRANT_LAYOUT', JSON.stringify({ sample, step, geometry }));
      };
      await measure(1);
      await page.focus('.adm-role-grant-drawer > header button');
      await page.keyboard.press('Tab');
      assert.equal(await page.evaluate(() => Boolean(document.activeElement.closest('.adm-role-grant-drawer'))), true);
      await typeGrantTarget('22222222-2222-4222-8222-222222222222');
      await click('[role=dialog] button', words.next);
      await page.waitForSelector('[role=dialog] select'); await measure(2);
      await page.select('[role=dialog] select', 'PLATFORM_BILLING_OPERATOR');
      await page.type('[role=dialog] textarea', 'Synthetic drawer review');
      await click('[role=dialog] button', words.next);
      await page.waitForSelector('[role=dialog] .adm-role-preview'); await measure(3);
      await page.focus('.adm-grant-step .adm-role-drawer-foot button:last-child');
      await page.keyboard.press('Tab');
      assert.equal(await page.$eval('.adm-role-grant-drawer > header button', node => document.activeElement === node), true, 'last Tab stays inside the native dialog');
      await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
      assert.equal(await page.$eval('.adm-grant-step .adm-role-drawer-foot button:last-child', node => document.activeElement === node), true);
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
      assert.equal(await page.$eval(trigger, node => document.activeElement === node), true);
      assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'POST' && call.url.endsWith('/role-bindings')).length), 0, 'review and cancellation never grant a role');
    }
  });
  await run('roles: original catalog and permission groups fit desktop and narrow viewports', async () => {
    await readyRoles();
    for (const language of ['zh', 'en', 'ja']) {
      await page.select('.adm-sidebar-foot select', language);
      const words = await roleWords();
      assert.equal(await page.$eval('.adm-role-detail h3', node => node.textContent), words.names.PLATFORM_AUDITOR);
      assert.equal(await page.$$eval('.adm-role-permission-groups details', nodes => nodes.length), 4);
      assert.deepEqual(await page.$$eval('.adm-role-permission code', nodes => nodes.map(node => node.textContent).sort()), ['platform.audit.export', 'platform.audit.read', 'platform.billing.ledger.read', 'platform.billing.wallets.read', 'platform.models.read', 'platform.organizations.read', 'platform.roles.read', 'platform.system.status.read', 'platform.usage.read', 'platform.users.read'].sort());
      assert.deepEqual(await page.$$eval('.adm-role-permission-groups summary > span:last-child', nodes => nodes.map(node => Number(node.textContent))), [3, 3, 1, 3]);
      assert.equal(await page.$$eval('.adm-role-permission .adm-identifier-copy', nodes => nodes.length), 10);
      assert.equal(await page.$$eval('.adm-role-permission .adm-identifier-toggle', nodes => nodes.length), 0, 'full permission codes need no expansion action');
      const firstGroup = await page.$('.adm-role-permission-groups summary'); await firstGroup.focus();
      await page.keyboard.press('Enter'); assert.equal(await page.$eval('.adm-role-permission-groups details', node => node.open), false);
      await page.keyboard.press('Enter'); assert.equal(await page.$eval('.adm-role-permission-groups details', node => node.open), true); await firstGroup.dispose();
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
        assert.ok(await page.$$eval('.adm-role-permission > strong', nodes => nodes.every(node => Number.parseFloat(getComputedStyle(node).fontSize) >= 14)));
        assert.ok(await page.$$eval('.adm-role-permission .adm-identifier code', nodes => nodes.every(node => Number.parseFloat(getComputedStyle(node).fontSize) >= 12 && getComputedStyle(node).whiteSpace === 'normal')));
        const catalogScroll = await page.$$eval('.adm-role-catalog-layout>.adm-table-wrap,.adm-role-catalog-layout>.adm-role-detail', nodes => nodes.map(node => ({ height: node.clientHeight, innerVerticalScroll: ['auto', 'scroll'].includes(getComputedStyle(node).overflowY) && node.scrollHeight > node.clientHeight + 1 })));
        assert.equal(catalogScroll.length, 2); assert.ok(catalogScroll.every(item => item.height > 0 && !item.innerVerticalScroll), 'catalog and detail use the main page for vertical reading');
        await page.$eval('.adm-role-table button[aria-pressed=true]', node => node.focus());
        await page.keyboard.press('Enter');
        assert.ok(await page.evaluate(() => { const heading = document.querySelector('.adm-role-detail h3'); return Boolean(heading && document.activeElement === heading); }), 'explicit selection focuses the current detail heading');
        assert.equal(await page.$eval('.adm-role-detail h3', node => node.textContent), words.names.PLATFORM_AUDITOR);
        await page.keyboard.press('Tab');
        assert.ok(await page.evaluate(() => document.activeElement?.classList.contains('adm-role-catalog-back')), 'return is next in the detail keyboard order');
        await page.keyboard.press('Enter');
        assert.ok(await page.evaluate(() => { const selected = document.querySelector('.adm-role-table button[aria-pressed=true]'); return Boolean(selected && document.activeElement === selected); }), 'return focuses the actual current role control');

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
  await run('roles: untranslated long role heading stays readable with its scope badge', async () => {
    await fresh('?mode=admin&section=roles&longCatalogRole=1');
    const code = `PLATFORM_EXTENSION_${'X'.repeat(160)}_TAIL`;
    await page.waitForSelector('.adm-role-table tbody tr:nth-child(5)');
    const buttons = await page.$$('.adm-role-table tbody button');
    try {
      const text = await Promise.all(buttons.map(button => button.evaluate(node => node.textContent)));
      const target = buttons[text.indexOf(code)];
      assert.ok(target, 'the unlocalized production-shaped role appears in the real list');
      await target.focus(); await page.keyboard.press('Enter');
      await page.waitForFunction(code => {
        const heading = document.querySelector('.adm-role-detail h3');
        return heading?.textContent === code && document.activeElement === heading;
      }, {}, code);
    } finally { await Promise.all(buttons.map(button => button.dispose())); }
    for (const width of [1300, 390]) {
      await page.setViewport({ width, height: 907 });
      await noOverflow();
      const layout = await page.$eval('.adm-role-detail h3', heading => {
        const header = heading.closest('header'), panel = heading.closest('.adm-role-panel');
        const textColumn = heading.parentElement, scope = header.querySelector('.adm-role-scope');
        const id = textColumn.querySelector('code');
        const description = heading.closest('.adm-role-detail').querySelector(':scope > p');
        const box = node => {
          const rect = node.getBoundingClientRect();
          return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width, height: rect.height };
        };
        const fragmentsFor = node => {
          const range = document.createRange(); range.selectNodeContents(node);
          return [...range.getClientRects()].map(rect => ({ left: rect.left, right: rect.right, width: rect.width, top: rect.top }));
        };
        const fragments = fragmentsFor(heading);
        const lines = [];
        for (const fragment of fragments) if (!lines.some(top => Math.abs(top - fragment.top) < 0.5)) lines.push(fragment.top);
        return { text: heading.textContent, fragments, lines: lines.length, column: box(textColumn), badge: box(scope), panel: box(panel), id: { text: id.textContent, fragments: fragmentsFor(id) }, description: { text: description.textContent, box: box(description), fragments: fragmentsFor(description) } };
      });
      const fits = (inner, outer) => inner.width > 0 && inner.left >= outer.left - 0.5 && inner.right <= outer.right + 0.5;
      assert.equal(layout.text, code); assert.equal(layout.id.text, code);
      assert.ok(layout.id.fragments.length > 1 && layout.id.fragments.every(rect => fits(rect, layout.column) && fits(rect, layout.panel)), JSON.stringify(layout));
      assert.ok(layout.lines > 1 && layout.fragments.every(rect => fits(rect, layout.column) && fits(rect, layout.panel)), JSON.stringify(layout));
      const separated = (first, second) => first.right <= second.left + 0.5 || second.right <= first.left + 0.5 || first.bottom <= second.top + 0.5 || second.bottom <= first.top + 0.5;
      assert.ok(fits(layout.badge, layout.panel) && layout.badge.height > 0 && fits(layout.column, layout.panel) && separated(layout.column, layout.badge), JSON.stringify(layout));
      assert.equal(layout.description.text, `Extension${'W'.repeat(160)}Description`);
      assert.ok(layout.description.fragments.length > 1 && layout.description.fragments.every(rect => fits(rect, layout.description.box) && fits(rect, layout.panel)), JSON.stringify(layout));
    }
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
    assert.equal(await page.$eval('[role=dialog] .adm-grant-target-readback code', node => node.textContent), '22222222-2222-4222-8222-222222222222');
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
    await click('[data-workspace=users] .adm-panel button', await page.evaluate(() => window.restored.copy().adminLayout.viewUserPlatformGrants));
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
    const detailIdentitySizes = await page.$$eval('[role=dialog] .adm-binding-identity code', nodes => nodes.map(node => Number.parseFloat(getComputedStyle(node).fontSize)));
    assert.ok(detailIdentitySizes.length > 0 && detailIdentitySizes.every(size => size >= 12), 'detail identity codes keep the common readable size');
    const revokeLabel = await page.evaluate(() => window.restored.copy().roles.revoke);
    await click('[role=dialog] button', revokeLabel);
    await page.waitForSelector('[role=dialog] .adm-role-revoke-review');
    const review = await page.$eval('[role=dialog]', node => node.textContent);
    assert.ok(review.includes('66666666-6666-4666-8666-666666666666') && review.includes('22222222-2222-4222-8222-222222222222'));
    assert.ok(review.includes(words.scopes.PLATFORM) && review.includes(words.revokeBindingImpact));
    const reviewIdentitySizes = await page.$$eval('[role=dialog] .adm-binding-identity code', nodes => nodes.map(node => Number.parseFloat(getComputedStyle(node).fontSize)));
    assert.ok(reviewIdentitySizes.length > 0 && reviewIdentitySizes.every(size => size >= 12), 'confirmation identity codes use the same readable size');
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'DELETE').length), 0);
    await click('[role=dialog] button', words.cancel);
    await page.waitForSelector('[role=dialog] .adm-role-preview');
    assert.equal(await page.$('[role=dialog] .adm-role-revoke-review'), null);
    assert.equal(await page.evaluate(() => window.restored.calls.filter(call => call.method === 'DELETE').length), 0);
    await click('[role=dialog] button', revokeLabel);
    await page.waitForSelector('[role=dialog] .adm-role-revoke-review');
    await click('[role=dialog] button', revokeLabel);
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
    await typeGrantTarget('22222222-2222-4222-8222-222222222222');
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
finally { await fixtureScope.close(); }
