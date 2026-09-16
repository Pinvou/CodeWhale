import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const openapi = YAML.parse(await readFile(new URL('../../contracts/openapi/token-gateway.yaml', import.meta.url), 'utf8'));
const golden = JSON.parse(await readFile(new URL('../../internal/platform/approvalencoding/testdata/golden.json', import.meta.url), 'utf8'));
const ajv = new Ajv2020({ allErrors: true, strict: false });
addFormats(ajv);
function validator(name) {
  const schema = JSON.parse(JSON.stringify({ $schema: 'https://json-schema.org/draft/2020-12/schema', $ref: `#/$defs/${name}`, $defs: openapi.components.schemas }).replaceAll('#/components/schemas/', '#/$defs/'));
  return ajv.compile(schema);
}

test('Token proof transport remains closed and golden-compatible for all three commands', () => {
  const path = openapi.paths['/internal/v1/approval-command-proofs'].post;
  assert.equal(path.operationId, 'establishTokenApprovalProofInternal');
  const requestValid = validator('TokenApprovalProofRequest');
  const responseValid = validator('TokenApprovalProofResponse');
  const id = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  for (const vector of golden.proof_vectors.filter(item => item.command_type.startsWith('TOKEN_'))) {
    const request = {
      approval_request_id: id, command_type: vector.command_type,
      policy_version: vector.policy_version, proof_schema_version: '1',
      command_digest: vector.command_digest, snapshot_digest: vector.snapshot_digest,
      proof_input: JSON.parse(vector.proof_json),
    };
    assert.ok(requestValid(request), `${vector.name}: ${JSON.stringify(requestValid.errors)}`);
    const extra = structuredClone(request);
    extra.proof_input.critical_resource_versions.secret = 'forbidden';
    assert.equal(requestValid(extra), false, `${vector.name}: nested unknown key`);
    extra.proof_input.critical_resource_versions = request.proof_input.critical_resource_versions;
    extra.policy_version = 9;
    assert.equal(requestValid(extra), false, `${vector.name}: unsupported policy`);
    extra.policy_version = request.policy_version;
    extra.command_type = vector.command_type === 'TOKEN_CREDENTIAL_REVOKE' ? 'TOKEN_MODEL_LIFECYCLE_CHANGE' : 'TOKEN_CREDENTIAL_REVOKE';
    assert.equal(requestValid(extra), false, `${vector.name}: cross-command proof input`);
    const response = {
      approval_request_id: id, command_type: vector.command_type,
      command_digest: vector.command_digest, snapshot_digest: vector.snapshot_digest,
      policy_version: vector.policy_version, proof_schema_version: '1',
      domain_revision: `7:${'a'.repeat(64)}`, verdict: 'ALLOW', reason_code: 'PROOF_VALID',
      issued_at: '2026-09-16T01:00:00Z', expires_at: '2026-09-16T01:01:00Z',
    };
    assert.ok(responseValid(response), `${vector.name}: ${JSON.stringify(responseValid.errors)}`);
    assert.equal(responseValid({ ...response, proof_digest: 'untrusted' }), false);
  }
});
