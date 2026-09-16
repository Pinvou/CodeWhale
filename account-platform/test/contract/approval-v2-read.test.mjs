import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import YAML from 'yaml';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const read = (relative) => readFileSync(path.join(root, relative), 'utf8');

test('v2 approval reads are versioned, closed, and classified by current actor', () => {
  const api = YAML.parse(read('contracts/openapi/admin.yaml'));
  const service = read('internal/account/service.go');
  const catalog = JSON.parse(read('deploy/authorization-entrypoints.json'));
  const list = api.paths['/account/admin/v2/approval-requests'];
  const detail = api.paths['/account/admin/v2/approval-requests/{approval_request_id}'];
  assert.deepEqual(Object.keys(list), ['get']);
  assert.deepEqual(Object.keys(detail), ['get']);
  assert.match(service, /adminV2\.GET\("\/approval-requests", httpserver\.AdaptFunc\(s\.adminApprovalRequestsV2\)\)/);
  assert.match(service, /adminV2\.GET\("\/approval-requests\/:approval_request_id", httpserver\.AdaptFunc\(s\.adminApprovalRequestV2\)\)/);
  for (const resource of ['/approval-requests', '/approval-requests/:approval_request_id']) {
    assert.ok(catalog.http_policies.some((policy) => policy.receiver === 'adminV2' && policy.path === resource && policy.strategy.startsWith('domain.approval-policy-v2-')));
  }
  const schemas = api.components.schemas;
  for (const name of ['ApprovalV2Page', 'ApprovalV2Summary', 'ApprovalV2KnownDetail', 'ApprovalV2WalletCommand',
    'ApprovalV2ProviderCommand', 'ApprovalV2ProductCommand', 'ApprovalV2LifecycleCommand',
    'ApprovalV2CatalogCommand', 'ApprovalV2CredentialCommand']) {
    assert.equal(schemas[name].additionalProperties, false, name);
  }
  assert.equal(schemas.ApprovalV2KnownDetail.oneOf.length, 6);
  assert.ok(!('allowed_actions' in schemas.ApprovalV2Summary.properties));
  assert.ok(!('critical_resource_versions' in schemas.ApprovalV2Summary.properties));
  assert.ok(!('normalized_command' in schemas.ApprovalV2Summary.properties));
  assert.ok(list.get.responses['410'] && list.get.responses['422'] && list.get.responses['429'] && list.get.responses['504']);
  assert.ok(api.paths['/account/admin/v1/approval-requests/{approval_request_id}'].get.responses['200'].headers['X-Pinvou-Approval-Action-Control-Version']);
});

test('closed v2 detail schema accepts six presenters but rejects raw or mismatched commands', () => {
  const schemas = YAML.parse(read('contracts/openapi/admin.yaml')).components.schemas;
  const ajv = new Ajv2020({ strict: false, allErrors: true });
  addFormats(ajv);
  const valid = ajv.compile({ $ref: '#/components/schemas/ApprovalV2Detail', components: { schemas } });
  const id = '11111111-1111-4111-8111-111111111111';
  const base = { approval_request_id: id, risk_class: 'HIGH', target: { resource_type: 'MODEL', resource_id: id },
    requester: { user_id: id }, status: 'SUBMITTED', submitted_at: null, expires_at: '2026-09-16T12:00:00Z',
    updated_at: '2026-09-16T11:00:00Z', supersedes_request_id: null, detail_availability: 'AVAILABLE',
    allowed_actions: ['APPROVE'], action_control_version: 1, execution: null };
  const cases = [
    ['WALLET_ADJUSTMENT', 1, 1, { adjustment_id: id, wallet_id: id, amount_microcredits: '123' }],
    ['PAYMENT_PROVIDER_RESUME', 2, 1, { provider_id: 'provider-main', provider_control_version: '1' }],
    ['PAYMENT_PRODUCT_RESUME', 1, 1, { product_id: 'product-main', provider_id: 'provider-main', product_control_version: '1' }],
    ['TOKEN_MODEL_LIFECYCLE_CHANGE', 2, 2, { model_id: id, target_lifecycle: 'DEPRECATED', effective_at: null }],
    ['TOKEN_MODEL_CATALOG_ROLLBACK', 2, 2, { source_catalog_version: '17' }],
    ['TOKEN_CREDENTIAL_REVOKE', 1, 1, { credential_id: id, token_account_id: id }],
  ];
  for (const [command_type, policy_version, snapshot_schema_version, command] of cases) {
    const response = { ...base, command_type, policy_version, snapshot_schema_version, command };
    assert.equal(valid(response), true, `${command_type}: ${JSON.stringify(valid.errors)}`);
    assert.equal(valid({ ...response, normalized_command: { secret: 'no' } }), false, command_type);
    assert.equal(valid({ ...response, command: { ...command, secret: 'no' } }), false, command_type);
    assert.equal(valid({ ...response, policy_version: policy_version + 1 }), false, command_type);
	assert.equal(valid({ ...response, execution: { attempt_status: 'FAILED', error_class: 'EXECUTION_FAILED', retryable: false, next_retry_at: null, execution_deadline: null } }), true, command_type);
	assert.equal(valid({ ...response, execution: { attempt_status: 'FAILED', error_class: 'raw-provider-failure', retryable: true, next_retry_at: null, execution_deadline: null } }), false, command_type);
  }
});

test('v2 decision has a separate closed body and the same governed repository action', () => {
  const api = YAML.parse(read('contracts/openapi/admin.yaml'));
  const schema = api.components.schemas.ApprovalV2Decision;
  const path = api.paths['/account/admin/v2/approval-requests/{approval_request_id}/actions/decide'];
  const catalog = JSON.parse(read('deploy/authorization-entrypoints.json'));
  assert.deepEqual(Object.keys(path), ['post']);
  assert.equal(path.post.requestBody.content['application/json'].schema.$ref, '#/components/schemas/ApprovalV2Decision');
  assert.equal(schema.additionalProperties, false);
  assert.deepEqual(schema.required, ['expected_version', 'action_control_version', 'decision', 'reason']);
  const ajv = new Ajv2020({ strict: false, allErrors: true });
  const validate = ajv.compile(schema);
  const body = { expected_version: 1, action_control_version: 1, decision: 'APPROVED', reason: 'independent approval' };
  assert.equal(validate(body), true);
  for (const field of ['observed_versions', 'proof', 'proof_digest', 'domain_revision', 'verdict']) {
    assert.equal(validate({ ...body, [field]: 'untrusted' }), false, field);
  }
  assert.ok(catalog.http_policies.some((policy) => policy.receiver === 'adminV2' &&
    policy.method === 'POST' && policy.path === '/approval-requests/:approval_request_id/actions/decide'));
  assert.ok(read('internal/account/approvals_http.go').includes('requestID, decision.ExpectedVersion, decision.Decision, decision.Reason, nil'));
});
