import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import YAML from 'yaml';
import { portalLoginLocation } from '../../../pinvou3-app/src/features/account-platform/portal-sections.js';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

function nativeChromium() {
  const candidates = process.platform === 'win32' ? [
    process.env.CHROME,
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
    'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  ] : process.platform === 'darwin' ? [
    process.env.CHROME,
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
  ] : [
    process.env.CHROME, '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser',
  ];
  return candidates.find(candidate => candidate && path.isAbsolute(candidate) && existsSync(candidate)) || '';
}

function nativePuppeteer() {
  try {
    return createRequire(path.join(root, '../pinvou3-app/package.json'))('puppeteer-core');
  } catch {
    return null;
  }
}

function messageKeys(source) {
  return new Set(source.split(/\r?\n/).filter(line => line && !line.startsWith('#')).map(line => line.split('=', 1)[0]));
}

class LoginThemeElement {
  constructor(properties = {}) {
    Object.assign(this, properties);
    this.listeners = new Map();
    this.attributes = new Map();
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  fire(type, properties = {}) {
    const event = { isTrusted: true, preventDefault() {}, ...properties };
    for (const listener of this.listeners.get(type) || []) listener(event);
  }

  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  getAttribute(name) { return this.attributes.get(name); }
}

async function loginPasswordControls(initialPassword = '') {
  const source = await read('deploy/keycloak/themes/pinvou/login/resources/js/pinvou-login.js');
  const password = new LoginThemeElement({
    type: 'password', value: initialPassword,
    selectionStart: initialPassword.length, selectionEnd: initialPassword.length,
  });
  const eye = new LoginThemeElement({ hidden: true, disabled: true });
  const revealOutput = new LoginThemeElement({ hidden: true, textContent: '' });
  const form = new LoginThemeElement();
  form.querySelectorAll = () => [];
  form.querySelector = selector => new Map([
    ['[data-toggle-password]', eye],
    ['[data-password-reveal-output]', revealOutput],
  ]).get(selector) || null;
  const elements = new Map([
    ['pv-auth-form', form],
    ['pv-intent', new LoginThemeElement({ value: 'submit' })],
    ['pv-target-mode', new LoginThemeElement({ value: '' })],
    ['pv-device-id', new LoginThemeElement({ value: '' })],
    ['pv-password', password],
  ]);
  const storage = new Map();
  const animationFrames = [];
  const microtasks = [];
  const timers = [];
  vm.runInNewContext(source, {
    document: { getElementById: id => elements.get(id) || null },
    localStorage: {
      getItem: key => storage.get(key) || null,
      setItem: (key, value) => storage.set(key, value),
      removeItem: key => storage.delete(key),
    },
    crypto: { randomUUID: () => 'fixture-device' },
    Math, Number, Set, String,
    queueMicrotask: callback => { microtasks.push(callback); },
    requestAnimationFrame: callback => { animationFrames.push(callback); return animationFrames.length; },
    setTimeout: callback => { timers.push(callback); return timers.length; },
  });
  return {
    form, password, eye, revealOutput,
    runNextAnimationFrame() { animationFrames.shift()?.(); },
    prependMicrotask(callback) { microtasks.unshift(callback); },
    runMicrotasks() { while (microtasks.length) microtasks.shift()(); },
    runTimers() { while (timers.length) timers.shift()(); },
  };
}

test('web login is rendered by the Identity boundary and Portal redirects guests into OIDC', async () => {
  const [authenticator, template, portal] = await Promise.all([
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/PinvouBrowserAuthenticator.java'),
    read('deploy/keycloak/themes/pinvou/login/pinvou-login.ftl'),
    read('../pinvou3-app/src/features/account-platform/AccountPlatformView.jsx'),
  ]);
  assert.match(portal, /if \(portal && auth === 'guest'\) \{\s*globalThis\.location\.replace\(portalLoginLocation\(globalThis\.location\?\.pathname \|\| '\/', globalThis\.location\?\.search \|\| ''\)\);/);
  assert.match(authenticator, /CLIENT\.precheckPassword\(password\)/);
  assert.match(authenticator, /CLIENT\.createCaptcha\(mobile, purpose\(mode\)\)/);
  assert.match(authenticator, /CLIENT\.sendSMS\(/);
  assert.match(authenticator, /CLIENT\.verifySMS\(/);
  assert.match(authenticator, /CLIENT\.register\(/);
  assert.match(authenticator, /!"COMMITTED"\.equals\(status\)/);
  assert.match(authenticator, /result\.get\("user_id"\)/);
  assert.match(authenticator, /put\(context, "password_handle", handle\)/);
  assert.doesNotMatch(authenticator, /put\(context, "password",/);
  assert.match(template, /name="mode"/);
  assert.match(template, /data-mode="password"/);
  assert.match(template, /data-mode="sms"/);
  assert.match(template, /data-mode="register"/);
  assert.match(template, /name="sms_code"/);
  assert.match(template, /name="captcha_offset"/);
  assert.doesNotMatch(template, /type="hidden"[^>]+name="password"/);
  assert.doesNotMatch(template, /\?html/, 'Keycloak 26 auto-escapes HTML templates and rejects legacy ?html escaping');
});

test('saved browser passwords stay masked while passwords typed from empty can be revealed', async () => {
  const [template, styles, script] = await Promise.all([
    read('deploy/keycloak/themes/pinvou/login/pinvou-login.ftl'),
    read('deploy/keycloak/themes/pinvou/login/resources/css/pinvou-login.css'),
    read('deploy/keycloak/themes/pinvou/login/resources/js/pinvou-login.js'),
  ]);
  assert.match(template, /data-toggle-password[^>]+aria-pressed="false" disabled hidden/);
  assert.match(template, /<input id="pv-password" name="password" type="password"[^>]*>\s*<span class="pv-password-reveal-output" data-password-reveal-output role="status" aria-live="polite" aria-atomic="true" aria-hidden="true" hidden><\/span>/);
  assert.doesNotMatch(template, /data-password-reveal-output[^>]+name=/);
  assert.match(styles, /input::\-ms-reveal, \.pv-password-wrap input::\-ms-clear \{ display: none; \}/);
  assert.match(styles, /\.pv-password-reveal-output \{[^}]+pointer-events: none;[^}]+user-select: none;/);
  assert.doesNotMatch(script, /password\.type\s*=/, 'the named password input must stay type=password');

  const typed = await loginPasswordControls();
  typed.password.fire('beforeinput', { inputType: 'insertText', data: 'freshly-typed-secret' });
  typed.password.value = 'freshly-typed-secret';
  typed.password.selectionStart = typed.password.selectionEnd = typed.password.value.length;
  typed.password.fire('input', { inputType: 'insertText', data: 'freshly-typed-secret' });
  assert.equal(typed.eye.hidden, false);
  assert.equal(typed.eye.disabled, false);
  typed.eye.fire('click');
  assert.equal(typed.password.type, 'password', 'the submitted credential control must never become a text input');
  assert.equal(typed.revealOutput.hidden, false);
  assert.equal(typed.revealOutput.textContent, 'freshly-typed-secret');
  assert.equal(typed.revealOutput.getAttribute('aria-hidden'), 'false');
  typed.eye.fire('click');
  assert.equal(typed.password.type, 'password');
  assert.equal(typed.revealOutput.hidden, true);
  assert.equal(typed.revealOutput.textContent, '');
  assert.equal(typed.revealOutput.getAttribute('aria-hidden'), 'true');
  typed.eye.fire('click');
  assert.equal(typed.revealOutput.hidden, false);
  typed.form.fire('submit');
  assert.equal(typed.password.type, 'password');
  assert.equal(typed.revealOutput.hidden, true);
  assert.equal(typed.revealOutput.textContent, '');
  assert.equal(typed.revealOutput.getAttribute('aria-hidden'), 'true');
  assert.equal(typed.eye.getAttribute('aria-pressed'), 'false');

  const pasted = await loginPasswordControls();
  pasted.password.fire('paste', { clipboardData: { getData: () => 'pasted-secret' } });
  pasted.password.fire('beforeinput', { inputType: 'insertFromPaste', data: null });
  pasted.password.value = 'pasted-secret';
  pasted.password.selectionStart = pasted.password.selectionEnd = pasted.password.value.length;
  pasted.password.fire('input', { inputType: 'insertFromPaste', data: null });
  assert.equal(pasted.eye.hidden, false, 'a trusted paste from empty remains revealable');
  pasted.eye.fire('click');
  assert.equal(pasted.revealOutput.textContent, 'pasted-secret');

  const saved = await loginPasswordControls('saved-browser-secret');
  assert.equal(saved.password.type, 'password');
  assert.equal(saved.eye.hidden, true);
  assert.equal(saved.eye.disabled, true);
  saved.eye.fire('click');
  assert.equal(saved.password.type, 'password');

  saved.password.fire('beforeinput', { inputType: 'insertText', data: '!' });
  saved.password.value += '!';
  saved.password.selectionStart = saved.password.selectionEnd = saved.password.value.length;
  saved.password.fire('input', { inputType: 'insertText', data: '!' });
  assert.equal(saved.eye.hidden, true, 'editing an autofilled value must not reveal the saved prefix');
  saved.password.selectionStart = 0;
  saved.password.selectionEnd = saved.password.value.length;
  saved.password.fire('beforeinput', { inputType: 'deleteByCut', data: null });
  saved.password.value = '';
  saved.password.selectionStart = saved.password.selectionEnd = 0;
  saved.password.fire('input', { inputType: 'deleteByCut', data: null });
  saved.password.fire('beforeinput', { inputType: 'insertFromPaste', data: 'fresh-manual-secret' });
  saved.password.value = 'fresh-manual-secret';
  saved.password.selectionStart = saved.password.selectionEnd = saved.password.value.length;
  saved.password.fire('input', { inputType: 'insertFromPaste', data: 'fresh-manual-secret' });
  assert.equal(saved.eye.hidden, false);
  assert.equal(saved.eye.disabled, false);
  saved.eye.fire('click');
  assert.equal(saved.password.type, 'password');
  assert.equal(saved.revealOutput.textContent, 'fresh-manual-secret');
  assert.equal(saved.eye.getAttribute('aria-pressed'), 'true');

  saved.password.value = 'password-manager-replacement';
  assert.equal(saved.password.type, 'password', 'a silent replacement must remain masked in the real input');
  assert.equal(saved.revealOutput.textContent, 'fresh-manual-secret', 'a silent replacement must not reach the plaintext mirror');
  assert.notEqual(saved.revealOutput.textContent, saved.password.value);
  saved.runNextAnimationFrame();
  assert.equal(saved.password.type, 'password');
  assert.equal(saved.eye.hidden, true);
  assert.equal(saved.revealOutput.hidden, true);
  assert.equal(saved.revealOutput.textContent, '');

  const scripted = await loginPasswordControls();
  scripted.password.value = 'scripted-secret';
  scripted.password.fire('input', { isTrusted: false, inputType: 'insertText' });
  assert.equal(scripted.eye.hidden, true);
  assert.equal(scripted.password.type, 'password');

  const unknown = await loginPasswordControls();
  unknown.password.fire('beforeinput', { inputType: 'insertReplacementText' });
  unknown.password.value = 'unknown-event-secret';
  unknown.password.fire('input', { inputType: 'insertReplacementText' });
  assert.equal(unknown.eye.hidden, true, 'unknown input events must fail closed');
  assert.equal(unknown.revealOutput.textContent, '');

  const blurred = await loginPasswordControls();
  blurred.password.fire('beforeinput', { inputType: 'insertText', data: 'blurred-secret' });
  blurred.password.value = 'blurred-secret';
  blurred.password.selectionStart = blurred.password.selectionEnd = blurred.password.value.length;
  blurred.password.fire('input', { inputType: 'insertText', data: 'blurred-secret' });
  blurred.eye.fire('click');
  blurred.password.fire('blur', { relatedTarget: null });
  assert.equal(blurred.password.type, 'password');
  assert.equal(blurred.revealOutput.hidden, true);
  assert.equal(blurred.revealOutput.textContent, '');

  const expiredIntent = await loginPasswordControls();
  expiredIntent.password.fire('beforeinput', { inputType: 'insertText', data: 'at-maxlength' });
  expiredIntent.password.value = 'at-maxlength';
  expiredIntent.password.selectionStart = expiredIntent.password.selectionEnd = expiredIntent.password.value.length;
  expiredIntent.password.fire('input', { inputType: 'insertText', data: 'at-maxlength' });
  assert.equal(expiredIntent.eye.hidden, false);
  expiredIntent.password.fire('beforeinput', { inputType: 'insertText', data: '!' });
  expiredIntent.runTimers();
  expiredIntent.password.value = 'unpaired-later-mutation';
  expiredIntent.password.selectionStart = expiredIntent.password.selectionEnd = expiredIntent.password.value.length;
  expiredIntent.password.fire('input', { inputType: 'insertText' });
  assert.equal(expiredIntent.eye.hidden, true, 'an unmatched beforeinput must not authorize a later input event');
  assert.equal(expiredIntent.revealOutput.textContent, '');

  const nestedMutation = await loginPasswordControls();
  nestedMutation.password.fire('beforeinput', { inputType: 'insertText', data: 'manual-at-limit' });
  nestedMutation.password.value = 'manual-at-limit';
  nestedMutation.password.selectionStart = nestedMutation.password.selectionEnd = nestedMutation.password.value.length;
  nestedMutation.password.fire('input', { inputType: 'insertText', data: 'manual-at-limit' });
  nestedMutation.password.fire('beforeinput', { inputType: 'insertText', data: '!' });
  nestedMutation.prependMicrotask(() => {
    nestedMutation.password.fire('beforeinput', { inputType: 'insertText', data: 'saved-manager-secret' });
    nestedMutation.password.value = 'saved-manager-secret';
    nestedMutation.password.selectionStart = nestedMutation.password.selectionEnd = nestedMutation.password.value.length;
    nestedMutation.password.fire('input', { inputType: 'insertText', data: 'saved-manager-secret' });
  });
  nestedMutation.runMicrotasks();
  nestedMutation.runTimers();
  assert.equal(nestedMutation.eye.hidden, true, 'a nested trusted-looking mutation must not consume an unmatched edit authorization');
  assert.equal(nestedMutation.revealOutput.textContent, '');

  const reset = await loginPasswordControls();
  reset.password.fire('beforeinput', { inputType: 'insertText', data: 'before-reset' });
  reset.password.value = 'before-reset';
  reset.password.selectionStart = reset.password.selectionEnd = reset.password.value.length;
  reset.password.fire('input', { inputType: 'insertText', data: 'before-reset' });
  reset.eye.fire('click');
  reset.form.fire('reset');
  reset.password.value = '';
  reset.password.selectionStart = reset.password.selectionEnd = 0;
  reset.runTimers();
  assert.equal(reset.revealOutput.textContent, '');
  reset.password.fire('beforeinput', { inputType: 'insertText', data: 'after-reset' });
  reset.password.value = 'after-reset';
  reset.password.selectionStart = reset.password.selectionEnd = reset.password.value.length;
  reset.password.fire('input', { inputType: 'insertText', data: 'after-reset' });
  assert.equal(reset.eye.hidden, false, 'manual input after a default reset to empty must be revealable');

  const keyboard = await loginPasswordControls();
  keyboard.password.fire('beforeinput', { inputType: 'insertText', data: 'keyboard-secret' });
  keyboard.password.value = 'keyboard-secret';
  keyboard.password.selectionStart = keyboard.password.selectionEnd = keyboard.password.value.length;
  keyboard.password.fire('input', { inputType: 'insertText', data: 'keyboard-secret' });
  keyboard.password.fire('blur', { relatedTarget: keyboard.eye });
  keyboard.eye.fire('click', { detail: 0 });
  assert.equal(keyboard.password.type, 'password');
  assert.equal(keyboard.revealOutput.textContent, 'keyboard-secret');
  assert.equal(keyboard.revealOutput.getAttribute('aria-hidden'), 'false', 'keyboard reveal must enter the accessibility tree');
  keyboard.eye.fire('blur', { relatedTarget: null });
  assert.equal(keyboard.revealOutput.textContent, '');
  assert.equal(keyboard.revealOutput.getAttribute('aria-hidden'), 'true');
});

test('native Chromium keeps manual password editing revealable without accepting a blocked-edit race', async t => {
  const executablePath = nativeChromium();
  const puppeteer = nativePuppeteer();
  if (!executablePath || !puppeteer) {
    if (process.env.PINVOU_REQUIRE_PASSWORD_BROWSER_TEST === '1') {
      assert.fail(`required native password browser test is unavailable: chromium=${Boolean(executablePath)} puppeteer=${Boolean(puppeteer)}`);
    }
    t.skip('Chromium executable or puppeteer-core is unavailable');
    return;
  }

  const source = (await read('deploy/keycloak/themes/pinvou/login/resources/js/pinvou-login.js'))
    .replaceAll('</script', '<\\/script');
  const browser = await puppeteer.launch({
    executablePath, headless: true, timeout: 15000, protocolTimeout: 15000,
    args: ['--disable-gpu', '--no-first-run', '--no-sandbox'],
  });
  try {
    const page = await browser.newPage();
    const mount = async (extraScript = '') => {
      await page.setContent(`<!doctype html><form id="pv-auth-form">
        <input id="pv-intent" value="submit"><input id="pv-target-mode"><input id="pv-device-id">
        <span class="pv-password-wrap"><input id="pv-password" name="password" type="password" maxlength="128">
        <span data-password-reveal-output aria-hidden="true" hidden></span>
        <button type="button" data-toggle-password disabled hidden>show</button></span>
        <button id="copy-fixture" type="button">copy</button>
      </form><script>${source}</script><script>
        document.getElementById('copy-fixture').addEventListener('click', () => {
          const copy = document.createElement('textarea'); copy.value = 'pasted-secret';
          document.body.append(copy); copy.select(); document.execCommand('copy'); copy.remove();
        });
        ${extraScript}
      </script>`);
    };
    const state = () => page.evaluate(() => ({
      value: document.getElementById('pv-password').value,
      type: document.getElementById('pv-password').type,
      eyeHidden: document.querySelector('[data-toggle-password]').hidden,
      reveal: document.querySelector('[data-password-reveal-output]').textContent,
    }));

    await mount();
    await page.focus('#pv-password');
    await page.keyboard.type('typed-secret');
    assert.equal((await state()).eyeHidden, false, 'native typing must make the reveal control available');
    await page.click('[data-toggle-password]');
    assert.deepEqual(await state(), { value: 'typed-secret', type: 'password', eyeHidden: false, reveal: 'typed-secret' });
    await page.focus('#pv-password');
    await page.keyboard.press('Backspace');
    await page.evaluate(() => document.getElementById('pv-password').setSelectionRange(0, 5));
    await page.keyboard.type('new');
    assert.equal((await state()).eyeHidden, false, 'native deletion and selection replacement must stay revealable');

    await mount();
    await page.click('#copy-fixture');
    await page.focus('#pv-password');
    const pasteModifier = process.platform === 'darwin' ? 'Meta' : 'Control';
    await page.keyboard.down(pasteModifier);
    await page.keyboard.press('KeyV');
    await page.keyboard.up(pasteModifier);
    assert.deepEqual(await state(), { value: 'pasted-secret', type: 'password', eyeHidden: false, reveal: '' });

    await mount(`
      window.__passwordEvents = [];
      for (const type of ['beforeinput', 'input', 'compositionstart', 'compositionupdate', 'compositionend']) {
        document.getElementById('pv-password').addEventListener(type, event => window.__passwordEvents.push({
          type, inputType: event.inputType, data: event.data, trusted: event.isTrusted,
          value: event.target.value, start: event.target.selectionStart, end: event.target.selectionEnd,
        }));
      }
    `);
    await page.focus('#pv-password');
    const cdp = await page.createCDPSession();
    await cdp.send('Input.insertText', { text: 'A' });
    await cdp.send('Input.insertText', { text: '🔐' });
    await cdp.send('Input.insertText', { text: 'B' });
    await cdp.detach();
    const unicodeState = await state();
    const unicodeEvents = await page.evaluate(() => window.__passwordEvents);
    assert.deepEqual(unicodeState, { value: 'A🔐B', type: 'password', eyeHidden: false, reveal: '' },
      `native A→emoji→B insertion must stay revealable: ${JSON.stringify(unicodeEvents)}`);
    await page.keyboard.press('Backspace');
    assert.deepEqual(await state(), { value: 'A🔐', type: 'password', eyeHidden: false, reveal: '' },
      'native deletion after an emoji must stay revealable');
    await page.evaluate(() => document.getElementById('pv-password').setSelectionRange(1, 2));
    await page.keyboard.type('Z');
    assert.deepEqual(await state(), { value: 'AZ', type: 'password', eyeHidden: false, reveal: '' },
      'native Unicode selection replacement must stay revealable');

    await mount(`
      window.__imeEvents = [];
      for (const type of ['beforeinput', 'input', 'compositionstart', 'compositionupdate', 'compositionend']) {
        document.getElementById('pv-password').addEventListener(type, event => window.__imeEvents.push({
          type, inputType: event.inputType, data: event.data, trusted: event.isTrusted,
          value: event.target.value, start: event.target.selectionStart, end: event.target.selectionEnd,
        }));
      }
    `);
    await page.focus('#pv-password');
    const ime = await page.createCDPSession();
    await ime.send('Input.imeSetComposition', {
      text: '拼音', selectionStart: 2, selectionEnd: 2, replacementStart: 0, replacementEnd: 0,
    });
    await ime.send('Input.insertText', { text: '拼音' });
    await ime.detach();
    const imeState = await state();
    const imeEvents = await page.evaluate(() => window.__imeEvents);
    assert.equal(imeState.eyeHidden, false, `native IME composition must stay revealable: ${JSON.stringify(imeEvents)}`);

    await mount();
    await page.focus('#pv-password');
    await page.keyboard.type('before-reset');
    await page.evaluate(() => document.getElementById('pv-auth-form').reset());
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 0)));
    await page.focus('#pv-password');
    await page.keyboard.type('after-reset');
    assert.equal((await state()).eyeHidden, false, 'native reset must reclassify the default empty value before later input');

    await mount(`
      document.getElementById('pv-password').addEventListener('beforeinput', event => {
        if (event.data !== '!') return;
        queueMicrotask(() => {
          const password = document.getElementById('pv-password'); password.select();
          document.execCommand('insertText', false, 'saved-secret');
        });
      });
    `);
    await page.focus('#pv-password');
    await page.keyboard.type('manual-at-limit');
    await page.evaluate(() => {
      const password = document.getElementById('pv-password'); password.maxLength = password.value.length;
    });
    await page.keyboard.type('!');
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 0)));
    const raced = await state();
    assert.equal(raced.type, 'password');
    assert.equal(raced.eyeHidden, true, 'a maxlength microtask injection must revoke revealability');
    assert.equal(raced.reveal, '');
  } finally {
    await browser.close();
  }
});

test('Portal login preserves local context in one encoded return target without replacing the OIDC boundary', () => {
  const origin = 'https://account.pinvou.com';
  for (const [pathname, search, expected] of [
    ['/admin.html', '?section=tokens&tab=usage&cursor=signed', '/admin.html?section=tokens&tab=usage&cursor=signed'],
    ['/', '?section=ordersInvoices&order_id=fixture', '/?section=ordersInvoices&order_id=fixture'],
    ['/admin.html', '?section=models&return_to=https://untrusted.example', '/admin.html?section=models&return_to=https://untrusted.example'],
    ['https://untrusted.example', '', '/'], ['//untrusted.example', '', '/'], ['bad', '', '/'],
    ['/\r\nLocation:invalid', '', '/'], [null, '', '/'], [['/admin.html'], '', '/'],
    ['/admin.html', '\r\n?section=models', '/admin.html'], ['/admin.html', 'section=models', '/admin.html'],
    ['/admin.html', '?section=models\nignored', '/admin.html'], ['/admin.html', null, '/admin.html'],
  ]) {
    const result = new URL(portalLoginLocation(pathname, search), origin);
    assert.equal(result.origin, origin); assert.equal(result.pathname, '/auth/login');
    assert.equal(result.hash, ''); assert.deepEqual([...result.searchParams.keys()], ['return_to']);
    assert.equal(result.searchParams.get('return_to'), expected);
  }
});

test('Keycloak browser flow keeps cookie SSO and requires the custom Identity image assets', async () => {
  const [realmSource, dockerfile, composeSource, helm] = await Promise.all([
    read('deploy/keycloak/pinvou-dev-realm.json'),
    read('deploy/keycloak/Dockerfile'),
    read('deploy/compose/compose.yaml'),
    read('deploy/helm/account-platform/templates/infrastructure.yaml'),
  ]);
  const realm = JSON.parse(realmSource);
  const browser = realm.authenticationFlows.find(flow => flow.alias === 'pinvou-browser');
  assert.equal(realm.browserFlow, 'pinvou-browser');
  assert.equal(realm.loginTheme, 'pinvou');
  assert.ok(browser.authenticationExecutions.some(item => item.authenticator === 'auth-cookie' && item.requirement === 'ALTERNATIVE'));
  assert.ok(browser.authenticationExecutions.some(item => item.authenticator === 'pinvou-browser-authenticator' && item.requirement === 'ALTERNATIVE'));
  assert.match(dockerfile, /COPY themes\/pinvou \/opt\/keycloak\/themes\/pinvou/);
  const compose = YAML.parse(composeSource);
  assert.equal(compose.services.keycloak.environment.PINVOU_ACCOUNT_AUTH_URL, 'http://account-service:8080');
  assert.equal(compose.services.keycloak.environment.PINVOU_IDENTITY_AUTH_URL, 'http://identity-service:8080');
  assert.match(helm, /PINVOU_ACCOUNT_AUTH_URL/);
  assert.match(helm, /PINVOU_IDENTITY_AUTH_URL/);
});

test('the Identity-hosted login theme has exact Simplified Chinese, English, and Japanese coverage', async () => {
  const [zh, en, ja, theme] = await Promise.all([
    read('deploy/keycloak/themes/pinvou/login/messages/messages_zh_CN.properties'),
    read('deploy/keycloak/themes/pinvou/login/messages/messages_en.properties'),
    read('deploy/keycloak/themes/pinvou/login/messages/messages_ja.properties'),
    read('deploy/keycloak/themes/pinvou/login/theme.properties'),
  ]);
  const expected = [...messageKeys(zh)].sort();
  assert.deepEqual([...messageKeys(en)].sort(), expected);
  assert.deepEqual([...messageKeys(ja)].sort(), expected);
  assert.match(theme, /locales=zh-CN,en,ja/);
});
