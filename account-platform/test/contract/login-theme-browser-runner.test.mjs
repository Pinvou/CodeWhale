import assert from 'node:assert/strict';
import test from 'node:test';
import { verifyBrowserEvidence } from '../../../scripts/test-account-platform-login-theme-windows.mjs';
const names = [
  'current Keycloak theme selected asset hashes and projected FTL controls agree',
  'native browser executes current Keycloak theme password protections',
  'manual typing uses one explicit mirror and blur clears accessible text',
  'initial saved or script-replaced values remain protected',
  'native Unicode editing and IME composition preserve manual provenance',
  'form reset retires old reveal state before new manual input',
  'maxlength nested microtask edit cannot reuse manual authorization',
  'Microsoft Edge displays only the current custom password reveal icon',
];
// Public synthetic TAP verifies the runner contract only; these are not browser executions.
const good = names.map((name, i) => `ok ${i + 1} - ${name}`).join('\n') + '\n# tests 8\n# pass 8\n# skipped 0\n';
test('explicit Windows browser evidence gate cannot accept incomplete or skipped execution', async t => {
  await t.test('complete named evidence accepted', () => assert.doesNotThrow(() => verifyBrowserEvidence(good)));
  await t.test('each missing native child rejected', () => {
    for (const name of names.slice(2, 7)) assert.throws(() => verifyBrowserEvidence(good.split('\n').filter(line => !line.endsWith(name)).join('\n')), /EVIDENCE_INCOMPLETE/);
  });
  await t.test('missing Edge root rejected', () => assert.throws(() => verifyBrowserEvidence(good.split('\n').filter(line => !line.endsWith(names[7])).join('\n')), /EVIDENCE_INCOMPLETE/));
  await t.test('skip summary rejected', () => assert.throws(() => verifyBrowserEvidence(good.replace('# skipped 0', '# skipped 1')), /EVIDENCE_INCOMPLETE/));
  await t.test('skipped named event rejected even with false zero summary', () => assert.throws(() => verifyBrowserEvidence(good.replace(names[2], names[2] + ' # SKIP')), /EVIDENCE_INCOMPLETE/));
  await t.test('duplicate child rejected', () => assert.throws(() => verifyBrowserEvidence(good + `ok 9 - ${names[2]}\n`), /EVIDENCE_INCOMPLETE/));
  await t.test('zero executed or parent-only evidence rejected', () => {
    assert.throws(() => verifyBrowserEvidence('# tests 0\n# pass 0\n# skipped 0\n'), /EVIDENCE_INCOMPLETE/);
    assert.throws(() => verifyBrowserEvidence(good.split('\n').filter(line => !names.slice(2, 7).some(name => line.endsWith(name))).join('\n')), /EVIDENCE_INCOMPLETE/);
  });
});
