import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import {
  checkAccountPlatformI18n,
  validateBuiltCatalog,
  validateCatalog,
  validateUserInterfaceSource,
  builtJavaScript,
} from '../../../scripts/check-account-platform-i18n.mjs';

test('account platform source catalogs and UI surfaces pass the release gate', async () => {
  assert.deepEqual(await checkAccountPlatformI18n({ artifacts: false }), { catalogs: 3, sources: 7 });
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

test('built catalogs follow canonical HTML chunk references, not stale unreferenced translations', async () => {
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-i18n-carrier-'));
  await mkdir(path.join(directory, 'assets'));
  try {
    for (const entry of ['account.html', 'admin.html', 'portal-session-handoff.html']) {
      await writeFile(path.join(directory, entry), '<script src="/assets/main-current.js"></script>');
    }
    await writeFile(path.join(directory, 'assets/main-current.js'), 'import "./locale-current.js";');
    await writeFile(path.join(directory, 'assets/locale-current.js'), 'current translation');
    await writeFile(path.join(directory, 'assets/stale.js'), 'missing translation');
    const carrier = await builtJavaScript(directory);
    assert.ok(carrier.includes('current translation'));
    assert.ok(!carrier.includes('missing translation'));
    assert.throws(() => validateBuiltCatalog('fixture', new Set(['missing translation']), carrier), /artifact_translation_missing/);
    await rm(path.join(directory, 'assets/locale-current.js'));
    await assert.rejects(builtJavaScript(directory), /ENOENT/);
    await writeFile(path.join(directory, 'assets/locale-current.js'), 'current translation');
    await writeFile(path.join(directory, 'account.html'), '<script src="/assets/../escape.js"></script>');
    await assert.rejects(builtJavaScript(directory), /artifact_path_invalid/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
