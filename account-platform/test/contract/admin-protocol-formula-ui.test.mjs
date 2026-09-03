import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';
import { normalizeAdminModelTokenEntry, readAdminModelTokenModelTab } from '../../../pinvou3-app/src/features/account-platform/admin-navigation.js';
import { createProtocolFormulaIdempotencyKey, formulaDraftCommand, protocolDraftCommand, protocolFormulaCommandReceipt, safeFormulaPreviewSummary, safeProtocolFormulaFailure, safeProtocolFormulaWriteFailure, validateProtocolFormulaCommandResult } from '../../../pinvou3-app/src/features/account-platform/admin-protocol-formula-state.js';
import { adminPlatformCopy } from '../../../pinvou3-app/src/shared/admin-platform-i18n.js';

const locationLike = (search) => ({ pathname: '/admin.html', search, hash: '' });

test('APT-128 model tabs normalize deep links and clear revoked source state before requests', () => {
  assert.deepEqual(readAdminModelTokenModelTab(['platform.models.read', 'platform.pricing.read'], locationLike('?model_tab=protocols')), { tab: 'protocols', replace: false, visible: ['directory', 'protocols'] });
  assert.deepEqual(readAdminModelTokenModelTab(['platform.pricing.read'], locationLike('?model_tab=directory')), { tab: 'protocols', replace: true, visible: ['protocols'] });
  const pricingOnly = normalizeAdminModelTokenEntry('models', ['models'], true, locationLike('?section=models&model_tab=directory&model_q=private&model_cursor=model&protocol_q=chat&formula_cursor=formula'), ['platform.pricing.read']);
  assert.equal(pricingOnly.updates.model_tab, 'protocols');
  assert.equal(pricingOnly.updates.model_q, null);
  assert.equal(pricingOnly.updates.model_cursor, null);
  assert.equal(pricingOnly.updates.protocol_q, undefined);
  assert.equal(pricingOnly.updates.formula_cursor, undefined);
  const modelOnly = normalizeAdminModelTokenEntry('models', ['models'], true, locationLike('?section=models&model_tab=protocols&protocol_q=private&protocol_cursor=p&formula_q=secret&formula_cursor=f'), ['platform.models.read']);
  assert.equal(modelOnly.updates.model_tab, 'directory');
  for (const key of ['protocol_q', 'protocol_cursor', 'formula_q', 'formula_cursor']) assert.equal(modelOnly.updates[key], null);
});

test('APT-128 command builders accept only structured JSON and allow-listed formula inputs', () => {
  assert.deepEqual(protocolDraftCommand({ protocolCode: 'chat', requestSchema: '{"type":"object"}', expectedHeadVersion: 3, reason: 'document schema change' }).value, {
    protocol_code: 'CHAT', request_schema: { type: 'object' }, expected_protocol_head_version: 3, reason: 'document schema change',
  });
  assert.equal(protocolDraftCommand({ protocolCode: 'chat', requestSchema: 'not-json', reason: 'document schema change' }).error, 'invalidSchema');
  const formula = formulaDraftCommand({
    rows: [{ unitCode: 'INPUT_TOKEN', unitDefinitionID: '00000000-0000-4000-8000-000000000010', input: 'estimated_input_tokens' }],
    expectedProtocolVersion: 2, expectedFormulaHeadVersion: 4, reason: 'map authoritative unit',
  });
  assert.deepEqual(formula.value, {
    required_unit_versions: { INPUT_TOKEN: '00000000-0000-4000-8000-000000000010' },
    expression: { INPUT_TOKEN: 'estimated_input_tokens' },
    expected_protocol_version: 2, expected_formula_head_version: 4, reason: 'map authoritative unit',
  });
  assert.equal(formulaDraftCommand({ rows: [{ unitCode: 'X', unitDefinitionID: 'id', input: 'process.env.SECRET' }], expectedProtocolVersion: 1, reason: 'unsafe expression attempt' }).error, 'invalidFormulaRows');
  assert.equal(formulaDraftCommand({ rows: [{ unitCode: 'INPUT_TOKEN', unitDefinitionID: '00000000-0000-4000-8000-000000000010', input: 'request_count' }, { unitCode: 'INPUT_TOKEN', unitDefinitionID: '00000000-0000-4000-8000-000000000011', input: 'image_count' }], expectedProtocolVersion: 1, reason: 'duplicate unit mapping' }).error, 'duplicateUnitCode');
  for (const unitCode of ['input_token', ' INPUT_TOKEN', 'INPUT_TOKEN ']) assert.equal(formulaDraftCommand({ rows: [{ unitCode, unitDefinitionID: '00000000-0000-4000-8000-000000000010', input: 'request_count' }], expectedProtocolVersion: 1, reason: 'reject noncanonical code' }).error, 'invalidFormulaRows');
  assert.equal(formulaDraftCommand({ rows: [
    { unitCode: 'INPUT_TOKEN', unitDefinitionID: 'abcdef01-abcd-4000-8abc-abcdefabcdef', input: 'request_count' },
    { unitCode: 'OUTPUT_TOKEN', unitDefinitionID: 'ABCDEF01-ABCD-4000-8ABC-ABCDEFABCDEF', input: 'max_output_tokens' },
  ], expectedProtocolVersion: 1, reason: 'reject duplicate unit id' }).error, 'invalidFormulaRows');
  assert.equal(createProtocolFormulaIdempotencyKey('publish', () => '00000000-0000-4000-8000-000000000001'), 'admin-publish-00000000-0000-4000-8000-000000000001');
});

test('APT-128 safe projections never expose raw formulas, private errors, or untrusted command references', () => {
  const preview = safeFormulaPreviewSummary({
    formula: { formula_version_id: 'formula', protocol_code: 'CHAT', version: 2, required_unit_versions: { OUTPUT: 'id-2', INPUT: 'id-1' }, expression: { INPUT: 'secret_internal_counter' } },
    sample_missing_optional_parameters: { cached_input_tokens: 0 }, deterministic: true, external_access: false,
  });
  assert.deepEqual(preview, { formulaVersionID: 'formula', protocolCode: 'CHAT', version: 2, unitCodes: ['INPUT', 'OUTPUT'], deterministic: true, externalAccess: false, optionalParameterCount: 1 });
  assert.equal(Object.hasOwn(preview, 'expression'), false);
  const failure = safeProtocolFormulaFailure({ code: 'COMMAND_RESULT_UNKNOWN', status: 504, message: 'provider secret endpoint', requestID: 'req-1', details: { command_reference: 'a'.repeat(64), unsafe: 'secret' } });
  assert.deepEqual(failure, { code: 'COMMAND_RESULT_UNKNOWN', status: 504, requestID: 'req-1', commandReference: 'a'.repeat(64), unknown: true });
  assert.equal(safeProtocolFormulaFailure({ details: { command_reference: '<script>' } }).commandReference, '');
  assert.deepEqual(protocolFormulaCommandReceipt({ command_reference: 'b'.repeat(64), command_type: 'TOKEN_FORMULA_PUBLISH', status: 'EXECUTED', billing_sync: 'SYNCHRONIZED', precheck: { metering_unit_version_count: 2, formula_snapshot_sha256: 'private' } }), {
    commandReference: 'b'.repeat(64), commandType: 'TOKEN_FORMULA_PUBLISH', status: 'EXECUTED', billingSync: 'SYNCHRONIZED', billingValidation: false, verifiedUnitCount: 2,
  });
  assert.deepEqual(protocolFormulaCommandReceipt({ command_type: 'TOKEN_FORMULA_DRAFT_CREATE', precheck: { metering_unit_version_count: 3, metering_unit_versions: ['private'] } }), { commandReference: '', commandType: 'TOKEN_FORMULA_DRAFT_CREATE', status: '', billingSync: '', billingValidation: true, verifiedUnitCount: 3 });
  assert.equal(Object.hasOwn(protocolFormulaCommandReceipt({ precheck: { metering_unit_versions: ['private'] } }), 'meteringUnitVersions'), false);
});

test('APT-128 verifies every 2xx command envelope and treats malformed success as unknown', () => {
  const protocolID = '00000000-0000-7000-8000-000000000101';
  const formulaID = '00000000-0000-7000-8000-000000000201';
  const unitID = '00000000-0000-4000-8000-000000000301';
  const secondUnitID = 'abcdef01-abcd-4000-8abc-abcdefabcdef';
  const reference = 'a'.repeat(64);
  const draftETag = 'b'.repeat(64);
  const publishedETag = 'd'.repeat(64);
  const createdAt = '2026-09-03T23:00:00Z';
  const publishedAt = '2026-09-04T00:00:00Z';
  const requestSchema = { type: 'object', properties: { messages: { type: 'array' } } };
  const expression = { REQUEST: 'request_count' };
  const common = (commandType) => ({ command_reference: reference, command_type: commandType, policy_version: 1, status: 'EXECUTED' });
  const protocolDraftAction = { type: 'PROTOCOL_DRAFT', body: { protocol_code: 'CHAT', request_schema: requestSchema, expected_protocol_head_version: 2 } };
  const protocolDraft = { protocol_definition_id: protocolID, protocol_code: 'CHAT', version: 3, status: 'DRAFT', request_schema: requestSchema, etag: draftETag, created_at: createdAt };
  const protocolDraftResult = { ...common('TOKEN_PROTOCOL_DRAFT_CREATE'), protocol: protocolDraft, precheck: { protocol_code_conflict: false, published_reference_count: 0, expected_protocol_head_version: 2, before_status: 'NONE', after_status: 'DRAFT' } };
  assert.equal(validateProtocolFormulaCommandResult(protocolDraftResult, protocolDraftAction).ok, true);
  const protocolPublishAction = { type: 'PROTOCOL_PUBLISH', item: protocolDraft };
  const protocolPublishResult = { ...common('TOKEN_PROTOCOL_PUBLISH'), protocol: { ...protocolDraft, status: 'PUBLISHED', etag: publishedETag, published_at: publishedAt }, precheck: { compatible_formula_count: 1, verified_metering_unit_version_count: 1, formula_set_sha256: 'e'.repeat(64), model_draft_reference_count: 0, protocol_code: 'CHAT', protocol_version: 3, request_schema_valid: true, before_status: 'DRAFT', after_status: 'PUBLISHED' } };
  assert.equal(validateProtocolFormulaCommandResult(protocolPublishResult, protocolPublishAction).ok, true);
  const formulaDraftAction = { type: 'FORMULA_DRAFT', item: { protocol_definition_id: protocolID, protocol_code: 'CHAT' }, body: { expected_protocol_version: 3, expected_formula_head_version: 4, required_unit_versions: { REQUEST: unitID }, expression } };
  const formulaDraft = { formula_version_id: formulaID, protocol_definition_id: protocolID, protocol_code: 'CHAT', version: 5, status: 'DRAFT', required_unit_versions: { REQUEST: unitID }, expression, evaluator_version: 'SAFE_PARAMETER_MAP_V1', etag: draftETag, created_at: createdAt };
  const formulaDraftResult = { ...common('TOKEN_FORMULA_DRAFT_CREATE'), formula: formulaDraft, precheck: { protocol_version: 3, expected_formula_head_version: 4, metering_unit_version_count: 1, metering_unit_versions: [{ unit_code: 'REQUEST', unit_definition_id: unitID, version: 7, status: 'ACTIVE' }], parameter_mapping_valid: true, before_status: 'NONE', after_status: 'DRAFT' } };
  const draftVerified = validateProtocolFormulaCommandResult(formulaDraftResult, formulaDraftAction);
  assert.equal(draftVerified.ok, true);
  assert.deepEqual(draftVerified.receipt, { commandReference: reference, commandType: 'TOKEN_FORMULA_DRAFT_CREATE', status: 'EXECUTED', billingSync: '', billingValidation: true, verifiedUnitCount: 1 });
  const multiUnitAction = { ...formulaDraftAction, body: { ...formulaDraftAction.body, required_unit_versions: { REQUEST: unitID, IMAGE: secondUnitID }, expression: { REQUEST: 'request_count', IMAGE: 'image_count' } } };
  const multiUnitResult = { ...formulaDraftResult,
    formula: { ...formulaDraft, required_unit_versions: multiUnitAction.body.required_unit_versions, expression: multiUnitAction.body.expression },
    precheck: { ...formulaDraftResult.precheck, metering_unit_version_count: 2, metering_unit_versions: [
      { unit_code: 'IMAGE', unit_definition_id: secondUnitID, version: 2, status: 'ACTIVE' },
      { unit_code: 'REQUEST', unit_definition_id: unitID, version: 7, status: 'ACTIVE' },
    ] },
  };
  assert.equal(validateProtocolFormulaCommandResult(multiUnitResult, multiUnitAction).ok, true, 'valid multi-unit response order is irrelevant');
  const formulaPublishAction = { type: 'FORMULA_PUBLISH', item: formulaDraft };
  const formulaPublishResult = { ...common('TOKEN_FORMULA_PUBLISH'), billing_sync: 'SYNCHRONIZED', formula: { ...formulaDraft, status: 'PUBLISHED', etag: publishedETag, published_at: publishedAt }, precheck: { deterministic_preview: true, metering_unit_version_count: 1, before_status: 'DRAFT', after_status: 'PUBLISHED', billing_sync: { formula_version_id: formulaID, status: 'REGISTERED', protocol_code: 'CHAT', required_unit_version_ids: [unitID], formula_snapshot_sha256: 'c'.repeat(64), published_at: publishedAt } } };
  assert.equal(validateProtocolFormulaCommandResult(formulaPublishResult, formulaPublishAction).ok, true);

  const without = (object, key) => Object.fromEntries(Object.entries(object).filter(([name]) => name !== key));
  for (const key of ['protocol_definition_id', 'protocol_code', 'version', 'status', 'request_schema', 'etag', 'created_at']) {
    assert.equal(validateProtocolFormulaCommandResult({ ...protocolDraftResult, protocol: without(protocolDraft, key) }, protocolDraftAction).ok, false, `missing protocol.${key}`);
  }
  for (const key of ['formula_version_id', 'protocol_definition_id', 'protocol_code', 'version', 'status', 'required_unit_versions', 'expression', 'evaluator_version', 'etag', 'created_at']) {
    assert.equal(validateProtocolFormulaCommandResult({ ...formulaDraftResult, formula: without(formulaDraft, key) }, formulaDraftAction).ok, false, `missing formula.${key}`);
  }
  for (const protocol of [
    { ...protocolDraft, protocol_definition_id: 1 },
    { ...protocolDraft, protocol_code: 1 },
    { ...protocolDraft, version: '3' },
    { ...protocolDraft, status: 1 },
    { ...protocolDraft, request_schema: [] },
    { ...protocolDraft, etag: 1 },
    { ...protocolDraft, created_at: 'not-a-time' },
    { ...protocolDraft, extra_private_field: true },
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...protocolDraftResult, protocol }, protocolDraftAction).ok, false);
  for (const formula of [
    { ...formulaDraft, formula_version_id: 1 },
    { ...formulaDraft, protocol_definition_id: 1 },
    { ...formulaDraft, protocol_code: 1 },
    { ...formulaDraft, version: '5' },
    { ...formulaDraft, status: 1 },
    { ...formulaDraft, required_unit_versions: { REQUEST: 'not-a-uuid' } },
    { ...formulaDraft, expression: { REQUEST: 1 } },
    { ...formulaDraft, expression: { REQUEST: 'unsafe_expression' } },
    { ...formulaDraft, expression: { OTHER: 'request_count' } },
    { ...formulaDraft, evaluator_version: 1 },
    { ...formulaDraft, evaluator_version: 'UNSAFE_EVAL_V1' },
    { ...formulaDraft, etag: 1 },
    { ...formulaDraft, created_at: 123 },
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...formulaDraftResult, formula }, formulaDraftAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...protocolDraftResult, protocol: { ...protocolDraft, request_schema: { type: 'string' } } }, protocolDraftAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...protocolPublishResult, protocol: { ...protocolPublishResult.protocol, request_schema: { type: 'string' } } }, protocolPublishAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...formulaDraftResult, formula: { ...formulaDraft, expression: { REQUEST: 'image_count' } } }, formulaDraftAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...formulaPublishResult, formula: { ...formulaPublishResult.formula, expression: { REQUEST: 'image_count' } } }, formulaPublishAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...formulaPublishResult, formula: { ...formulaPublishResult.formula, evaluator_version: 'SAFE_PARAMETER_MAP_V2' } }, formulaPublishAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...formulaPublishResult, formula: { ...formulaPublishResult.formula, required_unit_versions: { REQUEST: '00000000-0000-4000-8000-000000000302' } } }, formulaPublishAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...protocolPublishResult, protocol: { ...protocolPublishResult.protocol, created_at: '2026-09-03T22:59:59Z' } }, protocolPublishAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...formulaPublishResult, formula: { ...formulaPublishResult.formula, created_at: '2026-09-03T22:59:59Z' } }, formulaPublishAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...protocolPublishResult, protocol: without(protocolPublishResult.protocol, 'published_at') }, protocolPublishAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...formulaPublishResult, formula: without(formulaPublishResult.formula, 'published_at') }, formulaPublishAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...formulaPublishResult, precheck: { ...formulaPublishResult.precheck, billing_sync: { ...formulaPublishResult.precheck.billing_sync, published_at: 'not-a-time' } } }, formulaPublishAction).ok, false);
  assert.equal(validateProtocolFormulaCommandResult({ ...formulaPublishResult, formula: { ...formulaPublishResult.formula, published_at: '2026-02-31T00:00:00Z' }, precheck: { ...formulaPublishResult.precheck, billing_sync: { ...formulaPublishResult.precheck.billing_sync, published_at: '2026-02-31T00:00:00Z' } } }, formulaPublishAction).ok, false);
  for (const precheck of [
    { ...protocolDraftResult.precheck, expected_protocol_head_version: '2' },
    { ...protocolDraftResult.precheck, protocol_code_conflict: 0 },
    { ...protocolDraftResult.precheck, published_reference_count: '0' },
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...protocolDraftResult, precheck }, protocolDraftAction).ok, false);
  for (const precheck of [
    { ...formulaDraftResult.precheck, protocol_version: '3' },
    { ...formulaDraftResult.precheck, expected_formula_head_version: '4' },
    { ...formulaDraftResult.precheck, metering_unit_version_count: '1' },
    { ...formulaDraftResult.precheck, metering_unit_versions: {} },
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...formulaDraftResult, precheck }, formulaDraftAction).ok, false);
  for (const meteringUnit of [
    { unit_code: 'REQUEST', unit_definition_id: unitID, version: '7', status: 'ACTIVE' },
    { unit_code: 'REQUEST', unit_definition_id: unitID, version: 7, status: 'RETIRED' },
    { unit_code: 'OTHER', unit_definition_id: unitID, version: 7, status: 'ACTIVE' },
    { unit_code: 'REQUEST', unit_definition_id: '00000000-0000-4000-8000-000000000302', version: 7, status: 'ACTIVE' },
    { unit_code: 'REQUEST', unit_definition_id: unitID, version: 7, status: 'ACTIVE', secret: 'must-not-pass' },
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...formulaDraftResult, precheck: { ...formulaDraftResult.precheck, metering_unit_versions: [meteringUnit] } }, formulaDraftAction).ok, false);
  const duplicateUnitRequest = { ...multiUnitAction, body: { ...multiUnitAction.body, required_unit_versions: { REQUEST: secondUnitID, IMAGE: secondUnitID.toUpperCase() } } };
  assert.equal(validateProtocolFormulaCommandResult(multiUnitResult, duplicateUnitRequest).ok, false, 'request UUIDs are unique after case normalization');
  assert.equal(validateProtocolFormulaCommandResult({ ...multiUnitResult, formula: { ...multiUnitResult.formula, required_unit_versions: { REQUEST: secondUnitID, IMAGE: secondUnitID.toUpperCase() } } }, multiUnitAction).ok, false, 'resource UUIDs are unique after case normalization');
  assert.equal(validateProtocolFormulaCommandResult({ ...multiUnitResult, precheck: { ...multiUnitResult.precheck, metering_unit_versions: [
    { unit_code: 'IMAGE', unit_definition_id: secondUnitID.toUpperCase(), version: 2, status: 'ACTIVE' },
    { unit_code: 'REQUEST', unit_definition_id: secondUnitID, version: 7, status: 'ACTIVE' },
  ] } }, multiUnitAction).ok, false, 'Billing UUIDs are unique after case normalization');
  for (const noncanonicalCode of ['request', ' REQUEST', 'REQUEST ']) {
    assert.equal(validateProtocolFormulaCommandResult({ ...formulaDraftResult, formula: { ...formulaDraft, required_unit_versions: { [noncanonicalCode]: unitID }, expression: { [noncanonicalCode]: 'request_count' } } }, { ...formulaDraftAction, body: { ...formulaDraftAction.body, required_unit_versions: { [noncanonicalCode]: unitID }, expression: { [noncanonicalCode]: 'request_count' } } }).ok, false);
    assert.equal(validateProtocolFormulaCommandResult({ ...formulaDraftResult, precheck: { ...formulaDraftResult.precheck, metering_unit_versions: [{ unit_code: noncanonicalCode, unit_definition_id: unitID, version: 7, status: 'ACTIVE' }] } }, formulaDraftAction).ok, false);
  }
  for (const [result, action] of [
    [protocolDraftResult, protocolDraftAction],
    [protocolPublishResult, protocolPublishAction],
    [formulaDraftResult, formulaDraftAction],
    [formulaPublishResult, formulaPublishAction],
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...result, precheck: { ...result.precheck, secret: 'must-not-pass' } }, action).ok, false);
  for (const billingSync of [
    { ...formulaPublishResult.precheck.billing_sync, formula_version_id: 1 },
    { ...formulaPublishResult.precheck.billing_sync, status: 'PENDING' },
    { ...formulaPublishResult.precheck.billing_sync, protocol_code: 'OTHER' },
    { ...formulaPublishResult.precheck.billing_sync, required_unit_version_ids: ['not-a-uuid'] },
    { ...formulaPublishResult.precheck.billing_sync, formula_snapshot_sha256: 1 },
    { ...formulaPublishResult.precheck.billing_sync, secret: 'must-not-pass' },
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...formulaPublishResult, precheck: { ...formulaPublishResult.precheck, billing_sync: billingSync } }, formulaPublishAction).ok, false);
  for (const invalidProtocolID of [
    '00000000-0000-0000-0000-000000000000',
    '00000000-0000-4000-8000-000000000101',
    '00000000-0000-7000-7000-000000000101',
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...protocolDraftResult, protocol: { ...protocolDraft, protocol_definition_id: invalidProtocolID } }, protocolDraftAction).ok, false);
  for (const invalidFormulaID of [
    '00000000-0000-0000-0000-000000000000',
    '00000000-0000-4000-8000-000000000201',
    '00000000-0000-7000-7000-000000000201',
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...formulaDraftResult, formula: { ...formulaDraft, formula_version_id: invalidFormulaID } }, formulaDraftAction).ok, false);
  for (const invalidUnitID of [
    '00000000-0000-0000-0000-000000000000',
    '00000000-0000-0000-8000-000000000301',
    '00000000-0000-4000-7000-000000000301',
  ]) assert.equal(validateProtocolFormulaCommandResult({ ...formulaDraftResult, formula: { ...formulaDraft, required_unit_versions: { REQUEST: invalidUnitID } } }, { ...formulaDraftAction, body: { ...formulaDraftAction.body, required_unit_versions: { REQUEST: invalidUnitID } } }).ok, false);
  for (const malformed of [
    { ...formulaPublishResult, command_reference: 'not-safe' },
    { ...formulaPublishResult, command_type: 'TOKEN_PROTOCOL_PUBLISH' },
    { ...formulaPublishResult, policy_version: '1' },
    { ...formulaPublishResult, status: 'PENDING' },
    { ...formulaPublishResult, precheck: [] },
    { ...formulaPublishResult, private_result: true },
    { ...formulaPublishResult, formula: { ...formulaPublishResult.formula, formula_version_id: protocolID } },
    { ...formulaPublishResult, billing_sync: '' },
    { ...formulaPublishResult, precheck: { ...formulaPublishResult.precheck, billing_sync: { ...formulaPublishResult.precheck.billing_sync, formula_snapshot_sha256: 'bad' } } },
  ]) {
    const verified = validateProtocolFormulaCommandResult(malformed, formulaPublishAction);
    assert.equal(verified.ok, false);
    assert.equal(verified.failure.unknown, true);
  }
});

test('APT-128 write transport failures preserve safe references and lock when zero-write cannot be proven', () => {
  const safeReference = 'd'.repeat(64);
  const raw504 = safeProtocolFormulaWriteFailure({ status: 504, code: 'REQUEST_FAILED' }, true);
  assert.equal(raw504.unknown, true);
  assert.equal(raw504.code, 'COMMAND_RESULT_UNKNOWN');
  const network = safeProtocolFormulaWriteFailure(new TypeError('fetch failed with private endpoint'), true);
  assert.deepEqual({ code: network.code, unknown: network.unknown, requestID: network.requestID }, { code: 'COMMAND_RESULT_UNKNOWN', unknown: true, requestID: '' });
  const standard504 = safeProtocolFormulaWriteFailure({ status: 504, code: 'COMMAND_RESULT_UNKNOWN', details: { command_reference: safeReference }, requestID: 'req-safe' }, true);
  assert.equal(standard504.commandReference, safeReference);
  assert.equal(standard504.unknown, true);
  const inactive = safeProtocolFormulaWriteFailure({ status: 503, code: 'TOKEN_COMMAND_GOVERNANCE_INACTIVE', requestID: 'req-pre' }, true);
  assert.equal(inactive.unknown, false);
  for (const [status, code] of [
    [400, 'INVALID_PROTOCOL_COMMAND'],
    [400, 'INVALID_FORMULA_COMMAND'],
    [422, 'FORMULA_UNIT_VALIDATION_REJECTED'],
    [503, 'FORMULA_UNIT_VALIDATION_UNAVAILABLE'],
    [422, 'COMMAND_PRECHECK_FAILED'],
    [403, 'PERMISSION_DENIED'],
  ]) {
    assert.equal(safeProtocolFormulaWriteFailure({ status, code }, true).unknown, false, `${status}:${code}`);
  }
  for (const [status, code] of [
    [504, 'FORBIDDEN'],
    [500, 'INVALID_PROTOCOL_COMMAND'],
    [422, 'FORMULA_UNIT_VALIDATION_UNAVAILABLE'],
    [503, 'FORMULA_UNIT_VALIDATION_REJECTED'],
    [503, 'REQUEST_FAILED'],
    [502, 'UPSTREAM_UNAVAILABLE'],
    [0, 'INVALID_FORMULA_COMMAND'],
    ['400', 'INVALID_PROTOCOL_COMMAND'],
  ]) {
    const failure = safeProtocolFormulaWriteFailure({ status, code }, true);
    assert.equal(failure.unknown, true, `${status}:${code}`);
    assert.equal(failure.code, 'COMMAND_RESULT_UNKNOWN');
  }
  const beforeDispatch = safeProtocolFormulaWriteFailure(new TypeError('baseline unavailable'), false);
  assert.equal(beforeDispatch.unknown, false);
});

test('APT-128 client sends exact APT-122 routes, CAS bodies, and fixed idempotency headers', async (t) => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url: String(url), options });
    return new Response(JSON.stringify({ command_reference: 'c'.repeat(64), status: 'EXECUTED', precheck: {} }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  t.after(() => { globalThis.fetch = originalFetch; });
  const client = new AdminPlatformClient('https://portal.example');
  client.csrfToken = 'csrf';
  await client.createProtocolDraft({ protocol_code: 'CHAT', request_schema: {}, expected_protocol_head_version: 0, reason: 'create schema safely' }, 'fixed-protocol-key');
  await client.publishProtocol('00000000-0000-4000-8000-000000000001', { expected_protocol_version: 1, draft_etag: 'a'.repeat(64), reason: 'publish schema safely' }, 'fixed-publish-key');
  await client.createFormulaDraft('00000000-0000-4000-8000-000000000001', { required_unit_versions: { REQUEST: '00000000-0000-4000-8000-000000000002' }, expression: { REQUEST: 'request_count' }, expected_protocol_version: 1, expected_formula_head_version: 0, reason: 'create formula safely' }, 'fixed-formula-key');
  await client.previewFormula('00000000-0000-4000-8000-000000000003');
  await client.publishFormula('00000000-0000-4000-8000-000000000003', { expected_formula_version: 1, draft_etag: 'b'.repeat(64), reason: 'publish formula safely' }, 'fixed-formula-publish-key');
  assert.deepEqual(calls.map((call) => [new URL(call.url).pathname, call.options.method || 'GET']), [
    ['/api/admin/token/admin/v1/protocols', 'POST'],
    ['/api/admin/token/admin/v1/protocols/00000000-0000-4000-8000-000000000001/publish', 'POST'],
    ['/api/admin/token/admin/v1/protocols/00000000-0000-4000-8000-000000000001/formulas', 'POST'],
    ['/api/admin/token/admin/v1/formulas/00000000-0000-4000-8000-000000000003/preview', 'GET'],
    ['/api/admin/token/admin/v1/formulas/00000000-0000-4000-8000-000000000003/publish', 'POST'],
  ]);
  assert.deepEqual(calls.filter((call) => call.options.method === 'POST').map((call) => call.options.headers['Idempotency-Key']), ['fixed-protocol-key', 'fixed-publish-key', 'fixed-formula-key', 'fixed-formula-publish-key']);
  for (const call of calls.filter((item) => item.options.method === 'POST')) assert.equal(call.options.headers['X-CSRF-Token'], 'csrf');
});

test('APT-128 UI is localized, structured, responsive, and keeps APT-123 activation authoritative', async () => {
  for (const language of ['zh', 'en', 'ja']) {
    const copy = adminPlatformCopy[language].modelToken;
    for (const key of ['protocolFormulaView', 'protocolDefinitions', 'safeFormulas', 'createProtocolDraft', 'createFormulaDraft', 'previewSafety', 'billingAuthority']) assert.ok(copy[key], `${language}.${key}`);
    assert.ok(copy.commandErrors.TOKEN_COMMAND_GOVERNANCE_INACTIVE);
  }
  const [workspace, css, tokenService] = await Promise.all([
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/AdminProtocolFormulaWorkspace.jsx', import.meta.url), 'utf8'),
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/admin-platform.css', import.meta.url), 'utf8'),
    readFile(new URL('../../internal/token/service.go', import.meta.url), 'utf8'),
  ]);
  assert.doesNotMatch(workspace, /window\.(prompt|confirm)|eval\(|dangerouslySetInnerHTML/);
  assert.match(workspace, /role="dialog" aria-modal="true"/);
  assert.match(workspace, /<h2 id=\{titleID\}>/);
  assert.match(workspace, /function PreviewFailure/);
  assert.match(workspace, /aria-labelledby="formula-preview-error-title"/);
  assert.match(workspace, /close\.current\?\.focus\(\)/);
  assert.match(workspace, /keepDialogFocus\(event, root\.current\)/);
  assert.match(workspace, /event\.key === 'Escape'/);
  assert.match(workspace, /previousFocus\.current\?\.focus\?\.\(\)/);
  assert.match(workspace, /minLength="10"/);
  assert.match(workspace, /action\.failure\?\.unknown/);
  assert.match(css, /@media\(max-width:760px\)[\s\S]*\.adm-protocol-dialog\{width:100%;height:100%/);
  assert.match(css, /@media\(pointer:coarse\)/);
  for (const command of ['TOKEN_PROTOCOL_DRAFT_CREATE', 'TOKEN_PROTOCOL_PUBLISH', 'TOKEN_FORMULA_DRAFT_CREATE', 'TOKEN_FORMULA_PUBLISH']) {
    const policy = `approvalpolicy.Require("${command}", approvalpolicy.AuditOnly)`;
    const policyIndex = tokenService.indexOf(policy);
    assert.ok(policyIndex >= 0);
    assert.ok(tokenService.indexOf('s.requireNewTokenCommandGovernanceActive()', policyIndex) > policyIndex);
  }
});
