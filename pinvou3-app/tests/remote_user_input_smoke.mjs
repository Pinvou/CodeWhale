import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

// The committed DTO is checked against the real native question producer by
// Rust. Formal local runs may supply its exact fresh nonce/output instead.
const producer = JSON.parse(fs.readFileSync(process.env.PINVOU_REMOTE_USER_INPUT_PROJECTION_FILE
  || new URL('../../remote-control-relay/test/fixtures/controller-user-input.json', import.meta.url), 'utf8'));
const mixedProducer = process.env.PINVOU_REMOTE_USER_INPUT_MIXED_FILE
  ? JSON.parse(fs.readFileSync(process.env.PINVOU_REMOTE_USER_INPUT_MIXED_FILE, 'utf8')).requests
  : JSON.parse(fs.readFileSync(new URL('../../remote-control-relay/test/fixtures/controller-user-input.json', import.meta.url), 'utf8')).mixed_requests;
const chrome = process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium',
].find(file => fs.existsSync(file));
assert.ok(chrome, 'A test browser is required');
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
let browser, vite;
let count = 0;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()] });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/remote_permission_state_smoke.html`;
  async function load(language = 'zh') {
    errors.length = 0;
    await page.goto(`${url}?scenario=question&lang=${language}`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => window.dispatchFixture?.permissionRequests?.some(item => item.args.intent.operation === 'user_input_list'));
  }
  async function supply(index, requests = producer.requests) {
    await page.evaluate(({ index, requests }) => window.dispatchFixture.permissionRequests
      .filter(item => item.args.intent.operation === 'user_input_list')[index].resolve({ requests }), { index, requests });
    if (requests.length) await page.waitForSelector('[data-testid="remote-user-input"] input[type="checkbox"]');
  }
  async function clickCopy(key) {
    await page.evaluate(key => {
      const text = key === 'submit' ? window.dispatchFixture.copy.uiSubmit : window.dispatchFixture.copy.remoteConversation[key];
      const button = [...document.querySelectorAll('[data-testid="remote-user-input"] button')].find(item => item.textContent === text);
      if (!button || button.disabled) throw new Error(`Unavailable question action ${key}`);
      button.click();
    }, key);
  }
  async function decision(index) {
    await page.waitForFunction(index => window.dispatchFixture.permissionRequests
      .filter(item => item.args.intent.operation === 'user_input_respond').length > index, {}, index);
    return page.evaluate(index => window.dispatchFixture.permissionRequests
      .filter(item => item.args.intent.operation === 'user_input_respond')[index].args, index);
  }
  async function settle(index, value, reject = false) {
    await page.evaluate(({ index, value, reject }) => window.dispatchFixture.permissionRequests
      .filter(item => item.args.intent.operation === 'user_input_respond')[index][reject ? 'reject' : 'resolve'](value), { index, value, reject });
  }
  for (const language of ['zh', 'en', 'ja']) {
    await load(language); await supply(0);
    await page.waitForFunction(() => document.body.textContent.includes(window.dispatchFixture.copy.remoteConversation.userInputWaiting));
    await page.click('[data-testid="remote-user-input"] input[type="checkbox"]');
    const boxes = await page.$$('[data-testid="remote-user-input"] input[type="checkbox"]');
    await boxes[1].click();
    await clickCopy('submit');
    const sent = await decision(0);
    assert.equal(sent.intent.inputId, producer.requests[0].input_id);
    assert.equal(sent.intent.taskId, 'task-wait');
    assert.equal(sent.localSessionId, 'remote-a');
    assert.deepEqual(sent.intent.answers, [{ question_id: '0', option_ids: ['0', '1'] }]);
    await settle(0, { state: 'accepted' });
    await page.waitForFunction(() => !document.querySelector('[data-testid="remote-user-input"] input'));
    assert.deepEqual(errors, []); count += 1;
  }

  for (const custom of [false, true]) {
    await load(); await supply(0, mixedProducer);
    const boxes = await page.$$('[data-testid="remote-user-input"] input[type="checkbox"]');
    await boxes[0].click();
    if (!custom) await boxes[1].click();
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('[data-testid="remote-user-input"] button')]
      .find(button => button.textContent === window.dispatchFixture.copy.uiSubmit).disabled), true,
    'the unanswered single-choice question blocks submission of a multi-question card');
    if (custom) await page.type('[data-testid="remote-user-input"] fieldset:nth-of-type(2) input[type="text"]', 'My output');
    else {
      const radios = await page.$$('[data-testid="remote-user-input"] input[type="radio"]');
      await radios[1].click();
    }
    await clickCopy('submit');
    const sent = await decision(0);
    assert.equal(sent.intent.inputId, mixedProducer[0].input_id);
    assert.deepEqual(sent.intent.answers, [
      { question_id: '0', option_ids: custom ? ['0'] : ['0', '1'] },
      custom ? { question_id: '1', option_ids: [], custom_text: 'My output' }
        : { question_id: '1', option_ids: ['1'] },
    ]);
    await settle(0, { state: 'accepted' });
    await page.waitForFunction(() => !document.querySelector('[data-testid="remote-user-input"] input'));
    assert.deepEqual(errors, []); count += 1;
  }

  await load(); await supply(0); await clickCopy('userInputCancel');
  const cancellation = await decision(0);
  assert.equal(cancellation.intent.cancel, true); assert.deepEqual(cancellation.intent.answers, []);
  await settle(0, { state: 'accepted' });
  await page.waitForFunction(() => !document.querySelector('[data-testid="remote-user-input"] input'));
  count += 1;

  await load(); await supply(0);
  await page.click('[data-testid="remote-user-input"] input[type="checkbox"]');
  await clickCopy('submit');
  const original = await decision(0);
  await settle(0, { code: 'result_unknown', requestId: original.requestId }, true);
  const retry = await decision(1);
  assert.equal(retry.requestId, original.requestId);
  await settle(1, { code: 'remote_action_failed', requestId: original.requestId }, true);
  await page.waitForFunction(() => document.querySelector('[data-testid="remote-user-input"] [role="alert"]')
    ?.textContent.includes(window.dispatchFixture.copy.remoteConversation.userInputUnknown));
  await page.waitForFunction(() => window.dispatchFixture.permissionRequests.filter(item => item.args.intent.operation === 'user_input_list').length > 1);
  await supply(1);
  assert.equal(await page.evaluate(() => [...document.querySelectorAll('[data-testid="remote-user-input"] input')]
    .every(input => input.matches(':disabled'))), true, 'the fieldset must disable every editable answer');
  await clickCopy('retry'); const recovered = await decision(2);
  assert.equal(recovered.requestId, original.requestId); assert.deepEqual(recovered.intent, original.intent);
  await settle(2, { state: 'accepted' });
  await page.waitForFunction(() => !document.querySelector('[data-testid="remote-user-input"] input'));
  count += 1;

  await load(); await supply(0);
  await page.click('[data-testid="remote-user-input"] input[type="checkbox"]');
  await page.waitForFunction(() => window.dispatchFixture.permissionRequests.filter(item => item.args.intent.operation === 'user_input_list').length > 1);
  await page.evaluate(() => window.dispatchFixture.renewPermissionCapability());
  await supply(1);
  assert.equal(await page.$eval('[data-testid="remote-user-input"] input[type="checkbox"]', input => input.checked), true,
    'a capability refresh cannot erase an unsubmitted choice in the same task');
  assert.deepEqual(errors, []); count += 1;

  for (const terminal of ['cancelled', 'interrupted', 'failed', 'result_unknown']) {
    await load();
    await page.evaluate(terminal => window.dispatchFixture.questionTerminal(terminal), terminal);
    await page.waitForFunction(() => !document.querySelector('[data-testid="remote-user-input"]'));
    await page.evaluate(requests => window.dispatchFixture.permissionRequests
      .find(item => item.args.intent.operation === 'user_input_list').resolve({ requests }), producer.requests);
    await page.evaluate(() => new Promise(requestAnimationFrame));
    assert.equal(await page.$('[data-testid="remote-user-input"]'), null, 'late question snapshot cannot revive a terminal task');
    assert.deepEqual(errors, []); count += 1;
  }
  for (const move of ['session', 'lease']) {
    await load();
    await page.evaluate(move => move === 'session' ? window.dispatchFixture.select('remote-b') : window.dispatchFixture.select('remote-a', 2), move);
    await page.waitForFunction(() => window.dispatchFixture.permissionRequests.filter(item => item.args.intent.operation === 'user_input_list').length > 1);
    const successor = structuredClone(producer.requests); successor[0].input_id = 'input-successor';
    successor[0].questions[0].question = 'Successor question';
    await supply(1, successor);
    await page.evaluate(requests => window.dispatchFixture.permissionRequests
      .filter(item => item.args.intent.operation === 'user_input_list')[0].resolve({ requests }), producer.requests);
    await page.evaluate(() => new Promise(requestAnimationFrame));
    const text = await page.$eval('[data-testid="remote-user-input"]', element => element.textContent);
    assert.ok(text.includes('Successor question')); assert.equal(text.includes('Choose a report'), false);
    assert.deepEqual(errors, []); count += 1;
  }
  console.log(JSON.stringify({ cases: count, passed: count, nativeBoundary: 'controlled; real ChatView, QuestionChoiceCard, bridge and question DTO' }));
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
}
