import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { canonicalApprovalJSON, canonicalInt64, approvalSHA256, parseLegacyDomainJSON } from '../helpers/approval-canonical-oracle.mjs';

test('approval independent oracle retains full signed int64 without Number conversion', () => {
  for (const text of ['-9223372036854775808', '9223372036854775807', '9007199254740993', '-1', '0', '1']) {
    assert.equal(canonicalInt64(text).toString(), text);
    assert.equal(canonicalApprovalJSON({ value: text }), `{"value":"${text}"}`);
  }
  for (const text of ['9223372036854775808', '-9223372036854775809', '-0', '+1', '01', '-01', '1.0', '1e3', ' 1', '1 ', '', '١']) {
    assert.throws(() => canonicalInt64(text), undefined, text);
  }
  assert.throws(() => canonicalInt64(1));
  assert.throws(() => canonicalApprovalJSON({ value: 9007199254740992 }));
  assert.deepEqual(parseLegacyDomainJSON('{"a":9223372036854775807,"b":-9223372036854775808,"c":null}'),
    { a: '9223372036854775807', b: '-9223372036854775808', c: null });
});

test('approval Go and JavaScript share exact legacy-to-proof bytes and digests', () => {
  const fixture = JSON.parse(readFileSync(new URL('../../internal/platform/approvalencoding/testdata/golden.json', import.meta.url), 'utf8'));
  assert.equal(fixture.schema_version, 1);
  assert.ok(fixture.proof_vectors.length >= 6);
  const commands = new Set();
  for (const vector of fixture.proof_vectors) {
    commands.add(vector.command_type);
    const proof = JSON.parse(vector.proof_json);
    const normalized = parseLegacyDomainJSON(vector.legacy.normalized_json);
    const critical = parseLegacyDomainJSON(vector.legacy.critical_json);
    const financial = parseLegacyDomainJSON(vector.legacy.financial_json);
    if (vector.command_type === 'TOKEN_MODEL_LIFECYCLE_CHANGE' && !Object.hasOwn(normalized, 'lifecycle_plan_id')) {
      assert.equal(critical.lifecycle_plan_version, '0');
      assert.equal(Object.hasOwn(normalized, 'lifecycle_plan_version'), false);
      normalized.lifecycle_plan_id = null;
      normalized.lifecycle_plan_version = '0';
    }
    assert.deepEqual(proof, {
      snapshot_schema_version: vector.snapshot_schema_version,
      target: { resource_type: vector.legacy.target_resource_type, resource_id: vector.legacy.target_resource_id },
      normalized_command: normalized,
      critical_resource_versions: critical,
      financial_snapshot: financial,
    }, vector.name);
    assert.equal(canonicalApprovalJSON(proof), vector.proof_json, vector.name);
    const command = canonicalApprovalJSON({ command_type: vector.command_type, policy_version: vector.policy_version,
      target: proof.target, normalized_command: proof.normalized_command });
    const snapshot = canonicalApprovalJSON({ snapshot_schema_version: proof.snapshot_schema_version,
      critical_resource_versions: proof.critical_resource_versions, financial_snapshot: proof.financial_snapshot });
    assert.equal(command, vector.command_jcs, vector.name);
    assert.equal(snapshot, vector.snapshot_jcs, vector.name);
    assert.equal(approvalSHA256(command), vector.command_digest, vector.name);
    assert.equal(approvalSHA256(snapshot), vector.snapshot_digest, vector.name);
  }
  assert.deepEqual([...commands].sort(), ['WALLET_ADJUSTMENT', 'PAYMENT_PROVIDER_RESUME', 'PAYMENT_PRODUCT_RESUME',
    'TOKEN_MODEL_LIFECYCLE_CHANGE', 'TOKEN_MODEL_CATALOG_ROLLBACK', 'TOKEN_CREDENTIAL_REVOKE'].sort());
});

test('approval shared authorization vectors bind exact raw bytes without recanonicalizing legacy events', () => {
  const fixture = JSON.parse(readFileSync(new URL('../../internal/platform/approvalencoding/testdata/golden.json', import.meta.url), 'utf8'));
  assert.ok(fixture.authorization_vectors.length >= 2);
  const formats = new Set();
  for (const vector of fixture.authorization_vectors) {
    formats.add(vector.serialization_version);
    assert.equal(approvalSHA256(vector.event_json), vector.event_digest, vector.name);
    canonicalInt64(vector.approved_request_version);
    canonicalInt64(vector.approval_epoch);
    if (vector.serialization_version === 'APPROVAL_RFC8785_DECIMAL_V1') {
      const event = JSON.parse(vector.event_json);
      assert.equal(event.serialization_version, vector.serialization_version);
      assert.equal(canonicalApprovalJSON(event), vector.event_json, vector.name);
      assert.equal(event.aggregate_version, vector.approved_request_version, vector.name);
      assert.equal(event.payload.approval_control_epoch, vector.approval_epoch, vector.name);
    } else {
      assert.equal(vector.serialization_version, 'LEGACY_TYPED_V1');
      const event = parseLegacyDomainJSON(vector.event_json);
      assert.equal(Object.hasOwn(event, 'serialization_version'), false);
      assert.equal(event.aggregate_version, vector.approved_request_version, vector.name);
    }
  }
  assert.deepEqual([...formats].sort(), ['APPROVAL_RFC8785_DECIMAL_V1', 'LEGACY_TYPED_V1']);
});

test('approval proof digest derives from exactly the eleven response fields', () => {
  const fixture = JSON.parse(readFileSync(new URL('../../internal/platform/approvalencoding/testdata/golden.json', import.meta.url), 'utf8'));
  assert.ok(fixture.proof_response_vectors.length > 0);
  const keys = ['approval_request_id', 'command_type', 'command_digest', 'snapshot_digest', 'policy_version',
    'proof_schema_version', 'domain_revision', 'verdict', 'reason_code', 'issued_at', 'expires_at'].sort();
  for (const vector of fixture.proof_response_vectors) {
    const response = JSON.parse(vector.response_json);
    assert.deepEqual(Object.keys(response).sort(), keys, vector.name);
    assert.equal(approvalSHA256(canonicalApprovalJSON(response)), vector.proof_digest, vector.name);
  }
});

test('approval independent oracle preserves Unicode and uses UTF-16 key order', () => {
  const text = '<>&/\u2028\u2029😀e\u0301';
  assert.equal(canonicalApprovalJSON({ text }), '{"text":"' + text + '"}');
  assert.notEqual(canonicalApprovalJSON({ text: 'e\u0301' }), canonicalApprovalJSON({ text: 'é' }));
  assert.equal(canonicalApprovalJSON({ '\ue000': 'later', '😀': 'first' }), '{"😀":"first","\ue000":"later"}');
  assert.equal(canonicalApprovalJSON({ text: '\b\t\n\f\r\u0000"\\' }), '{"text":"\\b\\t\\n\\f\\r\\u0000\\"\\\\"}');
  assert.throws(() => canonicalApprovalJSON({ text: '\ud800' }));
  assert.throws(() => canonicalApprovalJSON({ text: '\udfff' }));
});
