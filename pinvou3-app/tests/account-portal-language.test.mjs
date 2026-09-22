import assert from 'node:assert/strict';
import test from 'node:test';

import {
  browserPortalLanguage,
  consumePortalLanguage,
  normalizePortalLanguage,
  PORTAL_LANGUAGE_STORAGE_KEY,
  portalHTMLLanguage,
} from '../src/features/account-platform/portal-language.js';
import {
  portalSessionHandoffLanguage,
  takePortalSessionHandoffTicket,
} from '../../account-platform/internal/portal/static/assets/portal-session-handoff-url.js';

test('desktop locale variants map to the three supported portal languages', () => {
  assert.equal(normalizePortalLanguage('zh-Hans'), 'zh');
  assert.equal(normalizePortalLanguage('en-US'), 'en');
  assert.equal(normalizePortalLanguage('ja-JP'), 'ja');
  assert.equal(normalizePortalLanguage('fr-FR'), '');
  assert.equal(normalizePortalLanguage('zh-TW'), '');
  assert.equal(normalizePortalLanguage('en-arbitrary'), '');
  assert.equal(normalizePortalLanguage('ja-anything'), '');
  assert.equal(portalHTMLLanguage('ja'), 'ja-JP');
});

test('browser locale keeps compatible regional variants separate from strict URL input', () => {
  assert.equal(browserPortalLanguage('en-AU'), 'en');
  assert.equal(browserPortalLanguage('en-CA'), 'en');
  assert.equal(browserPortalLanguage('ja-Hira-JP'), 'ja');
  assert.equal(browserPortalLanguage('zh-TW'), 'zh');
  assert.equal(browserPortalLanguage('fr-FR'), '');
});

test('portal locale handoff persists language and removes only the transient query key', () => {
  const values = new Map();
  let replaced = '';
  const storage = {
    setItem(key, value) { values.set(key, value); },
  };

  const language = consumePortalLanguage(
    'https://account.example/?section=creditsUsage&locale=en#details',
    storage,
    (_state, _title, target) => { replaced = target; },
  );

  assert.equal(language, 'en');
  assert.equal(values.get(PORTAL_LANGUAGE_STORAGE_KEY), 'en');
  assert.equal(replaced, '/?section=creditsUsage#details');
});

test('unsupported locale is removed without changing the stored language', () => {
  const values = new Map([[PORTAL_LANGUAGE_STORAGE_KEY, 'ja']]);
  let replaced = '';
  const storage = {
    setItem(key, value) { values.set(key, value); },
  };

  const language = consumePortalLanguage(
    'https://account.example/?locale=zh-TW&section=accountSettings',
    storage,
    (_state, _title, target) => { replaced = target; },
  );

  assert.equal(language, '');
  assert.equal(values.get(PORTAL_LANGUAGE_STORAGE_KEY), 'ja');
  assert.equal(replaced, '/?section=accountSettings');
});

test('session handoff removes locale and ticket before every consume outcome', () => {
  for (const testCase of [
    { name: 'valid ticket', href: 'https://account.example/portal-session-handoff?locale=en&source=desktop#ticket=one-time-ticket', ticket: 'one-time-ticket' },
    { name: 'missing ticket', href: 'https://account.example/portal-session-handoff?locale=ja', ticket: '' },
    { name: 'consume failure', href: 'https://account.example/portal-session-handoff?locale=zh-Hans#ticket=expired-ticket', ticket: 'expired-ticket' },
  ]) {
    let replaced = '';
    const ticket = takePortalSessionHandoffTicket(
      testCase.href,
      (_state, _title, target) => { replaced = target; },
    );
    assert.equal(ticket, testCase.ticket, testCase.name);
    assert.equal(replaced.includes('locale='), false, testCase.name);
    assert.equal(replaced.includes('#ticket='), false, testCase.name);
  }
});

test('old desktop handoff keeps the existing portal preference', () => {
  const writes = [];
  const storage = { setItem(key, value) { writes.push([key, value]); } };

  assert.equal(portalSessionHandoffLanguage('', 'en-US', storage), 'en');
  assert.deepEqual(writes, []);
  assert.equal(portalSessionHandoffLanguage('unsupported', 'ja-JP', storage), 'ja');
  assert.deepEqual(writes, []);
});

test('new desktop handoff overwrites the portal preference with an explicit locale', () => {
  const writes = [];
  const storage = { setItem(key, value) { writes.push([key, value]); } };

  assert.equal(portalSessionHandoffLanguage('en', 'ja-JP', storage), 'en');
  assert.deepEqual(writes, [['pinvou.account.language', 'en']]);
});
