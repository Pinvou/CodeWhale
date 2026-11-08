import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import test from 'node:test';

const read = path => readFileSync(new URL(`../../../pinvou3-app/src/${path}`, import.meta.url), 'utf8');
const entries = { account: read('app/account-main.jsx'), admin: read('app/admin-main.jsx'), handoff: read('app/portal-session-handoff-main.js') };
// Execute the real dependency in the same realm as the browser globals. This
// matters for throwing Storage getters; host-realm imports would test Node's
// globalThis instead of the entry's globalThis.
function installModule(realm, source) {
  const names = [...source.matchAll(/^export (?:async function|function|const) (\w+)/gm)].map(match => match[1]);
  assert.ok(names.length);
  assert.ok(!/^import /m.test(source), 'dependency gained imports; update the execution harness');
  vm.runInContext(`Object.assign(globalThis, (() => { ${source.replace(/^export /gm, '')}\nreturn { ${names.join(',')} }; })());`, realm);
}
function fixture({ storage = 'normal', saved = null, browser = 'ja-JP', href = 'https://portal.example/account?section=settings#anchor' } = {}) {
  const trace = [], values = new Map(saved === null ? [] : [['pinvou.account.language', saved], ['pinvou.admin.language', saved]]);
  const realm = vm.createContext({ URL, URLSearchParams, navigator: { language: browser }, location: { href, search: new URL(href).search },
    history: { replaceState(...args) { trace.push(['replaceState', ...args]); } },
    document: { documentElement: {} }, setLanguage(value) { trace.push(['memory', value]); },
    dispatchEvent(event) { trace.push(['event', event.type, event.detail.language]); },
    CustomEvent: class { constructor(type, options) { this.type = type; this.detail = options.detail; } }, trace, values });
  vm.runInContext(`const storageMode = ${JSON.stringify(storage)};
    const denied = name => { const error = new Error('public synthetic Storage denial'); error.name = name; throw error; };
    const storage = {
      getItem(key) { trace.push(['read', key]); if (storageMode === 'read-denied') denied('SecurityError'); if (storageMode === 'read-error') denied('Error'); return values.get(key) ?? null; },
      setItem(key, value) { trace.push(['write', key, value]); if (storageMode === 'write-denied') denied('QuotaExceededError'); if (storageMode === 'write-error') denied('Error'); values.set(key, value); }
    };
    Object.defineProperty(globalThis, 'localStorage', { get() { trace.push(['getter']);
      if (storageMode === 'getter-denied') denied('SecurityError'); return storageMode === 'missing' ? undefined : storage; } });`, realm);
  Object.defineProperty(realm.document.documentElement, 'lang', { configurable: true,
    get() { return realm.htmlLanguage; }, set(value) { realm.htmlLanguage = value; trace.push(['html', value]); } });
  installModule(realm, read('features/account-platform/portal-language.js'));
  return { realm, trace, values };
}
function initial(kind, options) {
  const actual = fixture(options);
  const definition = entries[kind].match(/^function initialLanguage\(\) \{[\s\S]*?^\}/m)?.[0];
  assert.ok(definition, `real ${kind} initializer must remain testable`);
  const constants = entries[kind].match(/^const LANGUAGE_BY_TAG = .*;$/m)?.[0] || '';
  actual.language = vm.runInContext(`${constants}\n${definition}\ninitialLanguage()`, actual.realm, { filename: `${kind}-main.jsx` });
  return actual;
}

test('account entry starts in browser language when the optional Storage getter is denied', () => {
  assert.equal(initial('account', { storage: 'getter-denied', browser: 'ja-JP' }).language, 'ja');
});

test('real preference helpers report persistence success and contain both browser and ordinary storage errors', () => {
  for (const [storage, writable, readable] of [['normal', true, true], ['getter-denied', false, false],
    ['read-denied', true, false], ['write-denied', false, true], ['read-error', true, false], ['write-error', false, true], ['missing', false, false]]) {
    const actual = fixture({ storage, saved: 'ja' });
    assert.equal(vm.runInContext('storePortalLanguage(optionalPortalLanguageStorage(), "en")', actual.realm), writable);
    assert.equal(vm.runInContext('storedPortalLanguage(optionalPortalLanguageStorage())', actual.realm), readable ? writable ? 'en' : 'ja' : '');
    assert.equal(vm.runInContext('storePortalLanguage(optionalPortalLanguageStorage(), "zh", ADMIN_PORTAL_LANGUAGE_STORAGE_KEY)', actual.realm), writable);
    assert.equal(vm.runInContext('storedPortalLanguage(optionalPortalLanguageStorage(), ADMIN_PORTAL_LANGUAGE_STORAGE_KEY)', actual.realm), readable ? writable ? 'zh' : 'ja' : '');
    if (writable) {
      assert.equal(actual.values.get('pinvou.account.language'), 'en');
      assert.equal(actual.values.get('pinvou.admin.language'), 'zh', 'personal and admin preference keys remain distinct');
    }
  }
  for (const kind of ['account', 'admin']) {
    assert.equal(initial(kind, { storage: 'read-error', browser: 'en-US' }).language, 'en');
    assert.deepEqual(update(kind, { storage: 'write-error' }, 'ja').trace.filter(item => item[0] === 'memory'), [['memory', 'ja']]);
  }
});

test('account initializer keeps locale then saved then browser then Chinese priority and cleans only locale', () => {
  for (const language of ['zh', 'en', 'ja']) {
    for (const storage of ['normal', 'getter-denied', 'read-denied', 'write-denied', 'missing']) {
      const actual = initial('account', { storage, saved: 'en', browser: 'en-GB',
        href: `https://portal.example/account?section=settings&locale=${language}&keep=public#anchor` });
      assert.equal(actual.language, language, `${language}/${storage}`);
      assert.deepEqual(actual.trace.filter(item => item[0] === 'replaceState'),
        [['replaceState', undefined, '', '/account?section=settings&keep=public#anchor']]);
      if (storage === 'normal') assert.equal(actual.values.get('pinvou.account.language'), language);
    }
    assert.equal(initial('account', { saved: language, browser: 'fr-FR' }).language, language);
  }
  for (const storage of ['getter-denied', 'read-denied', 'missing']) {
    for (const [browser, expected] of [['ja-JP', 'ja'], ['en-AU', 'en'], ['zh-TW', 'zh'], ['fr-FR', 'zh']]) {
      assert.equal(initial('account', { storage, browser }).language, expected);
    }
  }
  const unknown = initial('account', { saved: 'ja', browser: 'en-US', href: 'https://portal.example/account?locale=fr&section=profile#anchor' });
  assert.equal(unknown.language, 'ja');
  assert.deepEqual(unknown.trace.filter(item => item[0] === 'replaceState'), [['replaceState', undefined, '', '/account?section=profile#anchor']]);
  assert.equal(unknown.trace.some(item => item[0] === 'write'), false);
  for (const saved of ['fr', 'JA', '*', 'undefined', '']) assert.equal(initial('account', { saved, browser: 'en-US' }).language, 'en');
  assert.equal(initial('account').trace.some(item => item[0] === 'replaceState'), false);
});

test('account aliases retain all three supported languages and consume duplicate/unknown locale parameters', () => {
  for (const [locale, expected] of [['zh-Hans', 'zh'], ['en-GB', 'en'], ['ja-JP', 'ja']]) {
    const actual = initial('account', { storage: 'write-denied', href: `https://portal.example/?locale=${locale}&locale=fr&keep=public#anchor` });
    assert.equal(actual.language, expected);
    assert.equal(actual.trace.find(item => item[0] === 'replaceState')[3], '/?keep=public#anchor');
  }
});

test('admin initializer retains saved-language and existing browser-tag behavior despite optional Storage failures', () => {
  for (const language of ['zh', 'en', 'ja']) assert.equal(initial('admin', { saved: language, browser: 'fr-FR' }).language, language);
  for (const storage of ['normal', 'getter-denied', 'read-denied', 'write-denied', 'missing']) {
    for (const [browser, expected] of [['zh-Hans', 'zh'], ['en-GB', 'en'], ['ja-JP', 'ja'], ['ja-extra', 'ja'], ['fr-FR', 'zh'], ['JA-JP', 'zh']]) {
      const actual = initial('admin', { storage, saved: 'unknown', browser, href: 'https://portal.example/admin.html?locale=en#anchor' });
      assert.equal(actual.language, expected, `${browser}/${storage}`);
      assert.equal(actual.trace.some(item => item[0] === 'replaceState'), false, 'admin has no locale URL consumer');
    }
  }
});

function update(kind, options, value) {
  const actual = fixture(options);
  const callback = entries[kind].match(/const updateLanguage = value => \{[\s\S]*?\n  \};/)?.[0];
  assert.ok(callback, `real ${kind} language callback must remain testable`);
  vm.runInContext(`${callback}\nupdateLanguage(${JSON.stringify(value)})`, actual.realm, { filename: `${kind}-main.jsx` });
  return actual;
}

test('real account and admin callbacks keep in-memory switching when preference persistence is denied', () => {
  for (const kind of ['account', 'admin']) for (const language of ['zh', 'en', 'ja']) {
    for (const storage of ['normal', 'getter-denied', 'read-denied', 'write-denied', 'missing']) {
      const actual = update(kind, { storage }, language);
      assert.deepEqual(actual.trace.filter(item => item[0] === 'memory'), [['memory', language]], `${kind}/${language}/${storage}`);
      if (storage === 'normal' || storage === 'read-denied') assert.equal(actual.values.get(`pinvou.${kind}.language`), language);
      if (kind === 'admin') {
        const html = language === 'zh' ? 'zh-CN' : language === 'ja' ? 'ja-JP' : 'en';
        assert.deepEqual(actual.trace.filter(item => ['html', 'memory', 'event'].includes(item[0])),
          [['html', html], ['memory', language], ['event', 'pinvou-admin-language-change', language]], 'HTML then memory then notification order remains stable');
      } else assert.equal(actual.trace.some(item => item[0] === 'event'), false);
    }
  }
});

test('optional preference containment does not swallow unrelated URL-cleanup or state-update failures', () => {
  const actual = fixture({ storage: 'write-denied' });
  actual.realm.history.replaceState = () => { throw new Error('PUBLIC_HISTORY_FAILURE'); };
  assert.throws(() => vm.runInContext('consumePortalLanguage("https://portal.example/?locale=ja", localStorage, history.replaceState)', actual.realm), /PUBLIC_HISTORY_FAILURE/);
  const callback = entries.account.match(/const updateLanguage = value => \{[\s\S]*?\n  \};/)?.[0];
  actual.realm.setLanguage = () => { throw new Error('PUBLIC_STATE_FAILURE'); };
  assert.throws(() => vm.runInContext(`${callback}\nupdateLanguage("ja")`, actual.realm), /PUBLIC_STATE_FAILURE/);
});

async function handoff({ storage = 'normal', locale = 'ja', browser = 'en-US', response = 'success', ticket = 'public-fixture-ticket-value' } = {}) {
  const actual = fixture({ storage, browser, href: `https://portal.example/portal-session-handoff.html?keep=public&locale=${locale}#ticket=${ticket}` });
  installModule(actual.realm, read('features/account-platform/portal-session-handoff-url.js'));
  installModule(actual.realm, read('features/account-platform/portal-session-handoff.js'));
  const main = { textContent: '', append(...nodes) { actual.trace.push(['append', ...nodes.map(node => node.href || node.tag)]); } };
  actual.realm.document.querySelector = selector => { assert.equal(selector, 'main'); return main; };
  actual.realm.document.createElement = tag => ({ tag });
  actual.realm.accountPlatformCopy = Object.fromEntries(['zh', 'en', 'ja'].map(language => [language, {
    sessionHandoff: { title: `${language}-title`, opening: `${language}-opening`, unavailable: `${language}-unavailable`, invalid: `${language}-invalid` }, login: `${language}-login`,
  }]));
  actual.realm.location.replace = path => { actual.trace.push(['redirect', path]); };
  actual.realm.fetch = async (path, options) => {
    actual.trace.push(['consume', path, options, main.textContent]);
    return { ok: response === 'success', json: async () => response === 'success' ? { redirect_to: '/' }
      : { code: response === 'unavailable' ? 'SESSION_HANDOFF_UNAVAILABLE' : 'SESSION_HANDOFF_INVALID' } };
  };
  const entry = entries.handoff.replace(/^import [\s\S]*?;\r?\n/gm, '');
  assert.ok(!/^import /m.test(entry), 'entry gained unsupported import syntax');
  vm.runInContext(entry, actual.realm, { filename: 'portal-session-handoff-main.js' });
  // Drain the actual async consume/JSON/redirect or failure chain.
  await new Promise(resolve => setImmediate(resolve));
  actual.main = main;
  return actual;
}

test('real handoff entry clears ticket and locale before its sole consume even when preference Storage is unavailable', async () => {
  for (const storage of ['normal', 'getter-denied', 'read-denied', 'write-denied', 'missing']) for (const locale of ['zh', 'en', 'ja']) {
    const actual = await handoff({ storage, locale });
    const relevant = actual.trace.filter(item => ['replaceState', 'consume', 'redirect'].includes(item[0]));
    assert.equal(relevant.length, 3);
    assert.deepEqual(relevant[0], ['replaceState', null, '', '/portal-session-handoff.html?keep=public']);
    assert.equal(relevant[1][0], 'consume');
    assert.equal(relevant[1][1], '/portal-session-handoff/consume');
    assert.equal(relevant[1][2].method, 'POST');
    assert.equal(relevant[1][2].credentials, 'same-origin');
    assert.equal(relevant[1][2].cache, 'no-store');
    assert.equal(relevant[1][2].body, '{"ticket":"public-fixture-ticket-value"}');
    assert.equal(relevant[1][3], `${locale}-opening`);
    assert.deepEqual(relevant[2], ['redirect', '/']);
    assert.equal(actual.realm.document.documentElement.lang, locale === 'zh' ? 'zh-CN' : locale === 'ja' ? 'ja-JP' : 'en');
  }
});

test('handoff failures keep one consume and a normal-login recovery link without replay or authentication fallback', async () => {
  for (const response of ['invalid', 'unavailable']) {
    const actual = await handoff({ storage: 'getter-denied', locale: 'fr', browser: 'ja-JP', response });
    assert.equal(actual.trace.filter(item => item[0] === 'consume').length, 1);
    assert.equal(actual.main.textContent, `ja-${response}`);
    assert.equal(actual.trace.some(item => item[0] === 'redirect'), false);
    assert.deepEqual(actual.trace.find(item => item[0] === 'append'), ['append', 'br', '/auth/login?return_to=%2F']);
  }
  const invalid = await handoff({ storage: 'write-denied', ticket: 'short' });
  assert.equal(invalid.trace.some(item => item[0] === 'consume'), false, 'malformed ticket never dispatches');
  assert.equal(invalid.main.textContent, 'ja-invalid');
});
