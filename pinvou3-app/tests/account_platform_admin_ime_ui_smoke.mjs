import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createServer } from './helpers/vite-fixture.mjs';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';

const root = import.meta.dirname + '/..';
const browserPath = [process.env.CHROME, '/usr/bin/google-chrome', '/usr/bin/chromium', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe'].find(value => value && fs.existsSync(value));
assert.ok(browserPath, 'Chromium is required; set CHROME to its executable');
const server = await createServer({
  root, configFile: false,
  plugins: [{
    name: 'private-admin-ime-component-fixture', enforce: 'pre',
    // Test-only exports append aliases to the intact source module. No handler,
    // state hook, or imported dependency is replaced by the test transform.
    configureServer(vite) {
      vite.middlewares.use((request, response, next) => {
        if (request.url !== '/credential-handoff') return next();
        // Controlled same-origin popup runs the actual handoff MessagePort
        // protocol. Imported handoff/launch helpers remain intact.
        response.setHeader('Content-Type', 'text/html');
        response.end(`<script>window.addEventListener('message', event => {
          const port = event.ports[0], nonce = event.data.nonce;
          port.onmessage = () => { if (!window.opener.adminIme.lostAck) port.postMessage({ version: 1, type: 'HANDOFF_ACCEPTED', nonce }); };
          port.postMessage({ version: 1, type: 'HANDOFF_BOUND', nonce });
        }, { once: true });
        window.opener.postMessage({ version: 1, type: 'HANDOFF_READY' }, location.origin);</script>`);
      });
    },
    transform(source, id) {
      if (id.endsWith('/src/features/account-platform/AdminTokenAccountDetailWorkspace.jsx')) return source + '\nexport { CredentialLaunchDialog as ImeCredentialLaunchDialog };';
      if (id.endsWith('/src/features/account-platform/TokenAccountGovernanceActions.jsx')) return source + '\nexport { RouteDialog as ImeRouteDialog };';
      if (id.endsWith('/src/features/account-platform/AdminProtocolFormulaWorkspace.jsx')) return source + '\nexport { Dialog as ImeProtocolDialog };';
      if (id.endsWith('/src/features/account-platform/AdminTokenResourcesWorkspace.jsx')) return source + '\nexport { OwnerDialog as ImeOwnerDialog, TokenAccountRecoveryDialog as ImeRecoveryDialog };';
    },
  }, react()],
  cacheDir: root + '/node_modules/.vite-admin-ime',
  server: { host: '127.0.0.1', port: 0, fs: { allow: [root, fs.realpathSync(root + '/node_modules')], deny: ['**/.env*', '**/*.{pem,crt,key}'] } },
});
await server.listen();
let browser;
const results = [];
const draft = 'Controlled draft 中文入力';
const selector = kind => kind === 'closure' ? '[data-testid="closure-opener"]' : kind === 'route' ? 'input[type="search"]' : kind === 'owner' ? 'input[maxlength="120"]' : kind === 'protocol' ? 'textarea[rows="4"]' : 'textarea';
async function key(page, target, name, variant = 'ordinary') {
  return page.$eval(target, (element, input) => {
    element.focus();
    const event = new KeyboardEvent('keydown', { key: input.name, bubbles: true, cancelable: true, isComposing: input.variant === 'composing', keyCode: input.variant === '229' ? 229 : input.name === 'Enter' ? 13 : 27 });
    element.dispatchEvent(event);
    return { prevented: event.defaultPrevented, isComposing: event.isComposing, keyCode: event.keyCode };
  }, { name, variant });
}
async function remains(page, target, expected = draft) {
  assert.equal(await page.evaluate(() => window.adminIme.closed), 0);
  assert.equal(await page.$eval(target, element => element.value), expected);
}
async function trustedSearchEnter(page, variant, target = 'input[type="search"]') {
  await page.focus(target);
  await page.evaluate(value => {
    // Keep the browser's trusted key/default action, controlling only the two
    // composition fields consumed by the application. This models the guard's
    // inputs; it does not operate an OS IME candidate popup.
    window.addEventListener('keydown', event => {
      if (event.key !== 'Enter') throw new Error('Expected trusted Enter');
      Object.defineProperty(event, 'isComposing', { value: value === 'composing' });
      Object.defineProperty(event, 'keyCode', { value: value === '229' ? 229 : 13 });
    }, { capture: true, once: true });
    window.addEventListener('keydown', event => {
      window.adminIme.keyEvents.push({ trusted: event.isTrusted, isComposing: event.isComposing, keyCode: event.keyCode, prevented: event.defaultPrevented });
    }, { once: true });
  }, variant);
  await page.keyboard.press('Enter');
  await page.waitForFunction(() => window.adminIme.keyEvents.length === 1);
  return page.evaluate(() => window.adminIme.keyEvents[0]);
}
async function run(name, kind, action) {
  const page = await browser.newPage();
  page.setDefaultTimeout(6000);
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  try {
    await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/account-platform-admin-ime.html?kind=' + kind);
    await page.waitForSelector(selector(kind));
    await action(page);
    assert.deepEqual(errors, []);
    results.push({ name, pass: true });
  } catch (error) {
    results.push({ name, pass: false, error: error.message, errors, diagnostic: await page.evaluate(() => ({ text: document.body.innerText, closed: window.adminIme?.closed, queries: window.adminIme?.queries })).catch(() => null) });
  } finally { await page.close(); }
}
try {
  browser = await puppeteer.launch({ executablePath: browserPath, headless: true, args: ['--disable-gpu', '--no-sandbox'] });
  for (const kind of ['high-risk', 'protocol', 'recovery', 'owner', 'credential', 'route']) {
    await run(kind + ': composing Escape retains draft; Tab wraps; ordinary Escape closes', kind, async page => {
      const target = selector(kind);
      await page.type(target, draft);
      for (const variant of ['composing', '229']) {
        const event = await key(page, target, 'Escape', variant);
        assert.equal(event.isComposing, variant === 'composing');
        if (variant === '229') assert.equal(event.keyCode, 229);
        await remains(page, target);
      }
      await page.evaluate(() => { const controls = [...document.querySelector('[role="dialog"]').querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled),textarea:not(:disabled),[tabindex]:not([tabindex="-1"])')]; controls.at(-1).focus(); });
      await page.keyboard.press('Tab');
      assert.equal(await page.evaluate(() => document.activeElement === document.querySelector('[role="dialog"] button:not(:disabled)')), true);
      await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
      assert.equal(await page.evaluate(() => { const controls = [...document.querySelector('[role="dialog"]').querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled),textarea:not(:disabled),[tabindex]:not([tabindex="-1"])')]; return document.activeElement === controls.at(-1); }), true);
      await key(page, target, 'Escape');
      await page.waitForSelector('[data-testid="closed"]');
      assert.equal(await page.evaluate(() => window.adminIme.closed), 1);
    });
  }
  for (const kind of ['high-risk', 'protocol', 'recovery']) for (const mode of ['busy', 'unknown']) {
    await run(kind + ': ' + mode + ' still blocks ordinary Escape', kind, async page => {
      const target = selector(kind);
      await page.type(target, draft);
      if (kind === 'protocol') {
        await page.evaluate(value => window.adminIme.setMode(value), mode);
      } else {
        await page.$eval('form[role="dialog"]', element => element.requestSubmit());
        await page.waitForFunction(() => window.adminIme.submissions === 1);
        if (mode === 'unknown') await page.evaluate(() => window.adminIme.reject());
      }
      await page.waitForFunction(() => document.querySelector('textarea[rows="4"]').disabled);
      if (mode === 'unknown' && kind !== 'protocol') await page.waitForSelector('[role="alert"]');
      await key(page, target, 'Escape');
      await remains(page, target);
    });
  }
  await run('owner: composing Enter keeps search draft; ordinary Enter searches once', 'owner', async page => {
    const target = 'input[type="search"]';
    await page.type(target, draft);
    for (const variant of ['composing', '229']) {
      await key(page, target, 'Enter', variant);
      assert.deepEqual(await page.evaluate(() => window.adminIme.queries), []);
      await remains(page, target);
    }
    await key(page, target, 'Enter');
    await page.waitForFunction(() => window.adminIme.queries.length === 1);
    assert.equal(await page.evaluate(() => window.adminIme.queries[0].q), draft);
    assert.equal(await page.evaluate(() => window.adminIme.submissions), 0);
  });
  for (const variant of ['composing', '229', 'ordinary']) {
    await run('owner: valid selected form, trusted ' + variant + ' Enter never submits', 'owner', async page => {
      await page.waitForSelector('[role="option"]');
      await page.click('[role="option"]');
      await page.type('input[maxlength="120"]', 'Controlled account');
      assert.equal(await page.$eval('form[aria-labelledby="token-owner-dialog-title"]', form => form.checkValidity() && !form.querySelector('button[type="submit"]').disabled), true);
      await page.type('input[type="search"]', draft);
      const event = await trustedSearchEnter(page, variant);
      assert.equal(event.trusted, true);
      assert.equal(event.isComposing, variant === 'composing');
      assert.equal(event.keyCode, variant === '229' ? 229 : 13);
      const observed = await page.evaluate(() => ({ queries: window.adminIme.queries, submits: window.adminIme.formSubmits, ownerStep: Boolean(document.getElementById('token-owner-dialog-title')), name: document.querySelector('input[maxlength="120"]')?.value, search: document.querySelector('input[type="search"]')?.value }));
      assert.deepEqual(observed.submits, [], JSON.stringify(observed));
      assert.equal(event.prevented, true);
      assert.equal(observed.ownerStep, true);
      assert.equal(observed.name, 'Controlled account');
      assert.equal(observed.search, draft);
      assert.equal(observed.queries.length, variant === 'ordinary' ? 1 : 0);
      if (variant === 'ordinary') assert.equal(observed.queries[0].q, draft);
    });
  }
  for (const variant of ['composing', '229', 'ordinary']) {
    await run('route: trusted ' + variant + ' Enter searches only after composition and never submits', 'route', async page => {
      await page.waitForFunction(() => window.adminIme.queries.length === 1);
      await page.type('input[type="search"]', draft);
      const event = await trustedSearchEnter(page, variant);
      assert.equal(event.trusted, true);
      assert.equal(event.prevented, true);
      assert.equal(event.isComposing, variant === 'composing');
      assert.equal(event.keyCode, variant === '229' ? 229 : 13);
      const observed = await page.evaluate(() => ({ queries: window.adminIme.queries, submits: window.adminIme.formSubmits }));
      assert.deepEqual(observed.submits, []);
      assert.equal(observed.queries.length, variant === 'ordinary' ? 2 : 1);
      if (variant === 'ordinary') assert.equal(observed.queries[1].q, draft);
      await remains(page, 'input[type="search"]');
    });
    await run('closure: trusted ' + variant + ' Enter dispatches consent only after composition', 'closure', async page => {
      await page.click('[data-testid="closure-opener"]');
      await page.waitForSelector('[role="alertdialog"] input');
      await page.type('[role="alertdialog"] input', 'CLOSE_MY_ACCOUNT');
      const event = await trustedSearchEnter(page, variant, '[role="alertdialog"] input');
      assert.equal(event.trusted, true);
      assert.equal(event.isComposing, variant === 'composing');
      assert.equal(event.keyCode, variant === '229' ? 229 : 13);
      assert.equal(event.prevented, variant !== 'ordinary');
      const observed = await page.evaluate(() => ({ confirms: window.adminIme.confirms, submits: window.adminIme.formSubmits, text: document.querySelector('[role="alertdialog"] input').value }));
      assert.equal(observed.text, 'CLOSE_MY_ACCOUNT');
      assert.deepEqual(observed.confirms, variant === 'ordinary' ? ['CLOSE_MY_ACCOUNT'] : []);
      assert.equal(observed.submits.length, variant === 'ordinary' ? 1 : 0);
      if (variant === 'ordinary') assert.equal(observed.submits[0].trusted, true);
    });
  }
  await run('closure: composing Escape retains consent; Tab wraps; ordinary Escape restores opener', 'closure', async page => {
    await page.click('[data-testid="closure-opener"]');
    await page.waitForSelector('[role="alertdialog"] input');
    assert.equal(await page.$eval('[role="alertdialog"] input', element => element === document.activeElement), true);
    await page.type('[role="alertdialog"] input', draft);
    for (const variant of ['composing', '229']) {
      await key(page, '[role="alertdialog"] input', 'Escape', variant);
      await remains(page, '[role="alertdialog"] input');
    }
    await page.focus('[role="alertdialog"] button[type="submit"]');
    await page.keyboard.press('Tab');
    assert.equal(await page.$eval('[role="alertdialog"] input', element => element === document.activeElement), true);
    await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
    assert.equal(await page.$eval('[role="alertdialog"] button[type="submit"]', element => element === document.activeElement), true);
    await key(page, '[role="alertdialog"] input', 'Escape');
    await page.waitForFunction(() => !document.querySelector('[role="alertdialog"]'));
    assert.equal(await page.$eval('[data-testid="closure-opener"]', element => element === document.activeElement), true);
    assert.equal(await page.evaluate(() => window.adminIme.closed), 1);
    assert.deepEqual(await page.evaluate(() => window.adminIme.confirms), []);
  });
  for (const lostAck of [false, true]) {
    await run('credential: ' + (lostAck ? 'unknown handoff' : 'waiting handoff') + ' preserves busy/cancellation and immutable result recovery', 'credential', async page => {
      await page.type('textarea', draft);
      await page.evaluate(value => { window.adminIme.lostAck = value; document.querySelector('form').requestSubmit(); }, lostAck);
      await page.waitForFunction(() => window.adminIme.submissions === 1);
      // PREPARING remains locked even for an ordinary Escape.
      await key(page, '[role="dialog"]', 'Escape');
      assert.equal(await page.evaluate(() => window.adminIme.closed), 0);
      assert.deepEqual(await page.evaluate(() => window.adminIme.revokes), []);
      await page.evaluate(() => window.adminIme.resolveLaunch());
      await page.waitForFunction(value => !document.querySelector('[role="dialog"] header button').disabled && (value ? window.adminIme.revokes.length === 1 : !document.querySelector('textarea')), {}, lostAck);
      const priorRevokes = lostAck ? 1 : 0;
      for (const variant of ['composing', '229']) {
        await key(page, '[role="dialog"]', 'Escape', variant);
        assert.equal(await page.evaluate(() => window.adminIme.closed), 0);
        assert.equal(await page.evaluate(() => window.adminIme.revokes.length), priorRevokes);
        assert.equal(await page.$eval('[role="dialog"] header button', element => element.disabled), false);
      }
      // An ordinary Escape follows the existing cancellation path; a lost
      // revoke acknowledgement cannot close the dialog or recreate a launch.
      await key(page, '[role="dialog"]', 'Escape');
      await page.waitForFunction(() => document.querySelector('[role="dialog"] header button').disabled && window.adminIme.revokes.length === 1);
      await key(page, '[role="dialog"]', 'Escape');
      assert.equal(await page.evaluate(() => window.adminIme.closed), 0);
      const observed = await page.evaluate(() => ({ revokes: window.adminIme.revokes, results: window.adminIme.resultQueries, submissions: window.adminIme.submissions }));
      assert.equal(observed.submissions, 1);
      assert.deepEqual(observed.revokes, [{ reference: '33333333-3333-4333-8333-333333333333', reason: 'CREDENTIAL_LAUNCH_CANCELLED' }]);
      assert.ok(observed.results.length > 0);
      assert.ok(observed.results.every(reference => reference === observed.revokes[0].reference));
      await page.evaluate(() => { window.adminIme.terminal = true; });
      await page.waitForFunction(() => !document.querySelector('[role="dialog"] header button').disabled);
      await key(page, '[role="dialog"]', 'Escape');
      await page.waitForSelector('[data-testid="closed"]');
      assert.equal(await page.evaluate(() => window.adminIme.closed), 1);
      assert.equal(await page.evaluate(() => window.adminIme.revokes.length), 1);
      assert.equal(await page.evaluate(() => window.adminIme.submissions), 1);
    });
  }

} finally {
  await browser?.close(); await server.close();
}
console.log(JSON.stringify({ scope: 'Mounted React components with controlled dependencies; synthetic IME Escape and trusted browser Enter with capture-controlled composition fields/default-action checks; not OS IME or full admin-page E2E', browserPath, results }, null, 2));
if (results.some(result => !result.pass)) process.exitCode = 1;
