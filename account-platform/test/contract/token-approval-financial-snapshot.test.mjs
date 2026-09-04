import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');
const compile = (schemas, name) => {
  const document = JSON.parse(JSON.stringify({ $schema: 'https://json-schema.org/draft/2020-12/schema', $ref: `#/$defs/${name}`, $defs: schemas }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
};

const id = (head) => `${head.repeat(8)}-${head.repeat(4)}-4${head.repeat(3)}-8${head.repeat(3)}-${head.repeat(12)}`;
const digest = (head) => head.repeat(64);
const critical = {
  model_id: id('1'), catalog_version: 7, current_lifecycle: 'PUBLISHED', target_lifecycle: 'DEPRECATED', lifecycle_plan_version: 0,
  effective_at: '2026-09-04T08:00:00Z', existing_session_cutoff_at: null, replacement_model_id: null, billing_price_version: 11,
  route_set_fingerprint: digest('a'), provisioning_set_fingerprint: digest('b'), inflight_request_set_fingerprint: digest('c'), policy_version: 2,
};
const financial = { billing_price_revision: 11, price_set_fingerprint: digest('d'), models_checked: 1, models_covered: 1, fully_covered: true, billing_as_of: '2026-09-04T08:00:01Z' };

test('Billing unit validation exposes a closed authoritative snapshot', async () => {
  const contract = YAML.parse(await read('contracts/openapi/billing.yaml'));
  const validate = compile(contract.components.schemas, 'FormulaDraftUnitValidation');
  const value = { verified_count: 1, unit_versions: [{ unit_code: 'INPUT_TOKEN', unit_definition_id: id('2'), version: 1, status: 'ACTIVE' }], observed_at: '2026-09-04T08:00:00Z', unit_set_fingerprint: digest('e') };
  assert.equal(validate(value), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...value, secret: 'forbidden' }), false);
  assert.equal(validate({ ...value, observed_at: undefined }), false);
  assert.equal(validate({ ...value, unit_set_fingerprint: 'short' }), false);
});

test('Token approval projection binds command type to the exact 13-field and financial schemas', async () => {
  const contract = YAML.parse(await read('contracts/openapi/token-gateway.yaml'));
  const validate = compile(contract.components.schemas, 'ApprovalSnapshotProjection');
  const value = { command_type: 'TOKEN_MODEL_LIFECYCLE_CHANGE', target_resource_type: 'MODEL', target_resource_id: critical.model_id, critical_resource_versions: critical, financial_snapshot: financial, precheck_fingerprint: digest('f'), token_observed_at: '2026-09-04T08:00:00Z', billing_observed_at: financial.billing_as_of };
  assert.equal(validate(value), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...value, critical_resource_versions: { ...critical, extra: 1 } }), false);
  assert.equal(validate({ ...value, critical_resource_versions: { current_catalog_version: 8, source_catalog_version: 7, catalog_diff_fingerprint: digest('1'), billing_price_version: 11, route_set_fingerprint: digest('a'), provisioning_set_fingerprint: digest('b'), inflight_request_set_fingerprint: digest('c'), policy_version: 2 } }), false);

  const credentialID = id('2');
  const credential = {
    command_type: 'TOKEN_CREDENTIAL_REVOKE', target_resource_type: 'PROVIDER_CREDENTIAL', target_resource_id: credentialID,
    critical_resource_versions: {
      token_account_id: id('3'), token_account_version: 7, credential_id: credentialID, credential_version: 3,
      credential_status: 'ACTIVE', credential_fingerprint: digest('1'), active_route_set_fingerprint: digest('2'),
      provisioning_lease_set_fingerprint: digest('3'), inflight_request_set_fingerprint: digest('4'),
      replacement_credential_version: null, policy_version: 1,
    }, financial_snapshot: {}, precheck_fingerprint: digest('5'), token_observed_at: '2026-09-05T08:00:00Z',
  };
  assert.equal(validate(credential), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...credential, billing_observed_at: '2026-09-05T08:00:00Z' }), false);
  for (const invalidFinancial of [null, [], { extra: true }, financial]) assert.equal(validate({ ...credential, financial_snapshot: invalidFinancial }), false);
  assert.equal(validate({ ...credential, critical_resource_versions: { ...credential.critical_resource_versions, credential_secret: 'forbidden' } }), false);
});

test('model publish precheck is a closed stable-target and full price-evidence contract', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const validateRequest = compile(contract.components.schemas, 'TokenCommandPrecheckRequest');
  const validateResponse = compile(contract.components.schemas, 'TokenCommandPrecheck');
  const modelID = id('8');
  const protocolID = id('9');
  const formulaID = id('a');
  const priceID = id('b');
  const request = { command_type: 'TOKEN_MODEL_PUBLISH', target_resource_type: 'MODEL', target_resource_id: modelID, command: { model_id: modelID } };
  assert.equal(validateRequest(request), true, JSON.stringify(validateRequest.errors));
  assert.equal(validateRequest({ ...request, command: { ...request.command, extra: 1 } }), false);
  assert.equal(validateRequest({ ...request, extra: 1 }), false);

  const response = {
    action: 'TOKEN_MODEL_PUBLISH',
    evidence: {
      resource_type: 'MODEL', resource_id: modelID, current_status: 'DRAFT',
      current_version: { evidence_etag: `"impact-${digest('1')}"`, evidence_set_sha256: digest('1') },
      dependencies: Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map((key) => [key, 0])),
    },
    price_coverage: {
      models_checked: 1, models_covered: 1, fully_covered: true,
      versions: [{ model_id: modelID, protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT', covered: true, price_version_id: priceID, price_version: 4, effective_from: '2026-09-04T08:00:00Z' }],
      billing_observed_at: '2026-09-04T08:00:01Z', financial_fact_revision: 12, price_set_fingerprint: digest('2'),
    },
    blockers: [], token_observed_at: '2026-09-04T08:00:00Z', billing_observed_at: '2026-09-04T08:00:01Z',
    snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', precheck_fingerprint: digest('3'), requires_cas_revalidation: true,
    expected_versions: {
      model_id: modelID, expected_catalog_version: 7, expected_draft_version: 2, draft_fingerprint: digest('4'),
      protocol_definition_id: protocolID, protocol_version: 3,
      stable_route_count: 1, stable_route_set_fingerprint: digest('5'), provisioning_count: 1, ready_route_count: 1,
      provisioning_set_fingerprint: digest('6'), financial_fact_revision: 12, price_set_fingerprint: digest('2'),
    },
  };
  assert.equal(validateResponse(response), true, JSON.stringify(validateResponse.errors));
  assert.equal(validateResponse({ ...response, expected_versions: { ...response.expected_versions, extra: 1 } }), false);
  assert.equal(validateResponse({ ...response, expected_versions: { ...response.expected_versions, model_id: undefined } }), false);
  assert.equal(validateResponse({ ...response, price_coverage: { ...response.price_coverage, extra: 1 } }), false);
  assert.equal(validateResponse({ ...response, billing_observed_at: undefined }), false);
});

test('Account execution lease retains the exact snapshots for Token commands and legacy compatibility elsewhere', async () => {
  const contract = YAML.parse(await read('contracts/openapi/account.yaml'));
  const validate = compile(contract.components.schemas, 'ApprovalExecutionLease');
  const value = { approval_request_id: id('3'), command_type: 'TOKEN_MODEL_LIFECYCLE_CHANGE', policy_version: 2, command_fingerprint: digest('a'), normalized_command: {}, target_resource_type: 'MODEL', target_resource_id: critical.model_id, critical_resource_versions: critical, financial_snapshot: financial, requester_user_id: id('4'), approver_user_id: id('5'), decision_reason: 'approved', execution_idempotency_key: 'approval:key', executor_identity: 'token-command-executor', status: 'EXECUTING' };
  assert.equal(validate(value), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...value, financial_snapshot: { ...financial, extra: 1 } }), false);
  assert.equal(validate({ ...value, command_type: 'WALLET_ADJUSTMENT', critical_resource_versions: {}, financial_snapshot: {} }), true, JSON.stringify(validate.errors));
  const credentialID = id('8');
  const credentialCritical = { token_account_id: id('9'), token_account_version: 2, credential_id: credentialID, credential_version: 1, credential_status: 'RETIRED', credential_fingerprint: digest('1'), active_route_set_fingerprint: digest('2'), provisioning_lease_set_fingerprint: digest('3'), inflight_request_set_fingerprint: digest('4'), replacement_credential_version: null, policy_version: 1 };
  assert.equal(validate({ ...value, command_type: 'TOKEN_CREDENTIAL_REVOKE', policy_version: 1, target_resource_type: 'PROVIDER_CREDENTIAL', target_resource_id: credentialID, critical_resource_versions: credentialCritical, financial_snapshot: {} }), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...value, command_type: 'TOKEN_CREDENTIAL_REVOKE', policy_version: 1, target_resource_type: 'PROVIDER_CREDENTIAL', target_resource_id: credentialID, critical_resource_versions: credentialCritical, financial_snapshot: financial }), false);
});

test('authorized event carries the same closed snapshots without secret fields', async () => {
  const schema = JSON.parse(await read('contracts/events/account.approval-command.authorized.v1.schema.json'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  const validate = ajv.compile(schema);
  const event = { event_id: id('6'), event_type: 'account.approval-command.authorized.v1', aggregate_id: id('7'), aggregate_version: 2, occurred_at: '2026-09-04T08:00:02Z', trace_id: 'trace', payload: { approval_request_id: id('7'), command_type: 'TOKEN_MODEL_LIFECYCLE_CHANGE', command_fingerprint: digest('a'), policy_version: 2, executor_identity: 'token-command-executor', execution_deadline: '2026-09-04T08:05:00Z', normalized_command: {}, target_resource_type: 'MODEL', target_resource_id: critical.model_id, critical_resource_versions: critical, financial_snapshot: financial } };
  assert.equal(validate(event), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...event, payload: { ...event.payload, financial_snapshot: { ...financial, api_key: 'forbidden' } } }), false);
  const credentialID = id('8');
  const credentialCritical = { token_account_id: id('9'), token_account_version: 2, credential_id: credentialID, credential_version: 1, credential_status: 'RETIRED', credential_fingerprint: digest('1'), active_route_set_fingerprint: digest('2'), provisioning_lease_set_fingerprint: digest('3'), inflight_request_set_fingerprint: digest('4'), replacement_credential_version: null, policy_version: 1 };
  const credentialEvent = { ...event, payload: { ...event.payload, command_type: 'TOKEN_CREDENTIAL_REVOKE', policy_version: 1, target_resource_type: 'PROVIDER_CREDENTIAL', target_resource_id: credentialID, critical_resource_versions: credentialCritical, financial_snapshot: {} } };
  assert.equal(validate(credentialEvent), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...credentialEvent, payload: { ...credentialEvent.payload, financial_snapshot: financial } }), false);
});
