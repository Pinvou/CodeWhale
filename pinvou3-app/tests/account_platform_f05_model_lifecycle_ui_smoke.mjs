import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || process.env.CHROMIUM || [
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Google/Chrome/Application/chrome.exe', '/usr/bin/chromium', '/usr/bin/google-chrome',
].find(fs.existsSync);
if (!chrome) throw new Error('Browser unavailable');
// Reuse only the existing controlled IPC producer. Importing ui_smoke itself
// would start its unrelated full smoke. Fail closed if its explicit boundary changes.
const mockFile = fs.readFileSync(path.join(root, 'tests/ui_smoke.js'), 'utf8');
const begin = mockFile.indexOf('function injectSource() {');
const end = mockFile.indexOf('\nconst sleep =', begin);
assert.ok(begin >= 0 && end > begin, 'existing mock producer boundary required');
const existingProducer = new Function(`return (${mockFile.slice(begin, end)});`)()();
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f05-lifecycle-'));
const ownedProfile = path.resolve(profile);
assert.equal(path.dirname(ownedProfile), path.resolve(os.tmpdir()));
assert.ok(path.basename(ownedProfile).startsWith('pinvou-f05-lifecycle-'));
let vite, browser, page, passed = 0, launchAttempted = false, phase = "setup";
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  launchAttempted = true;
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  page = await browser.newPage(); await page.setViewport({ width: 1280, height: 960 });
  const errors = []; page.on('pageerror', error => errors.push(error.name));
  await page.evaluateOnNewDocument(existingProducer);
  await page.evaluateOnNewDocument(() => {
    const baseline = window.__TAURI__.core.invoke;
    const id = '01a08b7a-654d-7e8d-8a7c-87b4506f2c7c';
    const replacementId = '0199a100-0001-7000-8000-000000000106';
    const now = Date.now();
    const old = [{ event: 'user_start', turn_id: 'timing-A', timestamp: now },
      { event: 'assistant_done', turn_id: 'timing-A', timestamp: now, status: 'Completed' }];
    const control = window.f05Lifecycle = { language: 'en', ready: false, mode: 'fresh', legacy: false, calls: [], reads: [], timeline: old, oldSnapshot: old, branches: 0 };
    const savedSource = { metadata: { id: 's1', title: 'Controlled source', model: id, workspace: '' },
      messages: [{ role: 'user', content: [{ type: 'text', text: 'Previous committed question' }] },
        { role: 'assistant', content: [{ type: 'text', text: 'Previous committed response' }] }], artifacts: [] };
    control.savedSource = savedSource;
    control.resolveRead = index => { const read = control.reads[index]; if (!read) throw new Error('missing controlled read'); read.resolve(read.snapshot); read.resolved = true; };
    window.__TAURI__.core.invoke = (command, args = {}) => {
      control.calls.push({ command, args });
      if (command === 'chat') {
        const timestamp = Date.now();
        const rejection = control.legacy
          ? { schema_version: 1, code: 'MODEL_UNAVAILABLE', model_id: '', legacy_binding: true }
          : { schema_version: 1, code: 'MODEL_UNAVAILABLE', model_id: id };
        control.timeline = [...control.oldSnapshot, { event: 'user_start', turn_id: 'timing-B', timestamp },
          { event: 'assistant_done', turn_id: 'timing-B', timestamp: timestamp + 1, status: 'send_error', model_rejection: rejection }];
        return Promise.reject(new Error('controlled preparation unavailable'));
      }
      if (command === 'get_session_timeline') {
        if (args.sessionId !== 's1') return Promise.resolve([]);
        if (control.mode === 'late-old') return new Promise(resolve => control.reads.push({ resolve, snapshot: structuredClone(control.oldSnapshot) }));
        return Promise.resolve(structuredClone(control.timeline));
      }
      if (command === 'load_session' && args.id === 's1') return Promise.resolve(structuredClone(savedSource));
      if (command === 'account_platform_model_catalog') return Promise.resolve({ catalog_version: 1, items: [
        { model_id: replacementId, display_name: 'Current replacement', context_eligible: true, lifecycle_status: 'PUBLISHED' },
      ] });
      if (command === 'account_platform_branch_replacement_session') {
        control.branches++; control.branchInput = structuredClone(args.input);
        return Promise.resolve({ session: { id: 'replacement-s1', title: 'Controlled replacement' } });
      }
      if (command === 'load_session' && args.id === 'replacement-s1') return Promise.resolve({
        metadata: { ...savedSource.metadata, id: 'replacement-s1', model: replacementId },
        messages: structuredClone(savedSource.messages), artifacts: [],
      });
      return baseline(command, args);
    };
  });
  const fresh = async language => {
    errors.length = 0;
    await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f05-model-lifecycle.html`);
    await page.waitForFunction(() => window.f05Lifecycle?.ready && document.querySelector('textarea'));
    await page.evaluate(async language => { window.f05Lifecycle.language = language; await window.f05Lifecycle.render(); }, language);
  };
  const shown = () => page.evaluate(() => document.body.textContent.includes(window.f05Lifecycle.copy().uiModelReplacement.title));
  const awaitBanner = () => page.waitForFunction(() => document.body.textContent.includes(window.f05Lifecycle.copy().uiModelReplacement.title));
  for (const legacy of [false, true]) for (const language of ['zh', 'en', 'ja']) {
    phase = `locale ${language} setup`;
    await fresh(language); assert.equal(await shown(), false);
    await page.evaluate(legacy => { window.f05Lifecycle.legacy = legacy; if (legacy) window.f05Lifecycle.savedSource.metadata.model = 'deepseek-v4-flash'; }, legacy);
    const before = await page.evaluate(() => ({ defaultRef: window.f05Lifecycle.defaultRef(), source: window.f05Lifecycle.savedSource }));
    assert.equal((await page.evaluate(() => window.f05Lifecycle.sendFailure())).ok, false);
    phase = `locale ${language} current B banner`;
    await awaitBanner();
    assert.equal(await page.evaluate(() => window.f05Lifecycle.currentMarker()?.code), 'MODEL_UNAVAILABLE');
    assert.equal(await page.evaluate(() => window.f05Lifecycle.currentMarker()?.legacy_binding === true), legacy);
    assert.equal(await page.evaluate(() => window.f05Lifecycle.branches), 0, 'an unavailable binding never selects a new branch automatically');
    assert.equal(await page.evaluate(() => document.body.textContent.includes(window.f05Lifecycle.copy().uiModelReplacement.lead)), true);
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === window.f05Lifecycle.copy().uiModelReplacement.create && !button.disabled));
    await page.evaluate(() => [...document.querySelectorAll('button')].find(button => button.textContent === window.f05Lifecycle.copy().uiModelReplacement.create).click());
    await page.waitForFunction(() => window.f05Lifecycle.activeId() === 'replacement-s1');
    const after = await page.evaluate(() => ({ defaultRef: window.f05Lifecycle.defaultRef(), source: window.f05Lifecycle.savedSource, input: window.f05Lifecycle.branchInput,
      defaults: window.f05Lifecycle.calls.filter(call => ['account_platform_select_catalog_model', 'set_default_model', 'set_active_model'].includes(call.command)).length }));
    assert.deepEqual(after.defaultRef, before.defaultRef); assert.deepEqual(after.source, before.source); assert.equal(after.defaults, 0);
    assert.equal(after.input.sourceConversationId, 's1'); assert.equal(after.input.replacementModelId, '0199a100-0001-7000-8000-000000000106');
    assert.deepEqual(errors, []); passed++; console.log(`PASS compiled ChatView ${legacy ? 'legacy name' : 'UUID'} prepare rejection and manual branch ${language}`);
  }
  // An old A read starts after B's failed attempt, in a background working set.
  // Opening authoritative B must show B; late A cannot replace that owner.
  phase = 'background setup and failed B submission';
  await fresh('en');
  await page.evaluate(async () => {
    const c = window.f05Lifecycle;
    const marker = { schema_version: 1, code: 'MODEL_DEPRECATED', model_id: '01a08b7a-654d-7e8d-8a7c-87b4506f2c7c' };
    c.oldSnapshot = [c.oldSnapshot[0], { ...c.oldSnapshot[1], status: 'Failed', model_rejection: marker }];
    c.timeline = c.oldSnapshot;
    await c.switchTo('s-browser-b');
    c.mode = 'late-old'; await c.sendFailure();
  });
  phase = 'background frozen A read queued';
  await page.waitForFunction(() => window.f05Lifecycle.reads.length > 0);
  assert.equal(await shown(), false, 'background rejection does not borrow foreground ownership');
  phase = 'background actual authoritative B refresh';
  // The fixture explicitly drives a SECOND read through the captured installed
  // production reader. Warm switch itself performs no authority fetch. This
  // case verifies reader ownership plus rendered cached B, not automatic
  // user warm-switch recovery or a complete consumer-event lifecycle.
  const backgroundRefresh = await page.evaluate(async () => {
    const c = window.f05Lifecycle;
    const activeBefore = c.activeId();
    const timelineReadsBefore = c.calls.filter(call => call.command === 'get_session_timeline' && call.args.sessionId === 's1').length;
    c.mode = 'fresh';
    const changed = await c.refreshAuthoritativeTimeline('s1');
    return { changed, activeBefore, activeAfter: c.activeId(),
      timelineReadsAfter: c.calls.filter(call => call.command === 'get_session_timeline' && call.args.sessionId === 's1').length,
      timelineReadsBefore };
  });
  assert.equal(backgroundRefresh.changed, true, 'the actual installed reader accepts current typed B');
  assert.equal(backgroundRefresh.activeBefore, 's-browser-b');
  assert.equal(backgroundRefresh.activeAfter, backgroundRefresh.activeBefore, 'background authority refresh must not navigate foreground');
  assert.equal(backgroundRefresh.timelineReadsAfter, backgroundRefresh.timelineReadsBefore + 1, 'one real IPC authority read must occur');
  assert.equal(await shown(), false, 'typed background B remains outside foreground replacement UI');
  phase = 'background warm switch and current B banner';
  await page.evaluate(() => window.f05Lifecycle.switchTo('s1'));
  await awaitBanner();
  assert.equal(await page.evaluate(() => window.f05Lifecycle.currentMarker()?.code), 'MODEL_UNAVAILABLE');
  phase = 'background late old A resolution';
  await page.evaluate(() => window.f05Lifecycle.resolveRead(0));
  await page.waitForFunction(() => window.f05Lifecycle.reads[0].resolved);
  // A rendering turn gives the late promise continuation and subscription a
  // bounded visible commit opportunity; this does not replace state assertions.
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.evaluate(() => window.f05Lifecycle.currentMarker()?.code), 'MODEL_UNAVAILABLE');
  assert.deepEqual(errors, []); passed++; console.log('PASS background explicitly refreshed B survives late old A');
  console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
} catch (error) {
  console.error(`FAIL model lifecycle browser at ${phase}: ${error.name} ${error instanceof assert.AssertionError ? error.message : ''}`);
  process.exitCode = 1;
} finally {
  let browserStopped = !launchAttempted;
  try { if (browser) { await browser.close(); browserStopped = true; } }
  catch { process.exitCode = 1; console.error('FAIL browser cleanup; owned profile retained'); }
  try { if (vite) await vite.close(); }
  catch { process.exitCode = 1; console.error('FAIL fixture server cleanup'); }
  if (browserStopped && fs.existsSync(ownedProfile)) {
    const resolved = fs.realpathSync(ownedProfile);
    assert.equal(path.dirname(resolved), fs.realpathSync(os.tmpdir()));
    assert.ok(path.basename(resolved).startsWith('pinvou-f05-lifecycle-'));
    assert.equal(fs.lstatSync(ownedProfile).isSymbolicLink(), false);
    fs.rmSync(ownedProfile, { recursive: true, force: true });
  }
}
