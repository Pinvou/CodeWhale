import assert from 'node:assert/strict';
import test from 'node:test';
import { formulaDraftCommand, protocolDraftCommand } from '../src/features/account-platform/admin-protocol-formula-state.js';
import { tokenManagementCopy } from '../src/shared/token-management-i18n.js';

const reason = 'Independent version validation';
const protocol = { protocolCode: ' chat_v2 ', requestSchema: '{"type":"object"}', expectedHeadVersion: 2, reason };
const rows = [{ unitCode: 'REQUEST', unitDefinitionID: '11111111-1111-4111-8111-111111111111', input: 'request_count' }];
const formula = { rows, expectedProtocolVersion: 2, expectedFormulaHeadVersion: 3, reason };
const fields = [
  { name: 'protocol-head', build: (value) => protocolDraftCommand({ ...protocol, expectedHeadVersion: value }), field: 'expected_protocol_head_version', minimum: 0 },
  { name: 'formula-protocol', build: (value) => formulaDraftCommand({ ...formula, expectedProtocolVersion: value }), field: 'expected_protocol_version', minimum: 1 },
  { name: 'formula-head', build: (value) => formulaDraftCommand({ ...formula, expectedFormulaHeadVersion: value }), field: 'expected_formula_head_version', minimum: 0 },
];

test('valid draft payloads retain existing schema, row, reason and version facts', () => {
  assert.deepEqual(protocolDraftCommand(protocol), { value: { protocol_code: 'CHAT_V2', request_schema: { type: 'object' }, expected_protocol_head_version: 2, reason } });
  assert.deepEqual(formulaDraftCommand(formula), { value: { required_unit_versions: { REQUEST: rows[0].unitDefinitionID }, expression: { REQUEST: 'request_count' }, expected_protocol_version: 2, expected_formula_head_version: 3, reason } });
});

for (const { name, build, field, minimum } of fields) {
  test(`${name} emits only exact finite safe integer version facts`, () => {
    for (const value of [minimum, 42, Number.MAX_SAFE_INTEGER, '42', ' 0042 ', '+42']) {
      const result = build(value);
      assert.equal(result.error, undefined);
      assert.equal(result.value[field], Number(value));
      assert.equal(Number.isSafeInteger(result.value[field]), true);
      assert.deepEqual(JSON.parse(JSON.stringify(result)), result);
    }
  });
  test(`${name} rejects invalid facts without producing a command`, () => {
    const invalid = [NaN, Infinity, -Infinity, -1, 1.25, Number.MAX_SAFE_INTEGER + 1, 'NaN', 'Infinity', '9007199254740991.1', '', ' ', '1.25', true, false, null, [], [1], {}, Symbol('version'), 1n];
    if (minimum === 1) invalid.push(0, '0', undefined);
    for (const value of invalid) {
      const result = build(value);
      assert.deepEqual(result, { error: 'invalidVersion' }, `${name} accepted an invalid ${typeof value} fact`);
      assert.equal(Object.hasOwn(result, 'value'), false);
    }
  });
}

test('first drafts preserve zero and omitted-head compatibility without clamping negative facts', () => {
  for (const { build, field } of fields.filter((item) => item.minimum === 0)) {
    for (const value of [0, -0, '0', undefined]) {
      assert.equal(Object.is(build(value).value[field], 0), true);
    }
    assert.deepEqual(build(-1), { error: 'invalidVersion' });
  }
});

test('existing validation errors retain precedence and valid source data remains untouched', () => {
  assert.deepEqual(protocolDraftCommand({ ...protocol, requestSchema: 'not-json', expectedHeadVersion: NaN }), { error: 'invalidSchema' });
  assert.deepEqual(protocolDraftCommand({ ...protocol, protocolCode: '', expectedHeadVersion: NaN }), { error: 'protocolCodeRequired' });
  assert.deepEqual(protocolDraftCommand({ ...protocol, reason: 'short', expectedHeadVersion: NaN }), { error: 'reasonTooShort' });
  assert.deepEqual(formulaDraftCommand({ ...formula, rows: [], expectedProtocolVersion: NaN }), { error: 'invalidFormulaRows' });
  assert.deepEqual(formulaDraftCommand({ ...formula, rows: [rows[0], rows[0]], expectedProtocolVersion: NaN }), { error: 'duplicateUnitCode' });
  assert.deepEqual(formulaDraftCommand({ ...formula, reason: 'short', expectedProtocolVersion: NaN }), { error: 'reasonTooShort' });
  assert.equal(protocol.expectedHeadVersion, 2);
  assert.equal(formula.expectedProtocolVersion, 2);
  assert.equal(formula.expectedFormulaHeadVersion, 3);
});

test('the real dialog validation error has actionable text in all supported locales', () => {
  for (const locale of ['zh', 'en', 'ja']) {
    assert.equal(typeof tokenManagementCopy[locale].modelToken.validationErrors.invalidVersion, 'string');
    assert.notEqual(tokenManagementCopy[locale].modelToken.validationErrors.invalidVersion.trim(), '');
  }
  assert.match(tokenManagementCopy.zh.modelToken.validationErrors.invalidVersion, /刷新/);
  assert.match(tokenManagementCopy.en.modelToken.validationErrors.invalidVersion, /Refresh/);
  assert.match(tokenManagementCopy.ja.modelToken.validationErrors.invalidVersion, /更新/);
});
