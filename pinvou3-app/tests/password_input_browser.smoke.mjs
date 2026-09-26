import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { createServer as createHTTPServer } from 'node:http';
import { tmpdir } from 'node:os';
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import { build } from 'vite';
import react from '@vitejs/plugin-react';

const candidates = process.platform === 'win32' ? [
  process.env.CHROME,
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
] : process.platform === 'darwin' ? [
  process.env.CHROME,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
] : [
  process.env.CHROME,
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
];
const executablePath = candidates.find(candidate => candidate && path.isAbsolute(candidate) && existsSync(candidate));
assert.ok(executablePath, 'real Chrome or Edge is required for the password input security test');
process.stdout.write(`password browser: ${executablePath}\n`);

const projectRoot = path.resolve(import.meta.dirname, '..');
const fixtureRoot = await mkdtemp(path.join(tmpdir(), 'pinvou-password-browser-'));
process.stdout.write('password browser: building Vite fixture\n');
await build({
  root: projectRoot,
  configFile: false,
  plugins: [react()],
  logLevel: 'error',
  build: {
    outDir: fixtureRoot,
    emptyOutDir: true,
    minify: false,
    sourcemap: 'inline',
    rollupOptions: { input: path.join(projectRoot, 'tests/fixtures/password_input_browser.html') },
  },
});
const contentTypes = new Map([['.html', 'text/html'], ['.js', 'text/javascript'], ['.css', 'text/css']]);
const server = createHTTPServer(async (request, response) => {
  try {
    const requestPath = new URL(request.url, 'http://127.0.0.1').pathname;
    const relative = requestPath === '/' ? 'tests/fixtures/password_input_browser.html' : requestPath.slice(1);
    const target = path.resolve(fixtureRoot, relative);
    assert.ok(target.startsWith(`${path.resolve(fixtureRoot)}${path.sep}`));
    const body = await readFile(target);
    response.writeHead(200, { 'content-type': contentTypes.get(path.extname(target)) || 'application/octet-stream' });
    response.end(body);
  } catch {
    process.stderr.write(`password browser fixture 404: ${request.url}\n`);
    response.writeHead(404);
    response.end('not found');
  }
});
await new Promise((resolve, reject) => {
  server.once('error', reject);
  server.listen(0, '127.0.0.1', resolve);
});
const address = server.address();
assert.equal(typeof address, 'object');
process.stdout.write(`password browser: fixture listening on ${address.port}\n`);

const browser = await puppeteer.launch({
  executablePath,
  headless: true,
  timeout: 30000,
  protocolTimeout: 60000,
  args: ['--disable-gpu', '--no-first-run', '--no-sandbox'],
});
process.stdout.write('password browser: Chromium launched\n');

try {
  const page = await browser.newPage();
  page.on('console', message => process.stderr.write(`password browser console: ${message.type()} ${message.text()}\n`));
  page.on('pageerror', error => process.stderr.write(`password browser page error: ${error.stack || error.message}\n`));
  page.on('requestfailed', request => process.stderr.write(`password browser request failed: ${request.url()} ${request.failure()?.errorText || ''}\n`));
  await page.goto(`http://127.0.0.1:${address.port}/tests/fixtures/password_input_browser.html`, {
    waitUntil: 'domcontentloaded', timeout: 15000,
  });
  process.stdout.write('password browser: fixture loaded\n');
  const selector = '[data-password-input] input';
  const outputSelector = '[data-password-reveal-output]';
  const eyeSelector = '[data-password-input] button';
  await page.waitForSelector(selector, { timeout: 15000 });
  const state = () => page.evaluate((inputSelector, revealSelector, buttonSelector) => {
    const input = document.querySelector(inputSelector);
    const output = document.querySelector(revealSelector);
    const eye = document.querySelector(buttonSelector);
    const inputStyle = getComputedStyle(input);
    const outputStyle = getComputedStyle(output);
    return {
      value: input.value,
      type: input.type,
      active: document.activeElement === input,
      selectionStart: input.selectionStart,
      selectionEnd: input.selectionEnd,
      eyeHidden: eye.hidden,
      eyePressed: eye.getAttribute('aria-pressed'),
      eyeLabel: eye.getAttribute('aria-label'),
      reveal: output.textContent,
      revealHidden: output.hidden,
      revealAriaHidden: output.getAttribute('aria-hidden'),
      revealRole: output.getAttribute('role'),
      revealName: output.getAttribute('name'),
      pointerEvents: outputStyle.pointerEvents,
      inputZ: Number(inputStyle.zIndex),
      outputZ: Number(outputStyle.zIndex),
      background: inputStyle.backgroundColor,
      textFill: inputStyle.webkitTextFillColor,
      caretColor: inputStyle.caretColor,
    };
  }, selector, outputSelector, eyeSelector);
  const reset = () => page.evaluate(() => {
    globalThis.__passwordFixture.setValue('');
    return globalThis.__passwordFixture.reset();
  });

  await page.focus(selector);
  await page.keyboard.type('typed-secret');
  let current = await state();
  assert.equal(current.eyeHidden, false, `trusted typing must enable reveal: ${JSON.stringify(await page.evaluate(() => globalThis.__passwordEvents))}`);
  await page.click(eyeSelector);
  current = await state();
  assert.equal(current.type, 'password');
  assert.equal(current.reveal, 'typed-secret');
  assert.equal(current.active, true, 'mouse reveal keeps the real credential input focused');
  assert.equal(current.revealRole, 'status');
  assert.equal(current.revealAriaHidden, 'false');
  assert.equal(current.revealName, null);
  assert.equal(current.eyePressed, 'true');
  assert.equal(current.eyeLabel, 'Hide password');
  assert.equal(current.pointerEvents, 'none');
  assert.ok(current.inputZ > current.outputZ, 'native caret and selection must paint above the plaintext layer');
  assert.equal(current.background, 'rgba(0, 0, 0, 0)');
  assert.equal(current.textFill, 'rgba(0, 0, 0, 0)');
  assert.notEqual(current.caretColor, 'rgba(0, 0, 0, 0)');

  await page.keyboard.press('Home');
  await page.keyboard.down('Shift');
  await page.keyboard.press('ArrowRight');
  await page.keyboard.press('ArrowRight');
  await page.keyboard.press('ArrowRight');
  await page.keyboard.up('Shift');
  current = await state();
  assert.deepEqual([current.selectionStart, current.selectionEnd], [0, 3], 'native Shift+Arrow selection remains available while revealed');
  assert.equal(current.reveal, 'typed-secret');

  const clickPoint = await page.evaluate((inputSelector) => {
    const input = document.querySelector(inputSelector);
    const bounds = input.getBoundingClientRect();
    const canvas = document.createElement('canvas');
    const context = canvas.getContext('2d');
    context.font = getComputedStyle(input).font;
    return { x: bounds.left + 12 + context.measureText('M').width * 6.4, y: bounds.top + bounds.height / 2 };
  }, selector);
  await page.mouse.click(clickPoint.x, clickPoint.y);
  current = await state();
  assert.equal(current.active, true);
  assert.equal(current.selectionStart, current.selectionEnd);
  assert.ok(current.selectionStart >= 5 && current.selectionStart <= 7,
    `mouse positioning must address the plaintext character cells, got ${current.selectionStart}`);

  await page.evaluate((inputSelector) => {
    const input = document.querySelector(inputSelector);
    input.value = 'saved-silent-secret';
  }, selector);
  assert.notEqual((await state()).reveal, 'saved-silent-secret', 'silent replacement is never copied into plaintext');
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  current = await state();
  assert.equal(current.revealHidden, true);
  assert.equal(current.reveal, '');

  await reset();
  await page.focus(selector);
  const cdp = await page.createCDPSession();
  await cdp.send('Input.insertText', { text: 'saved-autofill-secret' });
  current = await state();
  assert.equal(current.value, 'saved-autofill-secret');
  assert.equal(current.eyeHidden, true, 'trusted insertion without a physical edit intent remains protected');
  assert.equal(current.reveal, '');

  await reset();
  await page.evaluate((inputSelector) => {
    const input = document.querySelector(inputSelector);
    input.value = 'script-secret';
    input.dispatchEvent(new InputEvent('input', { bubbles: true, inputType: 'insertText', data: 'script-secret' }));
  }, selector);
  current = await state();
  assert.equal(current.eyeHidden, true);
  assert.equal(current.reveal, '');

  await reset();
  await page.focus(selector);
  await page.keyboard.type('blur-secret');
  await page.click(eyeSelector);
  await page.click('#outside-control');
  current = await state();
  assert.equal(current.revealHidden, true);
  assert.equal(current.eyeHidden, true, 'leaving the password group revokes reveal provenance');

  await reset();
  await page.focus(selector);
  await page.keyboard.type('reset-secret');
  await page.click(eyeSelector);
  const synchronousReset = await page.evaluate(() => globalThis.__passwordFixture.reset());
  assert.deepEqual(synchronousReset, { hidden: true, text: '' }, 'reset token must hide plaintext before flushSync returns');

  await reset();
  await page.focus(selector);
  await page.keyboard.down('Control');
  await page.keyboard.press('KeyA');
  await page.keyboard.up('Control');
  await page.keyboard.type('submit-secret');
  await page.click(eyeSelector);
  await page.focus(selector);
  await page.keyboard.press('Enter');
  current = await state();
  assert.equal(current.active, true, 'Enter submit does not need a blur to revoke plaintext');
  assert.equal(current.revealHidden, true, 'Enter submit hides plaintext in the submit render');

  await reset();
  await page.focus(selector);
  await page.keyboard.type('semantic-reset-secret');
  await page.click(eyeSelector);
  await page.focus(selector);
  const semanticReset = await page.evaluate(() => globalThis.__passwordFixture.semanticReset('auth'));
  assert.deepEqual(semanticReset, { hidden: true, text: '' }, 'parent auth semantics must revoke plaintext in their first render');
  assert.equal((await state()).active, true, 'parent semantic reset must not depend on blur');

  await reset();
  await page.focus(selector);
  await cdp.send('Input.imeSetComposition', {
    text: '拼音', selectionStart: 2, selectionEnd: 2, replacementStart: 0, replacementEnd: 0,
  });
  await cdp.send('Input.insertText', { text: '拼音' });
  current = await state();
  const imeEvents = await page.evaluate(() => globalThis.__passwordEvents);
  assert.equal(current.eyeHidden, false, `trusted IME path must remain revealable: ${JSON.stringify(imeEvents)}`);

  await cdp.detach();
  process.stdout.write(`password input real-browser security test passed with ${path.basename(executablePath)}\n`);
} finally {
  await browser.close();
  await new Promise(resolve => server.close(resolve));
  await rm(fixtureRoot, { recursive: true, force: true });
}
