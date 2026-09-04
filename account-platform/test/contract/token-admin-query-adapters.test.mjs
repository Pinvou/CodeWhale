import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const read = path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('nine Token admin resources publish independent cursor and exact filters', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const resources = new Map([
    ['/token/admin/v1/models', ['q', 'lifecycle_status', 'protocol_code', 'source_adapter', 'source_missing', 'cursor', 'limit']],
    ['/token/admin/v1/protocols', ['q', 'status', 'protocol_code', 'published', 'created_from', 'created_to', 'cursor', 'limit']],
    ['/token/admin/v1/formulas', ['q', 'status', 'protocol_definition_id', 'metering_unit_code', 'published', 'created_from', 'created_to', 'cursor', 'limit']],
    ['/token/admin/v1/token-accounts', ['q', 'owner_type', 'owner_id', 'status', 'health_status', 'credential_status', 'cursor', 'limit']],
    ['/token/admin/v1/provisionings', ['q', 'status', 'model_id', 'token_account_id', 'scope_type', 'scope_id', 'sla_overdue', 'cursor', 'limit']],
    ['/token/admin/v1/usage-requests', ['q', 'execution_status', 'settlement_status', 'model', 'token_account_id', 'organization_id', 'started_from', 'started_to', 'cursor', 'limit']],
    ['/token/admin/v1/reconciliation-cases', ['q', 'status', 'severity', 'case_type', 'owner_id', 'sla_overdue', 'cursor', 'limit']],
    ['/token/admin/v1/resource-audit', ['resource_type', 'resource_id', 'action', 'actor_id', 'occurred_from', 'occurred_to', 'cursor', 'limit']],
    ['/token/admin/v1/owner-candidates', ['q', 'owner_type', 'status', 'organization_id', 'cursor', 'limit']],
  ]);
  for (const [path, expected] of resources) {
    const operation = contract.paths[path]?.get;
    assert.ok(operation, path);
    const parameters = operation.parameters.map(parameter => parameter.name ?? contract.components.parameters[parameter.$ref.split('/').at(-1)].name);
    assert.deepEqual(new Set(parameters), new Set(expected), `${path} parameters`);
    const expectedResponse = new Map([
      ['/token/admin/v1/models', '/TokenAdminModelCollection'],
      ['/token/admin/v1/protocols', '/TokenAdminProtocolCollection'],
      ['/token/admin/v1/formulas', '/TokenAdminFormulaCollection'],
      ['/token/admin/v1/token-accounts', '/TokenAdminAccountCollection'],
      ['/token/admin/v1/provisionings', '/TokenAdminProvisioningCollection'],
      ['/token/admin/v1/usage-requests', '/TokenAdminUsageCollection'],
      ['/token/admin/v1/reconciliation-cases', '/TokenAdminReconciliationCollection'],
      ['/token/admin/v1/resource-audit', '/TokenAdminResourceAuditCollection'],
      ['/token/admin/v1/owner-candidates', '/TokenAdminOwnerCandidateCollection'],
    ]).get(path);
    assert.ok(operation.responses['200'].$ref.endsWith(expectedResponse), `${path} envelope`);
    assert.deepEqual(Object.keys(operation.responses).sort(), ['200', '400', '403', '410', '422', '429', '503', '504'], `${path} stable errors`);
    for (const forbidden of ['sort', 'order', 'order_by']) assert.ok(!parameters.includes(forbidden), `${path} exposes ${forbidden}`);
  }
});

test('usage evidence publishes its independent cursor and stable error surface', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const operation = contract.paths['/token/admin/v1/usage-requests/{usage_request_id}/evidence'].get;
  const parameters = operation.parameters.map(parameter => parameter.name ?? contract.components.parameters[parameter.$ref.split('/').at(-1)].name);
  assert.deepEqual(parameters, ['usage_request_id', 'limit', 'cursor', 'evidence_type']);
  assert.ok(operation.responses['200'].$ref.endsWith('/TokenAdminUsageEvidenceCollection'));
  assert.deepEqual(Object.keys(operation.responses).sort(), ['200', '400', '403', '410', '422', '429', '503', '504']);
});

test('provisioning attempts publish an independent bounded cursor and closed projection', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const operation = contract.paths['/token/admin/v1/provisionings/{provisioning_id}/attempts'].get;
  const parameters = operation.parameters.map(parameter => parameter.name ?? contract.components.parameters[parameter.$ref.split('/').at(-1)].name);
  assert.deepEqual(parameters, ['provisioning_id', 'limit', 'cursor', 'status']);
  assert.ok(operation.responses['200'].$ref.endsWith('/TokenAdminProvisioningAttemptCollection'));
  assert.deepEqual(Object.keys(operation.responses).sort(), ['200', '400', '403', '404', '410', '422', '429', '503', '504']);
  assert.equal(contract.components.schemas.TokenAdminProvisioningAttempt.additionalProperties, false);
  assert.ok(contract.components.schemas.TokenAdminProvisioningAttemptPage.allOf[1].properties.items.items.$ref.endsWith('/TokenAdminProvisioningAttempt'));
  for (const hidden of ['worker_id', 'error_detail']) assert.ok(!Object.hasOwn(contract.components.schemas.TokenAdminProvisioningAttempt.properties, hidden));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  const validate = ajv.compile(contract.components.schemas.TokenAdminProvisioningAttempt);
  const attempt = { attempt_id: '00000000-0000-4000-8000-000000000201', repair_round: 0, attempt_number: 1, status: 'FAILED', stage: 'VERIFY', error_code: 'ROUTE_UNAVAILABLE', started_at: '2026-09-04T12:00:00Z' };
  assert.equal(validate(attempt), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...attempt, worker_id: 'private-worker' }), false);
  assert.equal(validate({ ...attempt, error_detail: 'provider secret' }), false);
});

test('versioned provisioning detail is a closed redacted projection without inline attempts', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const operation = contract.paths['/token/admin/v2/provisionings/{provisioning_id}'].get;
  assert.equal(operation.responses['200'].content['application/json'].schema.$ref, '#/components/schemas/TokenAdminProvisioningDetail');
  const schema = contract.components.schemas.TokenAdminProvisioningDetail;
  assert.equal(schema.additionalProperties, false);
  for (const hidden of ['attempts', 'worker_id', 'error_detail', 'last_error_detail', 'lease_owner', 'lease_until', 'active_credential_id']) assert.ok(!Object.hasOwn(schema.properties, hidden));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  const validate = ajv.compile(schema);
  const detail = {
    provisioning_id: '00000000-0000-4000-8000-000000000301', scope_type: 'PERSONAL', scope_id: '00000000-0000-4000-8000-000000000302',
    model_id: '00000000-0000-4000-8000-000000000303', protocol_code: 'CHAT', desired_generation: 2,
    status: 'FAILED', stage: 'VERIFY', policy_version: 1, attempt_count: 1, repair_round: 0, max_attempts: 3,
    sla_deadline_at: '2026-09-04T13:00:00Z', updated_at: '2026-09-04T12:00:00Z',
  };
  assert.equal(validate(detail), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...detail, attempts: [] }), false);
});

test('repair and reconciliation mutations publish bounded idempotency and conflict contracts', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const repair = contract.paths['/token/admin/v1/provisionings/{provisioning_id}/repair'].post;
  const retry = contract.paths['/token/admin/v1/usage-requests/{usage_request_id}/retry-reconciliation'].post;
  const names = repair.parameters.map(parameter => parameter.name ?? contract.components.parameters[parameter.$ref.split('/').at(-1)].name);
  assert.deepEqual(names, ['provisioning_id', 'Idempotency-Key']);
  assert.deepEqual(Object.keys(repair.responses).sort(), ['202', '400', '403', '404', '409']);
  assert.deepEqual(Object.keys(retry.responses).sort(), ['200', '400', '403', '404', '409']);
  const key = contract.components.parameters.IdempotencyKey.schema;
  assert.deepEqual({ minLength: key.minLength, maxLength: key.maxLength }, { minLength: 16, maxLength: 128 });
  const reason = contract.components.schemas.Reason.properties.reason;
  assert.deepEqual({ minLength: reason.minLength, maxLength: reason.maxLength }, { minLength: 1, maxLength: 500 });
});

test('all paginated Token projections and pages are closed schemas', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  for (const item of ['TokenAdminProtocol', 'TokenAdminFormula', 'TokenAdminAccountSummary', 'TokenAdminUsageSummary', 'TokenAdminUsageDetail', 'TokenAdminReconciliationCase', 'TokenAdminOwnerCandidate', 'TokenAdminResourceAudit', 'TokenAdminUsageEvidence', 'TokenAdminProvisioningAttempt']) {
    assert.equal(contract.components.schemas[item].additionalProperties, false, item);
  }
  assert.equal(contract.components.schemas.RouteProvisioning.additionalProperties, false);
  assert.equal(contract.components.schemas.TokenAdminOwnerCandidate.properties.status.enum.includes('FROZEN'), false);
  for (const page of ['TokenAdminModelPage', 'TokenAdminProtocolPage', 'TokenAdminFormulaPage', 'TokenAdminAccountPage', 'TokenAdminProvisioningPage', 'TokenAdminProvisioningAttemptPage', 'TokenAdminUsagePage', 'TokenAdminReconciliationPage', 'TokenAdminOwnerCandidatePage', 'TokenAdminResourceAuditPage', 'TokenAdminUsageEvidencePage']) {
    assert.ok(contract.components.schemas[page].allOf[0].$ref.endsWith('/AdminPaginationEnvelope'), page);
    assert.ok(contract.components.schemas[page].allOf[1].properties.items.items.$ref, `${page} item`);
  }
  assert.ok(contract.components.parameters.Cursor.schema.$ref.endsWith('/AdminPaginationCursor'));
  assert.ok(contract.components.responses.TokenAdminQueryLimited.content['application/json'].schema.properties.code.allOf[0].$ref.endsWith('/AdminPaginationErrorCode'));
});

test('closed model and legacy usage schemas validate production JSON shapes', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  const modelSchema = structuredClone(contract.components.schemas.AdminCatalogModel);
  modelSchema.properties.draft = contract.components.schemas.ModelDraft;
  const validateModel = ajv.compile(modelSchema);
  const model = {
    model_id: '11111111-1111-4111-8111-111111111111', catalog_version: 3,
    display_name: 'Model One', description: '', aliases: ['model-one'], lifecycle_status: 'PUBLISHED',
    protocol_code: 'CHAT', protocol_version: 1,
    protocol_definition_id: '22222222-2222-4222-8222-222222222222',
    model_capability_version_id: '33333333-3333-4333-8333-333333333333', capability_version: 2,
    context_length: 128000, maximum_output_tokens: 8192, supported_modalities: ['TEXT'],
    tokenizer_version: 'v1', source_available: true, source_missing_confirmations: 0,
    force_cancel_inflight: false,
  };
  assert.equal(validateModel(model), true, JSON.stringify(validateModel.errors));
  assert.equal(validateModel({ ...model, provider_id: 'must-not-leak' }), false);

  const validateUsage = ajv.compile(contract.components.schemas.AdminUsageRequest);
  const usage = {
    usage_request_id: '44444444-4444-4444-8444-444444444444',
    user_id: '55555555-5555-4555-8555-555555555555', session_id: 'session-id',
    token_account_id: '66666666-6666-4666-8666-666666666666',
    credential_id: '77777777-7777-4777-8777-777777777777',
    wallet_id: '88888888-8888-4888-8888-888888888888', model: 'model-one', status: 'RECEIVED',
    request_fingerprint: 'digest', execution_status: 'RECEIVED', delivery_status: 'NOT_STARTED',
    cancel_status: 'NOT_REQUESTED', settlement_status: 'HOLD_PENDING', maximum_microcredits: 100,
    reconciliation_attempts: 0, last_persisted_sequence: 0, last_client_confirmed_sequence: 0,
    cancellation_evidence: null, convergence_evidence: null,
    started_at: '2026-09-03T00:00:00Z', updated_at: '2026-09-03T00:00:00Z',
  };
  assert.equal(validateUsage(usage), true, JSON.stringify(validateUsage.errors));
  const legacyItem = contract.components.responses.TokenAdminUsageCollection.content['application/json'].schema.oneOf[0].properties.items.items.$ref;
  assert.equal(legacyItem, '#/components/schemas/AdminUsageRequest');

  const validateUsageDetail = ajv.compile(contract.components.schemas.TokenAdminUsageDetail);
  const detail = {
    usage_request_id: usage.usage_request_id,
    token_account_id: usage.token_account_id,
    model: usage.model,
    status: usage.status,
    execution_status: usage.execution_status,
    delivery_status: usage.delivery_status,
    cancel_status: usage.cancel_status,
    settlement_status: usage.settlement_status,
    maximum_microcredits: usage.maximum_microcredits,
    reconciliation_attempts: usage.reconciliation_attempts,
    started_at: usage.started_at,
    updated_at: usage.updated_at,
  };
  assert.equal(validateUsageDetail(detail), true, JSON.stringify(validateUsageDetail.errors));
  assert.equal(validateUsageDetail({ ...detail, reconciliation_last_error: 'MANUAL_RECONCILIATION_REQUESTED' }), true, JSON.stringify(validateUsageDetail.errors));
  for (const unsafe of ['SECRET', 'API_KEY', 'PASSWORD', 'CUSTOMER_PHONE_13800138000', '_CODE', '1CODE', 'customer phone 13800138000']) {
    assert.equal(validateUsageDetail({ ...detail, reconciliation_last_error: unsafe }), false, `${unsafe} is not in the controlled diagnostic vocabulary`);
  }
  for (const [field, value] of [['user_id', usage.user_id], ['session_id', usage.session_id], ['credential_id', usage.credential_id], ['request_fingerprint', usage.request_fingerprint], ['provider_id', 'secret-provider'], ['upstream_request_id', 'upstream-secret'], ['convergence_evidence', {}]]) {
    assert.equal(validateUsageDetail({ ...detail, [field]: value }), false, `${field} must not enter the Portal detail`);
  }
  assert.equal(contract.paths['/token/admin/v1/usage-requests/{usage_request_id}'].get.responses['200'].content['application/json'].schema.$ref, '#/components/schemas/TokenAdminUsageDetail');
  assert.equal(contract.paths['/token/admin/v1/usage-requests/{usage_request_id}/retry-reconciliation'].post.responses['200'].content['application/json'].schema.$ref, '#/components/schemas/TokenAdminUsageDetail');
});

test('impact precheck covered and uncovered price evidence validate as closed alternatives', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  const validate = ajv.compile(contract.components.schemas.TokenImpactPrecheck);
  const base = {
    action: 'MODEL_LIFECYCLE_DEPRECATED',
    evidence: {
      resource_type: 'MODEL', resource_id: '11111111-1111-4111-8111-111111111111', current_status: 'PUBLISHED',
      current_version: { evidence_etag: `"impact-${'a'.repeat(64)}"`, evidence_set_sha256: 'b'.repeat(64) },
      dependencies: { routes: 0, healthy_routes: 0, provisionings: 0, ready_provisionings: 0, active_leases: 0, inflight_requests: 0, active_sessions: 0, alternative_credentials: 0, alternative_accounts: 0, affected_coverage_tuples: 0, remaining_ready_coverage: 0, coverage_gaps: 0 },
    },
    blockers: [], token_observed_at: '2026-09-03T00:00:00Z', billing_observed_at: '2026-09-03T00:00:01Z',
    snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', precheck_fingerprint: 'c'.repeat(64), requires_cas_revalidation: true,
  };
  const common = { model_id: '11111111-1111-4111-8111-111111111111', protocol_code: 'CHAT', credit_unit: 'MICROCREDIT' };
  const covered = { ...base, price_coverage: { models_checked: 1, models_covered: 1, fully_covered: true, billing_observed_at: base.billing_observed_at, versions: [{ ...common, covered: true, formula_version_id: '22222222-2222-4222-8222-222222222222', price_version_id: '33333333-3333-4333-8333-333333333333', effective_from: '2026-09-01T00:00:00Z' }] } };
  const uncovered = { ...base, price_coverage: { models_checked: 1, models_covered: 0, fully_covered: false, billing_observed_at: base.billing_observed_at, versions: [{ ...common, covered: false }] } };
  assert.equal(validate(covered), true, JSON.stringify(validate.errors));
  assert.equal(validate(uncovered), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...uncovered, price_coverage: { ...uncovered.price_coverage, versions: [{ ...common, covered: false, price_version_id: '33333333-3333-4333-8333-333333333333' }] } }), false);
  assert.equal(validate({ ...covered, price_coverage: { ...covered.price_coverage, versions: [{ ...covered.price_coverage.versions[0], provider: 'must-not-leak' }] } }), false);
  const dependencies = contract.components.schemas.TokenImpactPrecheck.properties.evidence.properties.dependencies.properties;
  assert.match(dependencies.affected_coverage_tuples.description, /scope_type, scope_id, wallet_id, model_id, upstream_model_ref, protocol_code/);
  assert.match(dependencies.remaining_ready_coverage.description, /exact currently serviceable match/);
  assert.match(dependencies.coverage_gaps.description, /coverage_gaps = affected_coverage_tuples - remaining_ready_coverage/);
});

test('impact precheck freezes exact coverage tuples and separate credential candidates', async () => {
  const source = await read('internal/token/impact_precheck.go');
  assert.match(source, /candidate\.status IN \('PENDING','RETIRED'\) AND candidate\.validation_status='VALID'/);
  assert.match(source, /candidate\.token_account_id=\$1 AND candidate\.id<>\$3/);
  assert.match(source, /candidate\.provider<>affected\.required_provider OR NOT affected\.upstream_model_ref=ANY\(candidate\.allowed_models\)/);
  assert.match(source, /candidate\.scope_type=affected\.scope_type AND candidate\.scope_id=affected\.scope_id AND candidate\.wallet_id=affected\.wallet_id AND candidate\.model_id=affected\.model_id AND candidate\.upstream_model_ref=affected\.upstream_model_ref AND candidate\.protocol_code=affected\.protocol_code/);
  assert.match(source, /AffectedCoverageTuples.*RemainingReadyCoverage.*CoverageGaps/s);
  assert.match(source, /appendImpactKeyRowsIterator\(ctx, digest, rows\)/);
});

test('query adapters freeze authoritative total orders and isolated resource names', async () => {
  const [queries, handlers, service] = await Promise.all([read('internal/token/admin_queries.go'), read('internal/token/admin_queries_http.go'), read('internal/token/service.go')]);
  for (const order of [
    'normalized_name ASC,model_id ASC', 'protocol_code ASC,version DESC,id ASC',
    'updated_at DESC,id ASC', 'updated_at DESC,id DESC', 'occurred_at DESC,id DESC',
    'normalized_display_label ASC,owner_type ASC,owner_id ASC', 'repair_round ASC,attempt_number ASC,id ASC',
    'observed_at ASC,source_kind ASC,source_id ASC',
  ]) assert.ok(queries.includes(order), order);
  for (const resource of ['models', 'protocols', 'formulas', 'token-accounts', 'provisionings', 'usage-requests', 'reconciliation-cases', 'owner-candidates']) {
    assert.match(handlers, new RegExp(`"${resource}"`));
  }
  assert.match(handlers, /resource-audit:"\+strings\.ToLower\(resourceType\)/);
  assert.match(handlers, /authorization\.GrantedPermission\(r\.Context\(\), requiredRead\)/);
  assert.match(service, /RequireAnyAnd\(\[\]string\{"platform\.models\.read", "platform\.tokens\.read"\}, \[\]string\{"platform\.audit\.read"\}\)/u);
  assert.match(queries, /review\.domain='TOKEN' AND review\.case_type IN \('USAGE_RECONCILIATION_REVIEW'\) AND review\.resource_type='USAGE_REQUEST' AND EXISTS\(SELECT 1 FROM usage_requests request WHERE request\.id=review\.resource_id\)/u);
  assert.doesNotMatch(queries, /review\.case_type ILIKE|review\.resource_type ILIKE/u);
  assert.ok((queries.match(/adminUsageReconciliationCasePredicate/g) ?? []).length >= 3, 'snapshot and loader share the authoritative reconciliation predicate');
});

test('owner projection stays service-auth only and evidence has a cross-table identity', async () => {
  const [owner, account, tokenService, portal, queryHTTP, queries, helm] = await Promise.all([
    read('internal/token/owner_candidates.go'), read('internal/account/directory.go'),
    read('internal/token/service.go'), read('internal/portal/service.go'), read('internal/token/admin_queries_http.go'),
    read('internal/token/admin_queries.go'), read('deploy/helm/account-platform/templates/app-deployments.yaml'),
  ]);
  assert.match(owner, /internal\/v1\/token-owner-candidates/);
  assert.match(owner, /serviceauth\.SetAuthorization/);
  assert.match(owner, /cases\.Fold\(\)\.String\(norm\.NFKC\.String/);
  assert.match(account, /membership\.organization_id=\$6/);
  assert.match(account, /unit\.organization_id=\$6/);
  assert.match(account, /account_user\.status IN \('ACTIVE','DISABLED','PROVISIONING'\)/);
  assert.match(account, /organization\.status IN \('ACTIVE','DISABLED'\)/);
  assert.match(account, /unit\.status IN \('ACTIVE','DISABLED'\)/);
  assert.doesNotMatch(account, /ELSE 'PROVISIONING'/);
  assert.match(tokenService, /platform\.tokens\.owner-candidates\.read/);
  assert.match(portal, /\/admin\/v1\/owner-candidates/);
  assert.match(portal, /X-Pinvou-Client-Source-Signature/);
  assert.match(queryHTTP, /hmac\.Equal\(provided, mac\.Sum\(nil\)\)/);
  assert.match(helm, /token-portal[\s\S]*TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64/);
  assert.match(queries, /'CANCELLATION'::text source_kind/);
  assert.match(queries, /'CONVERGENCE'/);
  assert.match(queries, /extract\(epoch FROM observed_at\)[\s\S]*source_kind\|\|'\:'\|\|source_id::text/);
  assert.match(queries, /AdminUsageSummary/);
  for (const sensitive of ['credential_id', 'session_id', 'request_fingerprint', 'upstream_request_id', 'provider_id']) {
    const loader = queries.slice(queries.indexOf('func (r *PostgresRepository) adminUsageByIDs'), queries.indexOf('func (r *PostgresRepository) adminReconciliationByIDs'));
    assert.doesNotMatch(loader, new RegExp(sensitive), `usage list leaked ${sensitive}`);
  }
});

test('PostgreSQL gate applies the production Token migration chain and production loaders/handler', async () => {
  const integration = await read('internal/token/admin_queries_integration_test.go');
  assert.match(integration, /migrations\.Load\("\.\.\/\.\.\/migrations\/token"\)/);
  assert.match(integration, /migrations\.Up\(ctx, connection, loaded\)/);
  assert.doesNotMatch(integration, /CREATE TABLE model_catalog_versions|CREATE TABLE token_accounts/);
  for (const loader of ['adminModelsByIDs', 'adminProtocolsByIDs', 'adminFormulasByIDs', 'adminTokenAccountsByIDs', 'adminProvisioningsByIDs', 'adminProvisioningAttemptsByIDs', 'adminUsageByIDs', 'adminReconciliationByIDs', 'adminAuditByIDs', 'adminEvidenceByIDs']) {
    assert.match(integration, new RegExp(`repository\\.${loader}`), loader);
  }
  assert.match(integration, /service\.Handler\("", verify\)/);
  assert.match(integration, /audit matrix[\s\S]*authorization-version continuation/);
  const workflow = await read('../.github/workflows/pr-check.yml');
  assert.match(workflow, /TestTokenAdminQueryAdaptersPostgresIntegration/);
  assert.match(workflow, /! grep -F -- '--- SKIP:'/);
});
