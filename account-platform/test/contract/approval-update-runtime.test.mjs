import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

test('approval draft replacement accepts its required version while retaining strict draft facts', async () => {
  const document = YAML.parse(await readFile(path.resolve(import.meta.dirname, '../../contracts/openapi/admin.yaml'), 'utf8'));
  // The existing draft permits evidence as an array or object. Keep every
  // data constraint strict while accepting that valid JSON Schema union.
  const ajv = new Ajv2020({ strict: true, allowUnionTypes: true, allErrors: true });
  addFormats(ajv);
  ajv.addFormat('int64', { type: 'number', validate: Number.isInteger });
  const replaceReferences = (value) => JSON.parse(JSON.stringify(value).replaceAll('#/components/schemas/', 'urn:pinvou:approval-update#/$defs/'));
  ajv.addSchema({ $id: 'urn:pinvou:approval-update', $defs: replaceReferences(document.components.schemas) });
  const create = ajv.compile(replaceReferences(document.paths['/account/admin/v1/approval-requests'].post.requestBody.content['application/json'].schema));
  const update = ajv.compile(replaceReferences(document.paths['/account/admin/v1/approval-requests/{approval_request_id}'].put.requestBody.content['application/json'].schema));
  const draft = {
    command_type: 'WALLET_ADJUSTMENT',
    idempotency_key: 'fixture-approval-draft',
    normalized_command: {
      adjustment_id: '00000000-0000-4000-8000-000000000001',
      wallet_id: '00000000-0000-4000-8000-000000000002',
      amount_microcredits: 100,
      requested_by: '00000000-0000-4000-8000-000000000003',
    },
    target_resource_type: 'MANUAL_ADJUSTMENT',
    target_resource_id: '00000000-0000-4000-8000-000000000001',
    reason: 'A controlled financial correction',
    redacted_evidence: [{ reference: 'fixture-evidence' }],
    expected_impact: { delta: 100 },
    critical_resource_versions: { adjustment_version: 1 },
    financial_snapshot: { available: 500 },
  };
  assert.ok(create(draft), JSON.stringify(create.errors));
  assert.ok(update({ ...draft, expected_version: 1 }), JSON.stringify(update.errors));
  assert.equal(create({ ...draft, expected_version: 1 }), false, 'creation does not accept an update version');
  assert.equal(update(draft), false, 'replacement requires the optimistic version');
  for (const expected_version of [0, -1, 1.5, '1', null]) {
    assert.equal(update({ ...draft, expected_version }), false, 'replacement requires a positive integer version');
  }
  assert.equal(update({ ...draft, expected_version: 1, unexpected: true }), false, 'replacement keeps unknown fields forbidden');
  for (const field of document.components.schemas.ApprovalDraft.required) {
    const missing = { ...draft, expected_version: 1 };
    delete missing[field];
    assert.equal(update(missing), false, `replacement still requires ${field}`);
  }
  for (const [field, invalid] of [
    ['command_type', 7], ['idempotency_key', 'short'], ['normalized_command', []],
    ['target_resource_type', ''], ['target_resource_id', ''], ['reason', 'short'],
    ['redacted_evidence', 'unstructured'], ['expected_impact', []],
    ['critical_resource_versions', []], ['financial_snapshot', []],
    ['supersedes_request_id', 'invalid-uuid'],
  ]) {
    assert.equal(update({ ...draft, expected_version: 1, [field]: invalid }), false, `replacement preserves the ${field} constraint`);
  }
});
