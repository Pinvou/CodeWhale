import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { platformRoleCopy } from '../../../pinvou3-app/src/shared/platform-role-i18n.js';
import {
  checkAccountPlatformI18n,
  validateBuiltCatalog,
  validateCatalog,
  validateUserInterfaceSource,
  validatePlatformRoleUserInterfaceSource,
  builtJavaScript,
} from '../../../scripts/check-account-platform-i18n.mjs';

test('account platform source catalogs and UI surfaces pass the release gate', async () => {
  assert.deepEqual(await checkAccountPlatformI18n({ artifacts: false }), { catalogs: 6, sources: 26 });
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


test('platform role copy has all three complete locales without empty or fallback keys', () => {
  assert.doesNotThrow(() => validateCatalog('platform-roles', platformRoleCopy));
  for (const locale of ['zh', 'en', 'ja']) {
    const missing = structuredClone(platformRoleCopy);
    delete missing[locale].reloadBoundary;
    assert.throws(() => validateCatalog('platform-roles', missing), /i18n_key_mismatch/);
    const empty = structuredClone(platformRoleCopy);
    empty[locale].unknown = '';
    assert.throws(() => validateCatalog('platform-roles', empty), /i18n_empty_translation/);
  }
  const fallback = structuredClone(platformRoleCopy);
  delete fallback.ja;
  assert.throws(() => validateCatalog('platform-roles', fallback), /i18n_locale_mismatch/);
});

test('actual platform role UI uses the shared locale directly without language or raw error fallback', async () => {
  const source = await readFile(new URL('../../../pinvou3-app/src/features/account-platform/AdminRolesWorkspace.jsx', import.meta.url), 'utf8');
  assert.doesNotThrow(() => validatePlatformRoleUserInterfaceSource(source));
  assert.doesNotThrow(() => validateUserInterfaceSource('AdminRolesWorkspace.jsx', source));
  assert.throws(() => validatePlatformRoleUserInterfaceSource(source.replace('platformRoleCopy[language];', 'platformRoleCopy[language] || platformRoleCopy.en;')), /i18n_platform_roles_source_mismatch/);
  assert.throws(() => validateUserInterfaceSource('AdminRolesWorkspace.jsx', source + '\nconst column = {label:"Grant role"};'), /i18n_single_language_property/);
  assert.throws(() => validateUserInterfaceSource('AdminRolesWorkspace.jsx', source + '\nconst message = reason?.message && reason.message;'), /i18n_raw_error_fallback/);
});

test('platform role source accepts the original named binding among other imports', () => {
  for (const bindings of [
    'platformRoleCopy',
    'adminRolePresentationCopy, platformRoleCopy',
    'platformRoleCopy, adminRolePresentationCopy',
    '\n  adminRolePresentationCopy,\n  platformRoleCopy,\n',
    'platformRoleCopy as platformRoleCopy,',
  ]) {
    assert.doesNotThrow(() => validatePlatformRoleUserInterfaceSource(
      `import { ${bindings} } from '../../shared/i18n.js';\nconst copy = platformRoleCopy[language];`));
  }
});

test('platform role source rejects wrong modules, exported names and renamed local bindings', () => {
  for (const declaration of [
    "import { platformRoleCopy } from '../../shared/other-i18n.js';",
    "import { adminRolePresentationCopy as platformRoleCopy } from '../../shared/i18n.js';",
    "import { platformRoleCopy as wrongLocal } from '../../shared/i18n.js';",
    "import { platformRoleCopyExtra } from '../../shared/i18n.js';",
    "import { adminRolePresentationCopy as platformRoleCopy, platformRoleCopy as wrongLocal } from '../../shared/i18n.js';",
    "// import { platformRoleCopy } from '../../shared/i18n.js';",
    "const platformRoleCopy = anotherCatalog;",
  ]) {
    assert.throws(() => validatePlatformRoleUserInterfaceSource(
      `${declaration}\nconst copy = platformRoleCopy[language];`), /i18n_platform_roles_source_mismatch/);
  }
});

test('platform role translations must be reachable from the admin HTML chunk graph', async () => {
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-role-i18n-carrier-'));
  await mkdir(path.join(directory, 'assets'));
  try {
    const strings = validateCatalog('platform-roles', platformRoleCopy);
    await writeFile(path.join(directory, 'admin.html'), '<script src="/assets/admin.js"></script>');
    await writeFile(path.join(directory, 'account.html'), '<script src="/assets/account.js"></script>');
    await writeFile(path.join(directory, 'portal-session-handoff.html'), '<script src="/assets/account.js"></script>');
    await writeFile(path.join(directory, 'assets/admin.js'), 'const placeholder = "admin";');
    await writeFile(path.join(directory, 'assets/account.js'), JSON.stringify([...strings]));
    await writeFile(path.join(directory, 'assets/stale.js'), JSON.stringify([...strings]));
    assert.doesNotThrow(() => validateBuiltCatalog('platform-roles', strings, JSON.stringify([...strings])));
    assert.throws(() => validateBuiltCatalog('platform-roles.admin', strings, ''), /artifact_translation_missing/);
    const adminOnly = await builtJavaScript(directory, ['admin.html']);
    assert.throws(() => validateBuiltCatalog('platform-roles.admin', strings, adminOnly), /artifact_translation_missing/);
    // The all-entry carrier accepts account-only translations; the admin graph cannot.
    const allEntries = await builtJavaScript(directory);
    assert.doesNotThrow(() => validateBuiltCatalog('platform-roles', strings, allEntries));
    await writeFile(path.join(directory, 'assets/admin.js'), 'import "./roles-current.js";');
    await writeFile(path.join(directory, 'assets/roles-current.js'), JSON.stringify([...strings]));
    const currentAdmin = await builtJavaScript(directory, ['admin.html']);
    assert.doesNotThrow(() => validateBuiltCatalog('platform-roles.admin', strings, currentAdmin));
    await assert.rejects(builtJavaScript(directory, ['../admin.html']), /artifact_entry_invalid/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
