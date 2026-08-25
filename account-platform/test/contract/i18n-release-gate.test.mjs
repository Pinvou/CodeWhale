import assert from 'node:assert/strict';
import test from 'node:test';
import {
  checkAccountPlatformI18n,
  validateBuiltCatalog,
  validateCatalog,
  validateUserInterfaceSource,
} from '../../../scripts/check-account-platform-i18n.mjs';

test('account platform source catalogs and UI surfaces pass the release gate', async () => {
  assert.deepEqual(await checkAccountPlatformI18n({ artifacts: false }), { catalogs: 3, sources: 5 });
});

test('a missing, extra, empty, or fallback translation blocks release', () => {
  const complete = { zh:{ title:'中文' }, en:{ title:'English' }, ja:{ title:'日本語' } };
  assert.doesNotThrow(() => validateCatalog('fixture', complete));
  assert.throws(() => validateCatalog('fixture', { ...complete, ja:{} }), /i18n_key_mismatch/);
  assert.throws(() => validateCatalog('fixture', { ...complete, en:{ title:'', extra:'x' } }), /i18n_key_mismatch|i18n_empty_translation/);
  assert.throws(() => validateCatalog('fixture', { ...complete, en:{ title:null } }), /i18n_type_mismatch/);
});

test('single-language UI literals and raw backend errors block release', () => {
  assert.throws(() => validateUserInterfaceSource('fixture.jsx', 'return <p>单语文案</p>'), /i18n_single_language_literal/);
  assert.throws(() => validateUserInterfaceSource('fixture.jsx', "const column={label:'Usage detail'}"), /i18n_single_language_property/);
  assert.throws(() => validateUserInterfaceSource('fixture.jsx', 'return known || (reason?.message && reason.message)'), /i18n_raw_error_fallback/);
});

test('a release artifact missing a translated value is rejected', () => {
  assert.doesNotThrow(() => validateBuiltCatalog('fixture', new Set(['one', 'two']), 'zero one two three'));
  assert.throws(() => validateBuiltCatalog('fixture', new Set(['one', 'two']), 'one'), /i18n_artifact_translation_missing/);
});
