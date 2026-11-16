// Controlled real-browser evidence probe. No remote resources or application data.
import * as puppeteer from './puppeteer-fixture.mjs';
import { createServer } from './http-fixture.mjs';
import { fixtureScope } from './fixture-scope.mjs';
import assert from 'node:assert/strict';

let browser;
try {
  const server = createServer((request, response) => {
    if (request.url.startsWith('/reset')) { request.socket.destroy(); return; }
    if (request.url.startsWith('/unavailable')) { response.writeHead(503); response.end('controlled unavailable'); return; }
    response.setHeader('Content-Type', 'text/html');
    response.end('<!doctype html><title>diagnostic probe</title><body><h1>controlled fixture DOM</h1><input value="input-private"><textarea>textarea-private</textarea><p data-private>marked-private</p></body>');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  browser = await puppeteer.launch({ executablePath: process.env.CHROME, headless: true, args: ['--no-sandbox', '--disable-gpu'] });
  const page = await browser.newPage();
  await page.goto(`http://127.0.0.1:${server.address().port}/?token=query-private`);
  await page.evaluate(async () => {
    console.error('controlled console failure token=console-private');
    await fetch('/unavailable?token=request-private');
    try { await fetch('/reset?token=request-private'); } catch { /* deliberate connection failure */ }
    setTimeout(() => { throw new Error('controlled page exception'); }, 0);
    setTimeout(() => { window.probeDone = true; }, 10);
  });
  await page.waitForFunction(() => window.probeDone);
  await assert.rejects(page.waitForSelector('#controlled-missing-element', { timeout: 25 }), /Waiting for selector/);
  assert.equal(await page.$eval('input', node => getComputedStyle(node).visibility), 'visible');
  assert.equal(await page.$eval('input', node => node.value), 'input-private');
  if (process.env.DIAGNOSTIC_PROBE_FAIL === '1') throw new Error('controlled primary assertion');
} catch (error) {
  console.error(error.message);
  process.exitCode = 27;
} finally {
  await browser?.close();
  await fixtureScope.close();
}
