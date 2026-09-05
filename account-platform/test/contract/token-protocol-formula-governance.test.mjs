import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = (relative) => readFile(path.join(root, relative), 'utf8');
const commands = [
  'TOKEN_PROTOCOL_DRAFT_CREATE',
  'TOKEN_PROTOCOL_PUBLISH',
  'TOKEN_FORMULA_DRAFT_CREATE',
  'TOKEN_FORMULA_PUBLISH',
];

function validator(contract, schemaName) {
  const document = JSON.parse(JSON.stringify({ $schema: 'https://json-schema.org/draft/2020-12/schema', $ref: `#/$defs/${schemaName}`, $defs: contract.components.schemas }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}

test('APT-122 installs four exact audit-only policies without an executor or result table', async () => {
  const [up, down, registry] = await Promise.all([
    read('migrations/account/000044_token_protocol_formula_command_policies.up.sql'),
    read('migrations/account/000044_token_protocol_formula_command_policies.down.sql'),
    read('internal/platform/approvalpolicy/registry.go'),
  ]);
  for (const command of commands) {
    assert.match(up, new RegExp(`'${command}',1,'AUDIT_ONLY','platform\\.pricing\\.manage'`));
    assert.match(down, new RegExp(`'${command}'`));
    assert.match(registry, new RegExp(`"${command}"`));
  }
  assert.doesNotMatch(up, /CREATE\s+TABLE/i);
  assert.match(up, /'requesting-domain-service'/);
  assert.match(down, /approval_requests[\s\S]*RAISE EXCEPTION/);
});

test('APT-122-R2 retires dedicated writes and selects only the generic governed adapter', async () => {
  const [service, activation, handlers, governance, commandGovernance] = await Promise.all([
    read('internal/token/service.go'),
    read('internal/token/command_governance_activation.go'),
    read('internal/token/protocol_admin.go'),
    read('internal/token/protocol_formula_governance.go'),
    read('internal/token/command_governance.go'),
  ]);
  for (const command of commands) {
    const policy = `approvalpolicy.Require("${command}", approvalpolicy.AuditOnly)`;
    const gate = 's.requireNewTokenCommandGovernanceActive()';
    const target = 'rejectProtocolFormulaDirectWrite()';
    const policyIndex = service.indexOf(policy);
    const gateIndex = service.indexOf(gate, policyIndex);
    const handlerIndex = service.indexOf(target, gateIndex);
    assert.ok(policyIndex >= 0 && gateIndex > policyIndex && handlerIndex > gateIndex, `${command} must authorize, gate, then retire its dedicated write`);
    assert.match(handlers, new RegExp(`"${command}"`), 'historical repository adapter remains readable for migration compatibility');
    assert.match(governance, new RegExp(`"${command}"`));
  }
  assert.match(commandGovernance, /ExecuteProtocolFormulaTokenCommand/);
  assert.match(activation, /DIRECT_TOKEN_COMMAND_RETIRED/);
  assert.match(activation, /commandGovernanceActivation == nil/);
  assert.match(activation, /activation\.State != "ACTIVE" \|\| activation\.Epoch < 1/);
  assert.match(activation, /TOKEN_COMMAND_GOVERNANCE_INACTIVE/);
});

test('APT-122 OpenAPI freezes CAS, reason, idempotency and unknown-result envelopes', async () => {
  const [document, billingDocument] = await Promise.all([
    read('contracts/openapi/admin.yaml').then(YAML.parse),
    read('contracts/openapi/billing.yaml').then(YAML.parse),
  ]);
  const commandPaths = [
    ['/token/admin/v1/protocols', 'post'],
    ['/token/admin/v1/protocols/{protocol_definition_id}/publish', 'post'],
    ['/token/admin/v1/protocols/{protocol_definition_id}/formulas', 'post'],
    ['/token/admin/v1/formulas/{formula_version_id}/publish', 'post'],
  ];
  for (const [route, method] of commandPaths) {
    const operation = document.paths[route][method];
    assert.ok(operation.parameters.some((parameter) => parameter.$ref === '#/components/parameters/IdempotencyKey'));
    assert.equal(operation.responses['410'].$ref, '#/components/responses/Error');
    assert.equal(operation.responses['200'], undefined);
    assert.equal(operation.responses['201'], undefined);
  }
  assert.deepEqual(document.components.schemas.ProtocolDraft.required, ['protocol_code', 'request_schema', 'expected_protocol_head_version', 'reason']);
  assert.deepEqual(document.components.schemas.FormulaDraft.required, ['required_unit_versions', 'expression', 'expected_protocol_version', 'expected_formula_head_version', 'reason']);
  const envelopeCases = document.components.schemas.TokenCommandEnvelope.allOf;
  for (const command of commands) {
    const branch = envelopeCases.find((item) => item.if?.properties?.command_type?.const === command);
    assert.ok(branch, `${command} generic envelope must be closed`);
    assert.equal(branch.then.properties.command.additionalProperties, false);
    assert.equal(branch.then.properties.expected_versions.additionalProperties, false);
    assert.ok(branch.then.properties.expected_versions.required.includes('request_fingerprint'));
  }
  const protocolPublish = envelopeCases.find((item) => item.if?.properties?.command_type?.const === 'TOKEN_PROTOCOL_PUBLISH');
  assert.deepEqual(protocolPublish.then.properties.expected_versions.required, [
    'protocol_definition_id', 'expected_protocol_version', 'draft_etag', 'formula_set_sha256',
    'model_draft_set_sha256', 'model_draft_reference_count', 'request_fingerprint',
  ]);
  for (const schema of ['TokenProtocolDraftGovernedResult', 'TokenProtocolPublishGovernedResult', 'TokenFormulaDraftGovernedResult', 'TokenFormulaPublishGovernedResult']) {
    assert.equal(document.components.schemas[schema].additionalProperties, false);
  }
  const validateResult = validator(document, 'TokenCommandResult');
  const base = { command_reference: '018f57a9-9c17-7efc-a450-35c445492301', command_type: 'TOKEN_FORMULA_PUBLISH', policy_version: 1, actor_user_id: '018f57a9-9c17-7efc-a450-35c445492302', actor_permission: 'platform.pricing.manage', authorization_version: 2, idempotency_fingerprint: 'a'.repeat(64), target_resource_type: 'FORMULA', target_resource_id: '018f57a9-9c17-7efc-a450-35c445492303', status: 'SUCCEEDED', audit_request_id: 'audit-1', created_at: '2026-09-05T10:00:00Z', completed_at: '2026-09-05T10:00:01Z' };
  const formula = { formula_version_id: base.target_resource_id, protocol_definition_id: '018f57a9-9c17-7efc-a450-35c445492304', version: 1, status: 'PUBLISHED', etag: 'b'.repeat(64), billing_sync: { status: 'SYNCHRONIZED', formula_version_id: base.target_resource_id, protocol_code: 'CHAT', required_metering_unit_definition_ids: ['018f57a9-9c17-7efc-a450-35c445492305'], formula_snapshot_sha256: 'c'.repeat(64), published_at: '2026-09-05T10:00:00Z' } };
  assert.equal(validateResult({ ...base, redacted_result: formula }), true, JSON.stringify(validateResult.errors));
  assert.equal(validateResult({ ...base, redacted_result: { ...formula, status: 'DRAFT' } }), false);
  assert.equal(validateResult({ ...base, redacted_result: { ...formula, verified_unit_count: 1 } }), false);
  assert.equal(validateResult({ ...base, redacted_result: { ...formula, expression: { INPUT_TOKEN: 'estimated_input_tokens' } } }), false);
  assert.equal(validateResult({ ...base, redacted_result: { ...formula, billing_sync: { ...formula.billing_sync, secret: 'hidden' } } }), false);
  const failed = { ...base, status: 'FAILED', stable_error_code: 'BILLING_BINDING_CONFLICT' };
  assert.equal(validateResult(failed), true, JSON.stringify(validateResult.errors));
  const failedWithoutCompleted = { ...failed }; delete failedWithoutCompleted.completed_at;
  const failedWithoutCode = { ...failed }; delete failedWithoutCode.stable_error_code;
  assert.equal(validateResult(failedWithoutCompleted), false, 'FAILED requires completed_at');
  assert.equal(validateResult(failedWithoutCode), false, 'FAILED requires stable_error_code');
  assert.equal(validateResult({ ...failed, status: 'REJECTED' }), true, JSON.stringify(validateResult.errors));
  assert.equal(validateResult({ ...failed, status: 'UNKNOWN', stable_error_code: 'COMMAND_RESULT_UNKNOWN' }), true, JSON.stringify(validateResult.errors));
  assert.equal(validateResult({ ...base, stable_error_code: 'COMMAND_PRECHECK_FAILED' }), false, 'SUCCEEDED forbids stable_error_code');
  assert.equal(validateResult({ ...base, status: 'PENDING' }), false, 'PENDING forbids completed_at');
  assert.equal(validateResult({ ...base, status: 'PENDING', completed_at: null, stable_error_code: null }), true, JSON.stringify(validateResult.errors));
  for (const invalidTime of ['0', 0, '2026-02-30T10:00:00Z']) {
    assert.equal(validateResult({ ...base, created_at: invalidTime }), false, `created_at rejects ${JSON.stringify(invalidTime)}`);
    assert.equal(validateResult({ ...failed, completed_at: invalidTime }), false, `completed_at rejects ${JSON.stringify(invalidTime)}`);
  }
  for (const sensitive of ['TOKEN_ERROR', 'API_KEY_FAILURE', 'SECRET_FAILURE', 'PASSWORD_FAILURE', 'CREDENTIAL_FAILURE']) {
    assert.equal(validateResult({ ...failed, stable_error_code: sensitive }), false, `${sensitive} must be rejected`);
  }
  assert.equal(document.paths['/token/admin/v1/commands'].post.responses['201'].content['application/json'].schema.$ref, '#/components/schemas/TokenCommandResult');
  assert.equal(document.components.schemas.FormulaPreview.additionalProperties, false);
  const validation = billingDocument.paths['/internal/v1/pricing/formula-bindings/validate'].post;
  assert.equal(validation.responses['200'].content['application/json'].schema.$ref, '#/components/schemas/FormulaDraftUnitValidation');
  assert.equal(billingDocument.components.schemas.FormulaDraftUnitValidation.additionalProperties, false);
  assert.deepEqual(billingDocument.components.schemas.FormulaDraftUnitValidation.required, ['verified_count', 'unit_versions', 'observed_at', 'unit_set_fingerprint']);
  const binding = billingDocument.paths['/internal/v1/pricing/formula-bindings/{formula_version_id}'].put;
  assert.equal(binding.responses['200'].content['application/json'].schema.$ref, '#/components/schemas/FormulaBindingReceipt');
  assert.equal(billingDocument.components.schemas.FormulaBindingReceipt.properties.status.const, 'REGISTERED');
  assert.deepEqual(billingDocument.components.schemas.FormulaBindingReceipt.required, ['formula_version_id', 'status', 'protocol_code', 'required_metering_unit_definition_ids', 'formula_snapshot_sha256', 'published_at']);
  assert.ok(!binding.requestBody.content['application/json'].schema.properties.published_at, 'published_at must be assigned by Billing DB');
});

test('APT-122-R2 uses Billing authority and recoverable immutable evidence before Token writes', async () => {
  const [billingService, billingRepository, tokenBilling, tokenRepository, governance] = await Promise.all([
    read('internal/billing/service.go'),
    read('internal/billing/formula_bindings.go'),
    read('internal/token/billing.go'),
    read('internal/token/protocol_admin.go'),
    read('internal/token/protocol_formula_governance.go'),
  ]);
  assert.match(billingService, /formula-bindings\/validate", serviceauth\.Require\("internal\.billing\.pricing\.read"\)/);
  assert.match(billingRepository, /FROM metering_unit_definitions WHERE id=ANY\(\$1\) AND status='ACTIVE'/);
  assert.match(billingRepository, /result\.VerifiedCount = len\(result\.UnitVersions\)/);
  assert.match(billingRepository, /transaction_timestamp\(\)/);
  assert.match(billingRepository, /ORDER BY id FOR SHARE/);
  assert.match(billingRepository, /formulaBindingReceipt\(ctx, tx, formulaID\)/);
  assert.match(tokenBilling, /ValidateFormulaDraftUnits/);
  assert.match(tokenRepository, /validator\.ValidateFormulaDraftUnits\(r\.Context\(\), required\)/);
  const validateIndex = tokenRepository.indexOf('validator.ValidateFormulaDraftUnits(r.Context(), required)');
  const draftWriteIndex = tokenRepository.indexOf('repo.CreateFormulaDraftCommand(r.Context()', validateIndex);
  assert.ok(validateIndex >= 0 && draftWriteIndex > validateIndex, 'Billing validation must precede the Token formula draft write');
  const syncIndex = tokenRepository.indexOf('client.RegisterFormulaBinding(r.Context()');
  const publishWriteIndex = tokenRepository.indexOf('repo.PublishFormulaCommand(r.Context()', syncIndex);
  assert.ok(syncIndex >= 0 && publishWriteIndex > syncIndex, 'Billing binding must precede the Token formula publish write');
  assert.match(tokenRepository, /FormulaSnapshotSHA256/);
  assert.match(tokenRepository, /billingSync\.FormulaVersionID != item\.ID/);
  assert.match(tokenRepository, /billingSync\.BillingStatus != "REGISTERED"/);
  assert.match(tokenRepository, /receipt\.FormulaSnapshotSHA256/);
  assert.match(tokenRepository, /PublishedAt: receipt\.PublishedAt/);
  assert.match(tokenRepository, /ProtocolPublishDependencies/);
  assert.match(tokenRepository, /protocolPublicationFormulaDigest\(lockedFormulas\)/);
  assert.match(tokenRepository, /validation\.FormulaSetSHA256 != formulaSetDigest/);
  assert.match(tokenRepository, /model_catalog_drafts WHERE protocol_code=\$1 AND protocol_version=\$2 FOR SHARE/);
  assert.match(tokenRepository, /IsoLevel: pgx\.Serializable/);
  assert.doesNotMatch(tokenRepository, /billing_sync_available/);
  assert.match(tokenRepository, /utf8\.ValidString\(reason\)/);
  assert.match(tokenRepository, /len\(\[\]rune\(reason\)\) >= 10/);
  assert.match(governance, /insertProtocolFormulaPending/);
  assert.match(governance, /RegisterFormulaBinding/);
  assert.match(governance, /runtimeExistingTokenCommand/);
  assert.match(governance, /completeRuntimeTokenCommandTx/);
  assert.match(governance, /recheckTokenRouteActivation/);
  assert.match(governance, /pendingFormulaPublishCommand/);
  assert.match(governance, /protocolFormulaTerminalFailure/);
  assert.match(governance, /loadProtocolPublicationFacts/);
  assert.match(governance, /loadFormulaPublicationFacts/);
  assert.ok(governance.indexOf('insertProtocolFormulaPending') < governance.indexOf('RegisterFormulaBinding'), 'T1 reference must be persisted before Billing');
});
