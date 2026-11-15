import assert from 'node:assert/strict';
import test from 'node:test';
import { versionMeasurementError } from '../src/features/model-store/version-measurement-errors.js';
import { dictZh } from '../src/shared/i18n/zh.js';
import { dictEn } from '../src/shared/i18n/en.js';
import { dictJa } from '../src/shared/i18n/ja.js';

const codes = ['busy', 'path_unknown', 'snapshot_changed', 'version_changed']
  .map((suffix) => `version_measurement_${suffix}`);
const dictionaries = [dictZh.uiModelStore, dictEn.uiModelStore, dictJa.uiModelStore];

for (const code of codes) {
  test(`${code} has distinct owned translations in all three catalogs`, () => {
    const messages = dictionaries.map((copy) => versionMeasurementError(new Error(code), copy));
    assert.equal(new Set(messages).size, 3);
    for (const message of messages) {
      assert.equal(typeof message, 'string');
      assert.ok(message.length > 10);
      assert.ok(!message.includes('version_measurement_'));
    }
    assert.match(messages[0], /模型|版本/);
    assert.match(messages[1], /Model|model|version/i);
    assert.match(messages[2], /モデル|バージョン/);
  });
}

test('unknown errors and prototype names remain diagnostic strings', () => {
  for (const copy of dictionaries) {
    for (const message of ['Permission denied (os error 13)', '__proto__', 'toString']) {
      assert.equal(versionMeasurementError(message, copy), message);
    }
  }
});

test('a stored error uses the active catalog on each render', () => {
  const error = 'version_measurement_busy';
  const first = versionMeasurementError(error, dictionaries[0]);
  const next = versionMeasurementError(error, dictionaries[1]);
  assert.notEqual(first, next);
  assert.equal(next, dictionaries[1].versionMeasurementErrors[error]);
});
