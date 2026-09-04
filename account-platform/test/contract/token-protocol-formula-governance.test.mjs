import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = (relative) => readFile(path.join(root, relative), 'utf8');
const commands = [
  'TOKEN_PROTOCOL_DRAFT_CREATE',
  'TOKEN_PROTOCOL_PUBLISH',
  'TOKEN_FORMULA_DRAFT_CREATE',
  'TOKEN_FORMULA_PUBLISH',
];

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

test('APT-122 write routes remain fail-closed before their domain handlers', async () => {
  const [service, activation, handlers] = await Promise.all([
    read('internal/token/service.go'),
    read('internal/token/command_governance_activation.go'),
    read('internal/token/protocol_admin.go'),
  ]);
  const routes = new Map([
    ['TOKEN_PROTOCOL_DRAFT_CREATE', 'adminCreateProtocol'],
    ['TOKEN_PROTOCOL_PUBLISH', 'adminPublishProtocol'],
    ['TOKEN_FORMULA_DRAFT_CREATE', 'adminCreateFormula'],
    ['TOKEN_FORMULA_PUBLISH', 'adminPublishFormula'],
  ]);
  for (const [command, handler] of routes) {
    const policy = `approvalpolicy.Require("${command}", approvalpolicy.AuditOnly)`;
    const gate = 's.requireNewTokenCommandGovernanceActive()';
    const target = `httpserver.AdaptFunc(s.${handler})`;
    const policyIndex = service.indexOf(policy);
    const gateIndex = service.indexOf(gate, policyIndex);
    const handlerIndex = service.indexOf(target, gateIndex);
    assert.ok(policyIndex >= 0 && gateIndex > policyIndex && handlerIndex > gateIndex, `${command} must authorize, resolve policy, then gate before its handler`);
    assert.match(handlers, new RegExp(`"${command}"`));
  }
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
    assert.equal(operation.responses['504'].$ref, '#/components/responses/CommandResultUnknown');
  }
  assert.deepEqual(document.components.schemas.ProtocolDraft.required, ['protocol_code', 'request_schema', 'expected_protocol_head_version', 'reason']);
  assert.deepEqual(document.components.schemas.FormulaDraft.required, ['required_unit_versions', 'expression', 'expected_protocol_version', 'expected_formula_head_version', 'reason']);
  assert.equal(document.components.responses.CommandResultUnknown.content['application/json'].schema.properties.code.const, 'COMMAND_RESULT_UNKNOWN');
  assert.equal(document.components.schemas.ProtocolFormulaCommandResult.additionalProperties, false);
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

test('APT-122 uses Billing authority and immutable evidence before Token writes', async () => {
  const [billingService, billingRepository, tokenBilling, tokenRepository] = await Promise.all([
    read('internal/billing/service.go'),
    read('internal/billing/formula_bindings.go'),
    read('internal/token/billing.go'),
    read('internal/token/protocol_admin.go'),
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
});
