import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');
const id = digit => `${digit.repeat(8)}-${digit.repeat(4)}-4${digit.repeat(3)}-8${digit.repeat(3)}-${digit.repeat(12)}`;

function validator(contract, schemaName) {
  const document = JSON.parse(JSON.stringify({ $schema: 'https://json-schema.org/draft/2020-12/schema', $ref: `#/$defs/${schemaName}`, $defs: contract.components.schemas }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}

test('lifecycle plan API is closed, paginated and exposes no frozen evidence', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const collection = contract.paths['/token/admin/v1/models/{model_id}/lifecycle-plans'];
  const detail = contract.paths['/token/admin/v1/models/{model_id}/lifecycle-plans/{lifecycle_plan_id}'];
  const cancel = contract.paths['/token/admin/v1/models/{model_id}/lifecycle-plans/{lifecycle_plan_id}/cancel'];
  assert.ok(collection?.get && collection?.post && detail?.get && cancel?.post);
  assert.match(cancel.post.description, /PUBLISHED plans require platform\.models\.manage/);
  assert.match(cancel.post.description, /DEPRECATED or REMOVED plans require platform\.models\.lifecycle\.request/);
  for (const status of ['200', '400', '403', '410', '422', '429', '503', '504']) assert.ok(collection.get.responses[status], `missing list ${status}`);
  for (const status of ['201', '200', '400', '403', '404', '409', '503', '504']) assert.ok(collection.post.responses[status], `missing create ${status}`);
  for (const status of ['200', '400', '403', '404', '409', '503', '504']) assert.ok(cancel.post.responses[status], `missing cancel ${status}`);
  const validate = validator(contract, 'ModelLifecyclePlan');
  const plan = {
    lifecycle_plan_id: id('1'), model_id: id('2'), lifecycle_plan_version: 3, expected_catalog_version: 7,
    current_lifecycle: 'PUBLISHED', target_lifecycle: 'DEPRECATED', effective_at: '2026-09-06T12:00:00Z',
    existing_session_cutoff_at: '2026-09-07T12:00:00Z', replacement_model_id: null, force_cancel_inflight: false,
    status: 'SCHEDULED', reason: 'deprecate after dependency review', approval_request_id: id('3'), execution_attempt: 0,
    created_by: id('4'), created_at: '2026-09-05T12:00:00Z', updated_at: '2026-09-05T12:01:00Z',
  };
  assert.equal(validate(plan), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...plan, target_lifecycle: 'REMOVED', existing_session_cutoff_at: null, replacement_model_id: id('5') }), false);
  const publishPlan = { ...plan, current_lifecycle: 'DRAFT', target_lifecycle: 'PUBLISHED', existing_session_cutoff_at: null, replacement_model_id: null, approval_request_id: undefined, governed_command_reference: id('6') };
  assert.equal(validate(publishPlan), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...publishPlan, approval_request_id: id('7') }), false);
  for (const leaked of ['critical_resource_versions', 'financial_snapshot', 'approval_command_fingerprint', 'execution_lease_owner', 'request_digest', 'idempotency_key_digest']) {
    assert.equal(validate({ ...plan, [leaked]: leaked }), false, leaked);
  }
  const validateInput = validator(contract, 'ModelLifecyclePlanInput');
  const input = { expected_catalog_version: 7, expected_plan_version: 2, target_lifecycle: 'DEPRECATED', effective_at: '2026-09-06T12:00:00Z', existing_session_cutoff_at: null, replacement_model_id: null, force_cancel_inflight: false, reason: 'deprecate after dependency review' };
  assert.equal(validateInput(input), true, JSON.stringify(validateInput.errors));
  assert.equal(validateInput({ ...input, target_lifecycle: 'PUBLISHED' }), false);
  assert.equal(validateInput({ ...input, target_lifecycle: 'PUBLISHED', replacement_model_id: id('5') }), false);
  assert.equal(validateInput({ ...input, target_lifecycle: 'REMOVED', replacement_model_id: id('5'), existing_session_cutoff_at: null }), false);
  assert.equal(validateInput({ ...input, target_lifecycle: 'DEPRECATED', replacement_model_id: id('5') }), false);
  assert.equal(validateInput({ ...input, force_cancel_inflight: true }), false);
  assert.equal(validateInput({ ...input, ignore_blocker: true }), false);
});

test('lifecycle approval freezes the positive plan version in all public contracts', async () => {
  const [admin, account, eventSchema] = await Promise.all([
    read('contracts/openapi/admin.yaml').then(YAML.parse),
    read('contracts/openapi/account.yaml').then(YAML.parse),
    read('contracts/events/account.approval-command.authorized.v1.schema.json').then(JSON.parse),
  ]);
  const validateAdminCommand = validator(admin, 'TokenModelLifecyclePlanCommand');
  const validateAccountCommand = validator(account, 'TokenModelLifecyclePlanCommand');
  const command = { model_id: id('1'), lifecycle_plan_id: id('2'), lifecycle_plan_version: 4, target_lifecycle: 'REMOVED', effective_at: '2026-09-06T12:00:00Z', existing_session_cutoff_at: '2026-09-06T12:00:00Z', replacement_model_id: id('3') };
  assert.equal(validateAdminCommand(command), true, JSON.stringify(validateAdminCommand.errors));
  assert.equal(validateAccountCommand(command), true, JSON.stringify(validateAccountCommand.errors));
  const publishCommand = { model_id: command.model_id, publish_mode: 'SCHEDULED', lifecycle_plan_id: command.lifecycle_plan_id, lifecycle_plan_version: command.lifecycle_plan_version, effective_at: command.effective_at };
  const validatePublishCommand = validator(admin, 'TokenModelPublishCommand');
  assert.equal(validatePublishCommand(publishCommand), true, JSON.stringify(validatePublishCommand.errors));
  assert.equal(validatePublishCommand({ model_id: command.model_id }), true, JSON.stringify(validatePublishCommand.errors));
  assert.equal(validatePublishCommand({ model_id: command.model_id, publish_mode: 'IMMEDIATE' }), true, JSON.stringify(validatePublishCommand.errors));
  assert.equal(validatePublishCommand({ ...publishCommand, publish_mode: 'scheduled' }), false);
  assert.equal(validatePublishCommand({ ...publishCommand, effective_at: undefined }), false);
  assert.equal(validatePublishCommand({ ...publishCommand, replacement_model_id: id('3') }), false);
  const governedCreate = admin.paths['/token/admin/v1/commands'].post.responses;
  for (const status of ['200', '201', '202', '400', '401', '403', '409', '422', '503']) assert.ok(governedCreate[status], `missing governed command ${status}`);
  const validateScheduledResult = validator(admin, 'TokenScheduledModelPublishResult');
  const scheduledResult = { command_reference: id('4'), command_type: 'TOKEN_MODEL_PUBLISH', lifecycle_plan_id: command.lifecycle_plan_id, lifecycle_plan_version: 4, status: 'SCHEDULED', effective_at: command.effective_at };
  assert.equal(validateScheduledResult(scheduledResult), true, JSON.stringify(validateScheduledResult.errors));
  assert.equal(validateScheduledResult({ ...scheduledResult, catalog_version: 8 }), false);
  const validateImmediateResult = validator(admin, 'TokenImmediateModelPublishResult');
  const immediateResult = { command_reference: id('4'), command_type: 'TOKEN_MODEL_PUBLISH', model_id: command.model_id, catalog_version: 8, status: 'PUBLISHED' };
  assert.equal(validateImmediateResult(immediateResult), true, JSON.stringify(validateImmediateResult.errors));
  assert.equal(validateImmediateResult({ ...immediateResult, lifecycle_plan_id: command.lifecycle_plan_id }), false);
  const validateEnvelope = validator(admin, 'TokenCommandEnvelope');
  const publishExpected = {
    model_id: command.model_id, expected_catalog_version: 7, expected_draft_version: 2,
    draft_fingerprint: '1'.repeat(64), protocol_definition_id: id('6'), protocol_version: 1,
    stable_route_count: 1, stable_route_set_fingerprint: '2'.repeat(64), provisioning_count: 1,
    ready_route_count: 1, provisioning_set_fingerprint: '3'.repeat(64), financial_fact_revision: 9,
    price_set_fingerprint: '4'.repeat(64), precheck_fingerprint: '5'.repeat(64), request_fingerprint: '6'.repeat(64),
  };
  const envelope = { command_type: 'TOKEN_MODEL_PUBLISH', reason: 'publish reviewed draft on schedule', target_resource_type: 'MODEL', target_resource_id: command.model_id, expected_versions: publishExpected, precheck_fingerprint: publishExpected.precheck_fingerprint, command: publishCommand };
  assert.equal(validateEnvelope(envelope), true, JSON.stringify(validateEnvelope.errors));
  assert.equal(validateEnvelope({ ...envelope, expected_versions: { ...publishExpected, request_fingerprint: undefined } }), false);
  assert.equal(validateEnvelope({ ...envelope, expected_versions: { ...publishExpected, unexpected: true } }), false);
  assert.equal(validateEnvelope({ ...envelope, expected_versions: { ...publishExpected, stable_route_count: 0 } }), false);
  const validateGovernedResult = validator(admin, 'TokenCommandResult');
  const governedResult = { command_reference: id('4'), command_type: 'TOKEN_MODEL_PUBLISH', policy_version: 1, actor_user_id: id('5'), actor_permission: 'platform.models.manage', authorization_version: 7, idempotency_fingerprint: 'f'.repeat(64), target_resource_type: 'MODEL', target_resource_id: command.model_id, status: 'SUCCEEDED', redacted_result: scheduledResult, audit_request_id: 'scheduled-publish-audit', created_at: '2026-09-05T12:00:00Z', completed_at: '2026-09-05T12:00:01Z' };
  assert.equal(validateGovernedResult(governedResult), true, JSON.stringify(validateGovernedResult.errors));
  assert.equal(validateGovernedResult({ ...governedResult, redacted_result: { ...scheduledResult, catalog_version: 8 } }), false);
  assert.equal(validateGovernedResult({ ...governedResult, redacted_result: immediateResult }), true, JSON.stringify(validateGovernedResult.errors));
  assert.equal(validateGovernedResult({ ...governedResult, redacted_result: { ...immediateResult, lifecycle_plan_id: command.lifecycle_plan_id } }), false);
  assert.equal(validateAdminCommand({ ...command, target_lifecycle: 'PUBLISHED', existing_session_cutoff_at: null, replacement_model_id: null }), false);
  assert.equal(validateAccountCommand({ ...command, target_lifecycle: 'PUBLISHED', existing_session_cutoff_at: null, replacement_model_id: null }), false);
  assert.equal(validateAccountCommand({ ...command, target_lifecycle: 'DEPRECATED' }), false);
  assert.equal(validateAdminCommand({ ...command, lifecycle_plan_version: 0 }), false);
  assert.equal(validateAccountCommand({ ...command, unexpected: true }), false);
  const eventValidator = new Ajv2020({ allErrors: true, strict: false });
  addFormats(eventValidator);
  const validateEvent = eventValidator.compile(eventSchema);
  const critical = { model_id: command.model_id, catalog_version: 7, current_lifecycle: 'PUBLISHED', target_lifecycle: command.target_lifecycle, lifecycle_plan_version: command.lifecycle_plan_version, effective_at: command.effective_at, existing_session_cutoff_at: command.existing_session_cutoff_at, replacement_model_id: command.replacement_model_id, billing_price_version: 11, route_set_fingerprint: 'a'.repeat(64), provisioning_set_fingerprint: 'b'.repeat(64), inflight_request_set_fingerprint: 'c'.repeat(64), policy_version: 2 };
  const financial = { billing_price_revision: 11, price_set_fingerprint: 'd'.repeat(64), models_checked: 1, models_covered: 1, fully_covered: true, billing_as_of: '2026-09-05T12:00:00Z' };
  const event = { event_id: id('4'), event_type: 'account.approval-command.authorized.v1', aggregate_id: id('5'), aggregate_version: 2, occurred_at: '2026-09-05T12:01:00Z', trace_id: 'trace', payload: { approval_request_id: id('5'), command_type: 'TOKEN_MODEL_LIFECYCLE_CHANGE', command_fingerprint: 'e'.repeat(64), policy_version: 2, executor_identity: 'token-command-executor', execution_deadline: '2026-09-05T12:06:00Z', normalized_command: command, target_resource_type: 'MODEL', target_resource_id: command.model_id, critical_resource_versions: critical, financial_snapshot: financial } };
  assert.equal(validateEvent(event), true, JSON.stringify(validateEvent.errors));
  const publishEvent = { ...event, payload: { ...event.payload, normalized_command: publishCommand, critical_resource_versions: { ...critical, current_lifecycle: 'DRAFT', target_lifecycle: 'PUBLISHED', existing_session_cutoff_at: null, replacement_model_id: null } } };
  assert.equal(validateEvent(publishEvent), false);
  assert.equal(validateEvent({ ...event, payload: { ...event.payload, normalized_command: { ...command, lifecycle_plan_id: undefined } } }), false);
});

test('Portal and model detail UI expose only the gated plan workflow', async () => {
  const [portal, service, ui, client, navigation, worker, tokenMain, approvalChain] = await Promise.all([
    read('internal/portal/service.go'), read('internal/token/service.go'),
    read('../pinvou3-app/src/features/account-platform/AdminModelDetailWorkspace.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../pinvou3-app/src/features/account-platform/admin-navigation.js'),
    read('internal/token/lifecycle_plan_worker.go'), read('cmd/token-gateway/main.go'),
    read('internal/account/approval_snapshot_integration_test.go'),
  ]);
  assert.match(portal, /lifecycle-plans/);
  assert.match(service, /platform\.models\.lifecycle\.request/);
  assert.match(ui, /modelLifecyclePlans/);
  assert.match(ui, /datetime-local/);
  assert.match(ui, /force_cancel_inflight: false/);
  assert.match(client, /createModelLifecyclePlan/);
  assert.match(client, /cancelModelLifecyclePlan/);
  assert.match(navigation, /model_lifecycle_plan_status/);
  assert.match(worker, /APT-138/);
  assert.doesNotMatch(tokenMain, /NewLifecyclePlanWorker|RunOnce/);
  assert.doesNotMatch(ui, /force_cancel_inflight: true|ignore_blocker/);
  assert.match(approvalChain, /WithApprovedTokenCommandExecutor/);
  assert.match(approvalChain, /performApprovalExecutorRequest/);
  assert.match(approvalChain, /NewLifecyclePlanWorker/);
  assert.match(approvalChain, /planStatus != "APPLIED" \|\| lifecycleStatus != "DEPRECATED"/);
});
