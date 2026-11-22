import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';
import { CHAT_RECOVERY_COPY, PPTX_PROFILE_UNAVAILABLE_COPY } from '../src/shared/i18n.js';

// Regression for the turn-error display chain: stable backend error codes
// (session_model_binding_stale:<id>, image_input_unsupported) must reach the
// tri-lingual recovery copy when the rejection arrives as an Error object —
// the shape the real Web RPC client produces (bootstrap.js wraps message.error
// in new Error). Error.toString() prepends "Error: ", so prefix matching must
// run on Error.message; this file executes the shipped bridge code to prove
// the localized copy is reachable in all three locales.

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.join(here, '..');
const read = (...parts) => fs.readFileSync(path.join(root, ...parts), 'utf8');

const bridgeMessagesSource = read('src', 'shared', 'bridge-messages.js');
const rawBridgeSource = read('src', 'platform', 'web', 'bridge.js');
const chatSource = read('src', 'platform', 'tauri', 'bridge', 'chat.js');

assert.equal(
  rawBridgeSource.split('function displayTurnError(err)').length,
  2,
  'displayTurnError must stay a single function so the hook injection is unambiguous',
);
const bridgeSource = (() => {
  const injected = rawBridgeSource
    .replace(
      'function displayTurnError(err)',
      'window.__displayTurnError = function displayTurnError(err)',
    )
    // Expose the shared-web bt() delegate so the WEB_TURN_ERROR_KEYS
    // contract can pin each stable code to its exact dict copy per locale.
    .replace(
      'function bt(key) { return pinvouSharedweb().bt(key); }',
      'function bt(key) { return pinvouSharedweb().bt(key); } window.__btForContract = bt;',
    )
    // Expose the lane wrapper so the consumer contract below can pin
    // wireLaneError's fold/concat behavior, not just its table membership.
    .replace(
      'function wireLaneError(err, prefixKey)',
      'window.__wireLaneError = function wireLaneError(err, prefixKey)',
    );
  assert.ok(
    injected.includes('window.__btForContract') && injected.includes('window.__wireLaneError'),
    'web bridge bt()/wireLaneError source drifted; update the injection anchors',
  );
  return injected;
})();

const storage = new Map();
const localStorage = {
  getItem(key) { return storage.has(key) ? storage.get(key) : null; },
  setItem(key, value) { storage.set(key, String(value)); },
  removeItem(key) { storage.delete(key); },
};
const documentObject = {
  readyState: 'loading',
  addEventListener() {},
  createElement() {
    return { click() {}, remove() {}, style: {}, setAttribute() {} };
  },
  body: { appendChild() {} },
};
let invokeResponse = async () => null;
const windowObject = {
  PinvouChatRecoveryCopy: CHAT_RECOVERY_COPY,
  PinvouPptxProfileUnavailableCopy: PPTX_PROFILE_UNAVAILABLE_COPY,
  PinvouPlatform: {
    kind: 'web',
    isWeb: true,
    capabilities: {},
    can: () => false,
    canInvoke: () => false,
  },
  __TAURI__: {
    core: { invoke: (...args) => invokeResponse(...args) },
    event: { listen: async () => function () {} },
    dialog: { open: async () => null },
  },
  location: { search: '', href: 'https://example.test/pinvou3/remote/' },
  localStorage,
  crypto: { randomUUID: () => '00000000-0000-4000-8000-000000000000' },
  performance: { now: () => 0 },
  addEventListener() {},
  setTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
};
const context = vm.createContext({
  window: windowObject,
  document: documentObject,
  navigator: { mediaDevices: null },
  localStorage,
  console,
  setTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
  structuredClone: value => structuredClone(value),
  URL,
  URLSearchParams,
  Blob,
  Uint8Array,
  ArrayBuffer,
  TextEncoder,
  TextDecoder,
});

vm.runInContext(bridgeMessagesSource, context, { filename: 'src/shared/bridge-messages.js' });
vm.runInContext(read('src', 'shared', 'bridge-shared-helpers.js'), context, { filename: 'src/shared/bridge-shared-helpers.js' });
vm.runInContext(bridgeSource, context, { filename: 'src/platform/web/bridge.js' });
vm.runInContext(read('src', 'platform', 'web', 'bridge', 'domain-adapter.js'), context, {
  filename: 'src/platform/web/bridge/domain-adapter.js',
});

const displayTurnError = windowObject.__displayTurnError;
assert.equal(typeof displayTurnError, 'function', 'displayTurnError hook must be exposed');
const api = windowObject.TauriBridge;
assert.equal(typeof api.settings.saveSettings, 'function', 'settings API must be available');

const DEFAULT_SETTINGS = { theme: 'genesis', color_scheme: 'system', language: 'zh-Hans' };
invokeResponse = async (command, args) => command === 'web_access_update_settings'
  ? { ...DEFAULT_SETTINGS, language: (args && args.patch && args.patch.language) || DEFAULT_SETTINGS.language }
  : null;

async function withLanguage(language, run) {
  await api.settings.saveSettings({ language });
  try {
    await run();
  } finally {
    await api.settings.saveSettings({ language: DEFAULT_SETTINGS.language });
  }
}

const STALE_ID = 'removed-model';
const LOCALE_MARKERS = {
  en: { marker: 'no longer available', tail: 'pick a model again' },
  ja: { marker: '無効になりました', tail: '選び直し' },
  'zh-Hans': { marker: '已失效', tail: '重新选择模型' },
};
// engine_pool.rs rejects sends/voice postprocess with a stable code plus a
// hardcoded English hint; both bridges must localize the recovery copy and
// never leak the raw English tail (review round-7 P2).
const SELECTION_REQUIRED_MARKERS = {
  en: /Pick a model in model settings before sending/,
  ja: /モデル設定でモデルを選んでください/,
  'zh-Hans': /先在模型设置中选择模型后再发送/,
};

for (const [language, { marker, tail }] of Object.entries(LOCALE_MARKERS)) {
  await withLanguage(language, () => {
    // The exact rejection shape produced by the Web RPC client (bootstrap.js
    // wraps the backend message in new Error).
    const text = displayTurnError(new Error(`session_model_binding_stale:${STALE_ID}`));
    assert.match(text, new RegExp(marker), `${language}: stale-binding copy must be shown`);
    assert.match(text, new RegExp(tail), `${language}: recovery instruction must be shown`);
    assert.ok(text.includes(STALE_ID), `${language}: the missing model id must be carried`);
    assert.doesNotMatch(text, /^Error:/, `${language}: toString() prefix must not leak`);
    assert.ok(!text.includes('session_model_binding_stale'), `${language}: raw code must not leak`);

    // Plain strings (Tauri invoke rejections) take the same path.
    assert.equal(
      displayTurnError(`session_model_binding_stale:${STALE_ID}`),
      text,
      `${language}: Error-object and string rejections must localize identically`,
    );

    // model_selection_required keeps its localization on both rejection
    // shapes and drops the backend's hardcoded English hint.
    const selectionCopy = displayTurnError(new Error('model_selection_required: choose a model before sending'));
    assert.match(selectionCopy, SELECTION_REQUIRED_MARKERS[language], `${language}: selection-required copy must be shown`);
    assert.ok(!selectionCopy.includes('model_selection_required'), `${language}: raw code must not leak`);
    assert.ok(!selectionCopy.includes('choose a model'), `${language}: English hint must not leak`);
    assert.equal(
      displayTurnError('model_selection_required: choose a model before voice postprocess'),
      selectionCopy,
      `${language}: send and voice variants share one recovery copy`,
    );
  });
}

await withLanguage('en', () => {
  // image_input_unsupported had the same toString() blind spot before the
  // Error.message normalization; pin it so it cannot regress either.
  const text = displayTurnError(new Error('image_input_unsupported'));
  assert.match(text, /does not support images/, 'en: image-unsupported copy must be shown');
  assert.doesNotMatch(text, /^Error:/, 'en: toString() prefix must not leak');
});

await withLanguage('en', () => {
  assert.equal(
    displayTurnError(new Error('disk full')),
    'disk full',
    'unknown errors pass through (redactRawError keeps text when no classifier helper is loaded)',
  );
});

const migrationCases = {
  en: [/restart Pinvou3/, /restore read access/],
  ja: [/再起動/, /読み取りアクセス/],
  'zh-Hans': [/重启/, /恢复读取/],
};
for (const [language, copy] of Object.entries(CHAT_RECOVERY_COPY)) {
  await withLanguage(language === 'zh' ? 'zh-Hans' : language, () => {
    const pptError = 'send_user_message failed: pptx_session_profile_unavailable';
    const pptCopy = PPTX_PROFILE_UNAVAILABLE_COPY[language].message;
    assert.equal(displayTurnError(new Error(pptError)), pptCopy);
    assert.equal(displayTurnError(pptError), pptCopy);
    assert.equal(displayTurnError('pptx_session_profile_unavailable'), pptCopy);
    for (const message of ['account_platform_runtime_protocol_unavailable: internal detail', 'account platform model runtime protocol is missing or unsupported; publish its capability before invoking']) {
      for (const prefix of ['', 'send_user_message failed: ']) {
        assert.equal(displayTurnError(new Error(prefix + message)), copy.modelRuntimeProtocolUnavailable);
        assert.equal(displayTurnError(prefix + message), copy.modelRuntimeProtocolUnavailable);
      }
    }
  });
}
for (const [language, [restart, restoreRead]] of Object.entries(migrationCases)) {
  await withLanguage(language, () => {
    const pending = 'send_user_message failed: account_platform_migration_incomplete: internal detail';
    const invalid = 'account_platform_migration_invalid: internal detail';
    for (const input of [pending, new Error(pending)]) {
      assert.match(displayTurnError(input), restart);
      assert.doesNotMatch(displayTurnError(input), /account_platform_migration_|internal detail/);
    }
    assert.match(displayTurnError(new Error(invalid)), restoreRead);
    assert.ok(displayTurnError(invalid).includes('migrations/account-platform-model-v1.json'));
    assert.equal(displayTurnError('upstream says '+invalid), 'upstream says '+invalid);
  });
}
assert.match(read('src', 'app', 'main.jsx'), /window.PinvouChatRecoveryCopy = CHAT_RECOVERY_COPY/);
assert.match(read('src', 'app', 'main.jsx'), /window.PinvouPptxProfileUnavailableCopy = PPTX_PROFILE_UNAVAILABLE_COPY/);
const firstTurnStart = rawBridgeSource.indexOf('  async function runFirstTurnSubmission(');
const firstTurnEnd = rawBridgeSource.indexOf('  function submitFirstWebTurn(', firstTurnStart);
assert.ok(firstTurnStart >= 0 && firstTurnEnd > firstTurnStart);
const firstTurnSource = rawBridgeSource.slice(firstTurnStart, firstTurnEnd);
for (const [language, [restart, restoreRead]] of Object.entries(migrationCases)) {
  for (const [detail, code, expectedState, expectedCopy] of [
    ['send_user_message failed: account_platform_migration_incomplete: internal detail', 'rpc_failed', 'failed', restart],
    ['account_platform_migration_invalid: internal detail', 'rpc_failed', 'failed', restoreRead],
    ['connection ended after submit', 'outcome_unknown', 'unknown', null],
  ]) {
    const item = {};
    const submission = { clientMessageId: 'recovery-test', command: 'web_access_create_session_and_chat' };
    const firstTurn = vm.createContext({
      window: windowObject, state: { settings: { language } },
      findFirstTurnItem: () => item, firstTurnStillVisible: () => true,
      notify() {}, restoreFirstTurnUiState() {},
      invokeWithRequestId(command) {
        assert.equal(command, 'web_access_create_session_and_chat');
        throw Object.assign(new Error(detail), { code });
      },
    });
    vm.runInContext(firstTurnSource, firstTurn);
    await firstTurn.runFirstTurnSubmission(submission);
    assert.equal(submission.lastError, detail);
    assert.equal(submission.lastErrorCode, code);
    assert.equal(item.deliveryState, expectedState);
    if (expectedCopy) {
      assert.match(item.deliveryError, expectedCopy);
      assert.doesNotMatch(item.deliveryError, /account_platform_migration_|internal detail/);
    } else {
      assert.equal(item.deliveryError, detail);
    }
  }
}

// Tauri counterpart audit pinned as a source contract: the display chain must
// derive its text the same way the concurrentTurn check does (Error.message
// first) and must match session_model_binding_stale before the raw-body
// redact fallback so the recovery copy is never swallowed.
const displaySection = chatSource.slice(
  chatSource.indexOf('// 稳定错误码'),
  chatSource.indexOf('addSystemItem(concurrentTurn'),
);
assert.match(displaySection, /String\(err && err\.message \? err\.message : err \|\| ""\)/);
assert.match(displaySection, /indexOf\("session_model_binding_stale:"\) === 0/);
assert.match(displaySection, /indexOf\("model_selection_required"\) === 0/);
assert.ok(
  displaySection.indexOf('session_model_binding_stale') <
  displaySection.indexOf('redactRawError'),
  'stable-code localization must run before the redact fallback',
);
assert.ok(
  displaySection.indexOf('model_selection_required') <
  displaySection.indexOf('redactRawError'),
  'selection-required localization must run before the redact fallback',
);

// Exercise the real #148 helper, not only its optional/missing fallback.
vm.runInContext(read('src', 'shared', 'model-service-errors.js'), context, {
  filename: 'src/shared/model-service-errors.js',
});
assert.equal(typeof windowObject.PinvouModelServiceErrors.redactTechnicalDetail, 'function');
const desktopBridgeSource = read('src', 'platform', 'tauri', 'bridge.js');
const desktopContext = vm.createContext({
  window: windowObject,
  state: { settings: {} },
});
// Execute the shipped desktop localization table and error-display block.
// Surrounding turn mutation is excluded: it is exercised by error-isolation tests.
// #701 T0-c 壳化适配：bt 已委托 helpers；沙箱按 index.html 顺序补载共享基座
// 并接好 tauriMain 访问器。
vm.runInContext(read('src', 'shared', 'bridge-shared-helpers.js'), desktopContext, { filename: 'src/shared/bridge-shared-helpers.js' });
vm.runInContext(`this.pinvouSharedtauriMain = () => this.__mainCluster || (this.__mainCluster = window.PinvouBridgeShared.create("tauriMain", {
  state, BT_TABLE: { get value() { return BT_TABLE; } }, invoke: async () => null,
  notify: () => {}, addChatItem: () => {},
}));`, desktopContext);
vm.runInContext(desktopBridgeSource.slice(
  desktopBridgeSource.indexOf('var BT_TABLE ='),
  desktopBridgeSource.indexOf('function textMatchesBtKey'),
), desktopContext);
vm.runInContext(`function displayDesktopError(err) { ${displaySection}\nreturn errorText; }`, desktopContext);
for (const [language, { marker, tail }] of Object.entries(LOCALE_MARKERS)) {
  await withLanguage(language, () => {
    desktopContext.state.settings.language = language;
    const localized = displayTurnError(new Error(`session_model_binding_stale: ${STALE_ID}`));
    assert.match(localized, new RegExp(marker));
    assert.match(localized, new RegExp(tail));
    assert.ok(localized.includes(STALE_ID));
    // The backend emits the spaced form (engine_pool formats `<code>: <id>`);
    // both bridges localize by code prefix and trim the remainder, so the
    // unspaced form is a tolerance pin (also pinned on the web lane above):
    // it must still localize, and the raw code must never leak into the copy.
    const unspaced = displayTurnError(new Error(`session_model_binding_stale:${STALE_ID}`));
    assert.match(unspaced, new RegExp(marker));
    assert.match(unspaced, new RegExp(tail));
    assert.ok(unspaced.includes(STALE_ID));
    assert.ok(!unspaced.includes('session_model_binding_stale'), `${language}: raw code must not leak`);
    const raw = 'gateway rejected Authorization: Bearer fixture-private-value';
    const redacted = displayTurnError(new Error(raw));
    assert.ok(!redacted.includes('fixture-private-value'), `${language}: real helper must redact credentials`);
    assert.doesNotMatch(redacted, /^Error:/);
    assert.equal(displayTurnError(raw), redacted, `${language}: Error and string share the real redaction path`);
    assert.equal(displayTurnError(new Error('disk full')), 'disk full');
    const pathError = 'read C:\\fixture-project\\private-notes.md failed: permission denied';
    assert.equal(windowObject.PinvouModelServiceErrors.isModelServiceError(pathError), false);
    assert.equal(displayTurnError(new Error(pathError)), pathError, 'local file errors keep their actionable path');
    const withSecret = `${pathError}; password: "fixture sensitive text"`;
    const pathRedacted = displayTurnError(new Error(withSecret));
    assert.ok(pathRedacted.includes('C:\\fixture-project\\private-notes.md'));
    assert.ok(!pathRedacted.includes('fixture sensitive text'));
    for (const input of [`session_model_binding_stale: ${STALE_ID}`, `session_model_binding_stale:${STALE_ID}`,
      'model_selection_required: choose a model before sending', raw, withSecret, 'disk full',
      'send_user_message failed: account_platform_migration_incomplete: internal detail',
      'account_platform_migration_invalid: internal detail']) {
      assert.equal(desktopContext.displayDesktopError(new Error(input)), displayTurnError(new Error(input)),
        `${language}: actual desktop and web blocks agree with the real helper loaded`);
      assert.equal(desktopContext.displayDesktopError(input), displayTurnError(input));
    }
  });
}

// ── WEB_TURN_ERROR_KEYS contract ──────────────────────────────────────
// Every stable code in the shipped table must resolve to its exact dict
// copy per locale — never to the raw code (which would otherwise reach the
// redaction fallback and leak backend internals) — identically for
// Error-object and plain-string rejections. Mirrors the table in
// src/platform/web/bridge.js; new codes must land on both sides.
const btFor = windowObject.__btForContract;
assert.equal(typeof btFor, 'function', 'bt contract hook must be exposed');
assert.ok(
  rawBridgeSource.split('WEB_TURN_ERROR_KEYS').length >= 4,
  'one code table must serve both consumers (definition + displayTurnError + wireLaneError)',
);
const wireLaneError = windowObject.__wireLaneError;
assert.equal(typeof wireLaneError, 'function', 'wireLaneError contract hook must be exposed');
// Round-5: pin the wrapper's behavior, not just the table's consumer count —
// a stable code folds to the full localized sentence; unmapped text keeps the
// lane prefix copy so download/attachment lanes read like the UI language.
for (const language of Object.keys(LOCALE_MARKERS)) {
  await withLanguage(language, () => {
    assert.equal(
      wireLaneError(new Error('web_session_chat_failed'), 'openFailed'),
      btFor('webChatFailed'),
      `${language}: stable code folds to the full sentence`,
    );
    assert.equal(
      wireLaneError(new Error('boom detail'), 'openFailed'),
      btFor('openFailed') + 'boom detail',
      `${language}: unmapped text keeps the lane prefix copy`,
    );
  });
}
// Consumer wiring pins: each lane catch routes through the wrapper, so
// reverting any single catch back to bare concatenation turns this leg red
// (external URL open, artifact open, attachment open, attachment reveal).
assert.equal(
  rawBridgeSource.split('wireLaneError(e, "openFailed")').length,
  5,
  'four lane catches must go through wireLaneError (split length = sites + 1)',
);
assert.ok(
  rawBridgeSource.includes('addSystemItem(bt("downloadFailed") + displayTurnError(e));'),
  'download catch must fold stable codes via displayTurnError (open round-9)',
);
assert.ok(
  !rawBridgeSource.includes('bt("downloadFailed") + String('),
  'download catch must not render raw machine codes',
);
const CODE_TABLE = [
  ['web_session_chat_failed', 'webChatFailed'],
  ['web_session_create_session_failed', 'webSessionCreateFailed'],
  ['web_session_turn_in_progress', 'turnAlreadyInProgress'],
  ['session_revision_conflict', 'editStaleRevision'],
  ['session_revision_unavailable', 'editRevisionUnavailable'],
  ['web_artifact_too_large', 'artifactTooLarge'],
  ['web_artifact_preview_too_large', 'artifactPreviewTooLarge'],
  ['web_artifact_edit_too_large', 'artifactEditTooLarge'],
  ['web_artifact_read_failed', 'artifactReadFailed'],
  ['web_session_read_artifact_chunk_failed', 'artifactReadFailed'],
  ['web_session_artifact_access_failed', 'artifactReadFailed'],
  ['web_session_read_conversation_attachment_chunk_failed', 'attachReadFailed'],
  ['web_session_unavailable', 'webSessionUnavailable'],
  ['invalid_web_session_id', 'webSessionUnavailable'],
];
const locales = ['zh-Hans', 'en', 'ja'];
const copiesByLocale = new Map();
for (const language of locales) {
  await withLanguage(language, () => {
    for (const [code, key] of CODE_TABLE) {
      const copy = displayTurnError(new Error(code));
      assert.equal(copy, btFor(key), `${language}: ${code} must render the ${key} copy`);
      assert.equal(
        displayTurnError(code),
        copy,
        `${language}: ${code} must localize identically for string rejections`,
      );
      assert.ok(!copy.includes(code), `${language}: ${code} raw code must not leak`);
      copiesByLocale.set(`${key}\u0000${language}`, copy);
    }
  });
}
// A key that stopped consuming the locale dict (e.g. hardcoded to one
// language's sentence) would collapse the variants — require cross-locale
// spread per key so the table cannot silently go mono-lingual.
for (const key of new Set(CODE_TABLE.map(([, dictKey]) => dictKey))) {
  const variants = new Set(locales.map((language) => copiesByLocale.get(`${key}\u0000${language}`)));
  assert.ok(
    variants.size >= 2,
    `dict key ${key} must localize across locales (got ${variants.size} distinct copies)`,
  );
}

console.log('turn_error_display_contract: ok');
