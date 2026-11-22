import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { createRequire } from 'node:module';
import test from 'node:test';

const theme = new URL('../../deploy/keycloak/themes/pinvou/login/', import.meta.url);
const require = createRequire(new URL('../../../pinvou3-app/package.json', import.meta.url));
const mandatory = name => process.env[name] === '1';
const browserRequired = 'PINVOU_REQUIRE_PASSWORD_BROWSER_TEST';
const edgeRequired = 'PINVOU_REQUIRE_PASSWORD_EDGE_VISUAL_TEST';
function assets() {
  const properties = Object.fromEntries(readFileSync(new URL('theme.properties', theme), 'utf8').trim().split(/\r?\n/)
    .filter(line => line && !line.startsWith('#')).map(line => { const at = line.indexOf('='); return [line.slice(0, at), line.slice(at + 1)]; }));
  const selected = (property, directory, extension) => {
    const path = properties[property];
    assert.match(path, new RegExp(`^${directory}/pinvou-login[.]([0-9a-f]{64})[.]${extension}$`));
    const bytes = readFileSync(new URL(`resources/${path}`, theme));
    assert.equal(createHash('sha256').update(bytes).digest('hex'), path.match(/[.]([0-9a-f]{64})[.]/)[1]);
    return bytes.toString('utf8');
  };
  const source = selected('scripts', 'js', 'js'), styles = selected('styles', 'css', 'css');
  const ftl = readFileSync(new URL('pinvou-login.ftl', theme), 'utf8');
  const match = ftl.match(/<span class="pv-password-wrap">[\s\S]*?<\/button>\s*<\/span>/);
  assert.ok(match, 'current FTL password controls must exist');
  assert.match(match[0], /name="password" type="password" autocomplete="current-password"/);
  assert.match(match[0], /data-password-reveal-output[^>]*aria-hidden="true" hidden/);
  assert.match(match[0], /data-toggle-password[^>]*disabled hidden/);
  // This projects the actual password controls, not FreeMarker/server execution.
  const controls = match[0].replace(/\$\{msg\('[A-Za-z]+'\)\}/g, 'public fixture label');
  assert.ok(!controls.includes('${'), 'unresolved FTL expression in browser fixture');
  return { source, styles, controls };
}
function executable(edgeOnly) {
  const override = process.env.PINVOU_PASSWORD_BROWSER_EXECUTABLE;
  const candidates = override ? [override] : [
    'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
    'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
    ...(!edgeOnly ? [process.env.CHROME, '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome'] : []),
  ].filter(Boolean);
  return candidates.find(path => existsSync(path) && (!edgeOnly || basename(path).toLowerCase() === 'msedge.exe'));
}
async function session(t, edgeOnly, action) {
  const path = executable(edgeOnly); let puppeteer;
  try { puppeteer = require('puppeteer-core'); } catch { /* optional outside explicit browser gate */ }
  if (!path || !puppeteer) {
    if (mandatory(edgeOnly ? edgeRequired : browserRequired)) assert.fail('required login-theme browser or puppeteer-core is unavailable');
    t.skip('explicit browser test dependency is unavailable'); return;
  }
  const profile = mkdtempSync(join(tmpdir(), 'pinvou-public-theme-browser-'));
  let browser;
  try {
    browser = await puppeteer.launch({ executablePath: path, userDataDir: profile, headless: true,
      timeout: 15000, protocolTimeout: 15000, args: ['--disable-gpu', '--no-first-run', '--no-default-browser-check',
        '--disable-background-networking', '--disable-component-update', '--metrics-recording-only'] });
    const page = await browser.newPage(); await page.setViewport({ width: 800, height: 600, deviceScaleFactor: 1 });
    await page.setRequestInterception(true); let requests = 0;
    page.on('request', request => { requests++; void request.abort(); });
    const a = assets();
    const mount = async (initial = '', extra = '') => {
      const html = `<!doctype html><style>${a.styles.replaceAll('</style', '<\\/style')}
        .pv-password-wrap{width:420px;margin:30px}.pv-password-wrap input{width:100%;height:44px}</style>
        <form id="pv-auth-form"><input id="pv-intent" value="submit"><input id="pv-target-mode"><input id="pv-device-id">
        ${a.controls}</form><script>document.getElementById('pv-password').value=${JSON.stringify(initial)};</script>
        <script>${a.source.replaceAll('</script', '<\\/script')}</script><script>${extra}</script>`;
      await page.setContent(html); await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => resolve())));
    };
    const state = () => page.evaluate(() => ({ value: document.getElementById('pv-password').value,
      type: document.getElementById('pv-password').type, eyeHidden: document.querySelector('[data-toggle-password]').hidden,
      reveal: document.querySelector('[data-password-reveal-output]').textContent,
      ariaHidden: document.querySelector('[data-password-reveal-output]').getAttribute('aria-hidden') }));
    await action({ page, mount, state }); assert.equal(requests, 0, 'fixture must make no page network requests');
  } finally {
    try { if (browser) await browser.close(); } finally {
      assert.equal(dirname(resolve(profile)), resolve(tmpdir()));
      assert.ok(basename(profile).startsWith('pinvou-public-theme-browser-'));
      rmSync(profile, { recursive: true, force: true });
    }
  }
}

test('current Keycloak theme selected asset hashes and projected FTL controls agree', () => {
  const a = assets(); assert.match(a.styles, /::-ms-reveal/); assert.match(a.styles, /::-ms-clear/);
});

test('native browser executes current Keycloak theme password protections', async t => session(t, false, async ({ page, mount, state }) => {
  await t.test('manual typing uses one explicit mirror and blur clears accessible text', async () => {
    await mount(); await page.focus('#pv-password'); await page.keyboard.type('public-typed-value');
    assert.equal((await state()).eyeHidden, false); await page.click('[data-toggle-password]');
    assert.deepEqual(await state(), { value: 'public-typed-value', type: 'password', eyeHidden: false,
      reveal: 'public-typed-value', ariaHidden: 'false' });
    assert.equal(await page.evaluate(() => Array.from(document.querySelectorAll('.pv-password-wrap > [data-toggle-password]'))
      .filter(control => getComputedStyle(control).display !== 'none').length), 1);
    await page.focus('#pv-intent'); await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 0)));
    assert.equal((await state()).reveal, ''); assert.equal((await state()).ariaHidden, 'true');
  });
  await t.test('initial saved or script-replaced values remain protected', async () => {
    await mount('public-saved-value'); assert.equal((await state()).eyeHidden, true); assert.equal((await state()).reveal, '');
    await mount(); await page.focus('#pv-password'); await page.keyboard.type('public-manual'); await page.click('[data-toggle-password]');
    await page.evaluate(() => { document.getElementById('pv-password').value = 'public-unknown-replacement'; });
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal((await state()).eyeHidden, true); assert.equal((await state()).reveal, ''); assert.equal((await state()).type, 'password');
  });
  await t.test('native Unicode editing and IME composition preserve manual provenance', async () => {
    await mount(); await page.focus('#pv-password'); const cdp = await page.createCDPSession();
    try { for (const text of ['A', '🔐', 'B']) await cdp.send('Input.insertText', { text }); }
    finally { await cdp.detach(); }
    assert.equal((await state()).value, 'A🔐B'); assert.equal((await state()).eyeHidden, false);
    await page.keyboard.press('Backspace'); assert.equal((await state()).value, 'A🔐');
    await page.evaluate(() => document.getElementById('pv-password').setSelectionRange(1, 2)); await page.keyboard.type('Z');
    assert.equal((await state()).value, 'AZ'); assert.equal((await state()).eyeHidden, false);
    await mount(); await page.focus('#pv-password'); const ime = await page.createCDPSession();
    try { await ime.send('Input.imeSetComposition', { text: '拼音', selectionStart: 2, selectionEnd: 2, replacementStart: 0, replacementEnd: 0 });
      await ime.send('Input.insertText', { text: '拼音' }); } finally { await ime.detach(); }
    assert.equal((await state()).value, '拼音'); assert.equal((await state()).eyeHidden, false);
  });
  await t.test('form reset retires old reveal state before new manual input', async () => {
    await mount(); await page.focus('#pv-password'); await page.keyboard.type('public-before-reset'); await page.click('[data-toggle-password]');
    await page.evaluate(() => document.getElementById('pv-auth-form').reset());
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 0))); assert.equal((await state()).reveal, '');
    await page.focus('#pv-password'); await page.keyboard.type('public-after-reset'); assert.equal((await state()).eyeHidden, false);
  });
  await t.test('maxlength nested microtask edit cannot reuse manual authorization', async () => {
    await mount('', `document.getElementById('pv-password').addEventListener('beforeinput', event => {
      if(event.data !== '!') return; queueMicrotask(() => {const p=document.getElementById('pv-password');p.select();document.execCommand('insertText',false,'public-injected-value');});});`);
    await page.focus('#pv-password'); await page.keyboard.type('public-at-limit');
    await page.evaluate(() => {const p=document.getElementById('pv-password');p.maxLength=p.value.length;}); await page.keyboard.type('!');
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 0)));
    assert.equal((await state()).eyeHidden, true); assert.equal((await state()).reveal, ''); assert.equal((await state()).type, 'password');
  });
}));

test('Microsoft Edge displays only the current custom password reveal icon', async t => session(t, true, async ({ page, mount }) => {
  await mount(); await page.focus('#pv-password'); await page.keyboard.type('public-visual-value');
  const clip = await page.$eval('.pv-password-wrap', element => {const box=element.getBoundingClientRect();return {x:box.right-96,y:box.top,width:96,height:box.height};});
  // Password text and caret lie outside this crop. No screenshot is written to disk.
  const masked = await page.screenshot({ clip });
  await page.$eval('#pv-password', input => {input.type='text';});
  const text = await page.screenshot({ clip }); assert.deepEqual(masked, text, 'Edge native reveal icon must remain suppressed');
}));
