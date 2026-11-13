import assert from 'node:assert/strict';
import test from 'node:test';
import { dictZh } from '../src/shared/i18n/zh.js';
import { dictEn } from '../src/shared/i18n/en.js';
import { dictJa } from '../src/shared/i18n/ja.js';
import { checkCardUpdate, fetchStorageSettings, saveStorageSettings } from '../src/features/model-store/model-store-api.js';

test('the independent startup preference roundtrips false and does not block manual checks', async (t) => {
  const previous = globalThis.__TAURI__;
  t.after(() => { globalThis.__TAURI__ = previous; });
  const calls = [];
  let enabled = true;
  globalThis.__TAURI__ = { core: { invoke: async (command, payload) => {
    calls.push([command, payload]);
    if (command === 'model_store_set_storage_settings') {
      if (payload.startupUpdateCheckEnabled !== null) enabled = payload.startupUpdateCheckEnabled;
      return { startup_update_check_enabled: enabled };
    }
    if (command === 'model_store_storage_settings') return { startup_update_check_enabled: enabled };
    if (command === 'model_store_check_card_update') return { card_id: payload.cardId, changed: true };
    throw Error(`Unexpected command: ${command}`);
  } } };
  assert.equal((await saveStorageSettings({ startupUpdateCheckEnabled: false })).startup_update_check_enabled, false);
  assert.deepEqual(calls[0], ['model_store_set_storage_settings', { modelsDir: null, importScanDirs: null, startupUpdateCheckEnabled: false }]);
  assert.equal((await fetchStorageSettings()).startup_update_check_enabled, false);
  await saveStorageSettings({ importScanDirs: ['/keep/import'] });
  assert.equal((await fetchStorageSettings()).startup_update_check_enabled, false, 'old callers must omit rather than reset the policy');
  assert.equal((await checkCardUpdate('installed-card')).changed, true);
  assert.deepEqual(calls.at(-1), ['model_store_check_card_update', { cardId: 'installed-card' }]);
});

test('legacy unverified matching and startup network controls have explicit distinct copy in all locales', () => {
  const copies = [dictZh, dictEn, dictJa].map((dictionary) => dictionary.uiModelStore);
  for (const copy of copies) {
    for (const key of ['compatibilityLegacyUnverified', 'startupUpdateCheckLabel', 'startupUpdateCheckHint']) {
      assert.ok(Object.hasOwn(copy, key) && copy[key].length > 10, `missing translated ${key}`);
    }
    assert.notEqual(copy.compatibilityLegacyUnverified, copy.compatibilityKinds.compatible);
    assert.ok(copy.startupUpdateCheckHint.includes('IP'));
    assert.ok(copy.startupUpdateCheckHint.includes('ID'));
  }
  for (const key of ['compatibilityLegacyUnverified', 'startupUpdateCheckLabel', 'startupUpdateCheckHint']) {
    assert.equal(new Set(copies.map((copy) => copy[key])).size, 3, `all locales need their own ${key}`);
  }
});
