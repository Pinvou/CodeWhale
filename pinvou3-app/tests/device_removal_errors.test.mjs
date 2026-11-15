import assert from 'node:assert/strict';
import test from 'node:test';
import { deviceModelRemovalError } from '../src/features/device-manager/removal-errors.js';
import { dictZh } from '../src/shared/i18n/zh.js';
import { dictEn } from '../src/shared/i18n/en.js';
import { dictJa } from '../src/shared/i18n/ja.js';

const copies = [dictZh.uiDeviceMgr, dictEn.uiDeviceMgr, dictJa.uiDeviceMgr];
const diagnostic = 'Permission denied (os error 13): /tmp/weights\nretry source retained';
const owned = [
  ['model_store_weight_removal_busy', copy => copy.weightRemovalBusy],
  [`model_store_weight_removal_lock_failed: ${diagnostic}`, copy => copy.weightRemovalLockFailed(diagnostic)],
  ['model_store_weight_path_invalid', copy => copy.weightRemovalPathInvalid],
  [`model_store_weight_path_unavailable: ${diagnostic}`, copy => copy.weightRemovalPathUnavailable(diagnostic)],
];

for (const [message, expected] of owned) {
  test(`${message.split(':')[0]} uses the real zh/en/ja catalog and retains retry/state guidance`, () => {
    const actual = copies.map(copy => deviceModelRemovalError(new Error(message), copy));
    assert.equal(new Set(actual).size, 3, 'all three locales own distinct copy');
    actual.forEach((value, index) => {
      assert.equal(value, expected(copies[index]));
      assert.doesNotMatch(value, /model_store_weight_/);
      assert.match(value, /登记和文件|registration and files|登録とファイル/);
      assert.match(value, /重试|retry|Try again|再試行/);
      if (message.includes(': ')) assert.ok(value.endsWith(diagnostic), 'native diagnostic tail stays intact');
    });
  });
}

test('unknown and wrapped diagnostics never match owned codes by substring', () => {
  for (const copy of copies) {
    for (const message of [
      diagnostic, '__proto__', 'toString',
      'model_store_weight_removal_busy: other failure',
      'model_store_weight_removal_lock_failed',
      'model_store_weight_path_unavailable:missing separator',
      'outer guard: model_store_weight_path_invalid',
    ]) assert.equal(deviceModelRemovalError(message, copy), message);
  }
});

test('the caller can translate a pending failure with the current catalog', () => {
  const failure = new Error('model_store_weight_removal_busy');
  const first = deviceModelRemovalError(failure, copies[0]);
  const afterLocaleChange = deviceModelRemovalError(failure, copies[1]);
  assert.notEqual(first, afterLocaleChange);
  assert.equal(afterLocaleChange, copies[1].weightRemovalBusy);
});
