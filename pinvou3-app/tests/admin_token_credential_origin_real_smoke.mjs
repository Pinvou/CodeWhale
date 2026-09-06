import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import puppeteer from 'puppeteer-core';

const [portalOrigin, ...launchIDs] = process.argv.slice(2);
const browserPath = [process.env.CHROME, 'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium is required for the real credential-origin smoke');
assert.ok(portalOrigin && launchIDs.length === 5, 'portal origin and five launches are required');

const safeCredentialStage = (page) => page.evaluate(() => {
  const stages = { '/credential-handoff': 'HANDOFF', '/launch': 'LAUNCH', '/session': 'SESSION', '/authorize': 'AUTHORIZE', '/oidc/callback': 'CALLBACK' };
  let code = 'OMITTED';
  try {
    const value = JSON.parse(document.body.textContent);
    if (typeof value?.code === 'string' && /^(CREDENTIAL_|INVALID_CREDENTIAL_)[A-Z_]{1,60}$/.test(value.code)) code = value.code;
  } catch { /* HTML and arbitrary page content are intentionally omitted. */ }
  const failure = document.getElementById('credential-status');
  const language = document.documentElement.lang;
  return { stage: stages[location.pathname] ?? 'OTHER', authForm: Boolean(document.getElementById('credential-auth')), credentialForm: Boolean(document.getElementById('credential-form')), failureVisible: Boolean(failure && !failure.hidden), language: ['en', 'zh-CN', 'ja'].includes(language) ? language : 'OTHER', code };
});

const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
try {
  for (const launchID of launchIDs) {
    const context = await browser.createBrowserContext();
    const portal = await context.newPage();
    await portal.goto(`${portalOrigin}/start`, { waitUntil: 'domcontentloaded' });
    const popupPromise = new Promise((resolve) => portal.once('popup', resolve));
    const handoffPromise = portal.evaluate(async (opaqueLaunchID) => {
      const exact = (value, keys) => value && typeof value === 'object' && !Array.isArray(value)
        && Object.keys(value).sort().join(',') === [...keys].sort().join(',');
      const bytes = crypto.getRandomValues(new Uint8Array(32));
      const nonce = btoa(String.fromCharCode(...bytes)).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
      const popup = window.open('/credential-handoff', '_blank');
      if (!popup) throw new Error('handoff popup blocked');
      await new Promise((resolve, reject) => {
        let port = null;
        const timeout = setTimeout(() => reject(new Error('handoff timed out')), 8_000);
        const fail = (message) => { clearTimeout(timeout); port?.close(); reject(new Error(message)); };
        const listener = (event) => {
          if (event.source !== popup || event.origin !== location.origin || !exact(event.data, ['version', 'type']) || event.data.version !== 1 || event.data.type !== 'HANDOFF_READY') return;
          removeEventListener('message', listener);
          const channel = new MessageChannel();
          port = channel.port1;
          port.onmessageerror = () => fail('handoff message error');
          port.onmessage = (message) => {
            if (message.currentTarget !== port || !exact(message.data, ['version', 'type', 'nonce']) || message.data.version !== 1 || message.data.nonce !== nonce) {
              fail('handoff response invalid');
              return;
            }
            if (message.data.type === 'HANDOFF_BOUND') {
              port.postMessage({ version: 1, type: 'HANDOFF_SUBMIT', nonce, launch_id: opaqueLaunchID });
              opaqueLaunchID = '';
              return;
            }
            if (message.data.type !== 'HANDOFF_ACCEPTED') {
              fail('handoff response out of order');
              return;
            }
            clearTimeout(timeout);
            port.close();
            port = null;
            resolve();
          };
          port.start();
          popup.postMessage({ version: 1, type: 'HANDOFF_BIND', nonce }, location.origin, [channel.port2]);
        };
        addEventListener('message', listener);
      });
    }, launchID);
    const popup = await popupPromise;
    await handoffPromise;
    try {
      await popup.waitForSelector('input[name=secret]', { timeout: 10_000 });
    } catch (error) {
      // Report only a closed stage and form-presence flags. OIDC URLs, DOM,
      // cookie values and unknown paths must never enter test diagnostics.
      const stage = await safeCredentialStage(popup);
      throw new Error(`credential form unavailable: ${JSON.stringify(stage)}`, { cause: error });
    }
    assert.equal(await popup.evaluate(() => window.opener), null);
    assert.equal(portal.url(), `${portalOrigin}/start`);
    assert.equal(await portal.evaluate((privateID) => document.documentElement.outerHTML.includes(privateID) || location.href.includes(privateID) || performance.getEntries().some((entry) => entry.name.includes(privateID)), launchID), false);
    await popup.type('input[name=provider]', 'NEW_API');
    await popup.type('input[name=base_url]', 'https://provider.example');
    await popup.type('input[name=allowed_models]', 'model-a');
    await popup.type('input[name=secret]', `real-browser-secret-${launchID}`);
    try {
      await Promise.all([
        popup.waitForFunction(() => {
          const success = { en: 'Credential updated securely', 'zh-CN': '凭据已安全更新', ja: '認証情報を安全に更新しました' }[document.documentElement.lang];
          return Boolean(success && document.querySelector('h1')?.textContent === success && !document.getElementById('credential-form'));
        }, { timeout: 10_000 }),
        popup.click('button[type=submit]'),
      ]);
    } catch (error) {
      throw new Error(`credential submission unconfirmed: ${JSON.stringify(await safeCredentialStage(popup))}`, { cause: error });
    }
    assert.equal(await popup.evaluate(() => history.length > 0 && !location.href.includes('secret')), true);
    await context.close();
  }
} finally {
  await browser.close();
}
console.log('real CredentialOriginService + repository Chromium smoke passed 5 consecutive flows');
