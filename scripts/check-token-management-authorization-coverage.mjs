import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// These contracts use JSON syntax, which is also valid YAML. This check needs
// no installed frontend packages and never executes Go or contacts a service.
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const quoted = (text) => [...text.matchAll(/"([^"\n]+)"/g)].map((m) => m[1]);
const sorted = (values) => [...values].sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)));
const fail = (message) => { throw new Error(message); };
// F05 delivers complete personal execution and its retry governance together.
// This phase boundary is explicit; runtime database intersections cannot enable
// a later wave or conceal a missing management command.
const expectedTokenCommands = [
  'TOKEN_PROTOCOL_DRAFT_CREATE', 'TOKEN_PROTOCOL_PUBLISH',
  'TOKEN_FORMULA_DRAFT_CREATE', 'TOKEN_FORMULA_PUBLISH',
  'TOKEN_MODEL_DRAFT_SAVE', 'TOKEN_MODEL_PUBLISH', 'TOKEN_MODEL_LIFECYCLE_CHANGE',
  'TOKEN_MODEL_CATALOG_SYNC', 'TOKEN_MODEL_CATALOG_ROLLBACK',
  'TOKEN_ACCOUNT_CREATE', 'TOKEN_ACCOUNT_STATUS_CHANGE',
  'TOKEN_ROUTE_UPSERT', 'TOKEN_ROUTE_DELETE',
  'TOKEN_CREDENTIAL_ENROLL', 'TOKEN_CREDENTIAL_ROTATE', 'TOKEN_CREDENTIAL_REVOKE',
  'TOKEN_CREDENTIAL_VALIDATE', 'TOKEN_ROUTE_PROVISIONING_REPAIR',
  'TOKEN_USAGE_RECONCILIATION_RETRY',
];

export function managementRoutes(source) {
  const prefixes = { internal: '/internal/v1', protected: '', admin: '/admin/v1', adminV2: '/admin/v2' };
  const routes = [];
  for (const line of source.split(/\r?\n/)) {
    if (!/^\s*\w+\.(GET|POST|PUT|PATCH|DELETE)\(/.test(line)) continue;
    const match = line.match(/^\s*(internal|protected|admin|adminV2)\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)",\s*(.*)\)$/);
    if (!match) fail('unrecognized management route declaration');
    const [, group, method, suffix, expression] = match;
    const adapt = expression.match(/httpserver\.AdaptFunc\(s\.(.*)\)$/);
    if (!adapt) fail('unrecognized management handler');
    let authority;
    const workload = expression.match(/serviceauth\.Require\("([^"]+)"\)/);
    const permissions = expression.match(/s\.authorizer\.(RequireAnyAnd|RequireAny|Require)\((.*?)\),/);
    if (workload) authority = { kind: 'workload', all: [workload[1]] };
    else if (permissions) {
      const [, kind, args] = permissions;
      if (kind === 'RequireAnyAnd') {
        const sets = [...args.matchAll(/\[\]string\{([^}]+)\}/g)];
        if (sets.length !== 2) fail('unrecognized conjunctive permission sets');
        authority = { kind: 'bearer', anyAnd: sets.map((set) => quoted(set[1])) };
      } else authority = { kind: 'bearer', [kind === 'Require' ? 'all' : 'any']: quoted(args) };
    } else if (adapt[1] === 'adminRevokeCredentialLaunch') {
      authority = { kind: 'bearer_actor_bound', note: 'Repository verifies the issuing actor; permission revocation must not prevent terminalizing the issued launch.' };
    } else {
      authority = { kind: 'bearer_handler_authority', note: 'Handler rechecks the command permissions or the combined Account owner authority; no unguarded public mutation.' };
    }
    const gates = [];
    if (expression.includes('requireNewTokenCommandGovernanceActive')) gates.push('ACTIVE');
    if (expression.includes('requireKnownTokenCommandGovernance')) gates.push('KNOWN_GOVERNANCE');
    if (expression.includes('requireCredentialIsolationEnabled')) gates.push('CREDENTIAL_ISOLATION');
    const policy = expression.match(/approvalpolicy\.Require\("([^"]+)", approvalpolicy\.(\w+)\)/);
    routes.push({ method: method.toLowerCase(), path: (prefixes[group] + suffix).replace(/\/:([a-z_]+)/g, '/{$1}'), handler: adapt[1], authority, gates, ...(policy ? { policy: { command: policy[1], mode: policy[2] } } : {}) });
  }
  if (!routes.length) fail('empty management route set');
  return sorted(routes);
}

export function commandSpecs(source, handlers) {
  const block = source.match(/var tokenCommandSpecs = map\[string\]tokenCommandSpec\{([\s\S]*?)\n\}/)?.[1];
  if (!block) fail('missing command specs');
  const commands = block.trim().split(/\r?\n/).map((line) => {
    const m = line.match(/^\s*"(TOKEN_[A-Z_]+)":\s*\{\[\]string\{([^}]+)\}, (true|false), (true|false), (nil|\[\]string\{[^}]*\})\},?\s*$/);
    if (!m) fail('unrecognized command spec');
    const handler = handlers.match(new RegExp('"' + m[1] + '":\\s*\\{"([^"\\n]+)", "([^"\\n]+)"'));
    if (!handler) fail('command missing actual handler');
    return { command: m[1], permissions: quoted(m[2]), recentAuth: m[3] === 'true', approvalRequired: m[4] === 'true', requiredFacts: quoted(m[5]), family: handler[1], transport: handler[2] };
  });
  assert.deepEqual(commands.map((c) => c.command).sort(), [...expectedTokenCommands].sort(), 'F05 requires the complete management and usage retry command set');
  return sorted(commands);
}

export function credentialRoutes(source) {
  const routes = [];
  for (const line of source.split(/\r?\n/)) {
    if (!/^\s*router\.(GET|POST|PUT|PATCH|DELETE)\(/.test(line)) continue;
    const m = line.match(/^\s*router\.(GET|POST)\("([^"]+)", httpserver\.AdaptFunc\(s\.(\w+)\)\)\s*$/);
    if (!m) fail('unrecognized isolated credential route');
    routes.push({ method: m[1].toLowerCase(), path: m[2], handler: m[3] });
  }
  if (!routes.length) fail('empty isolated credential route set');
  return sorted(routes);
}

function operations(document) {
  return sorted(Object.entries(document.paths).flatMap(([url, item]) => Object.entries(item)
    .filter(([method]) => ['get', 'post', 'put', 'patch', 'delete'].includes(method))
    .map(([method, op]) => ({ method, path: url, op }))));
}

function validateRefs(document) {
  const visit = (value) => {
    if (value && typeof value === 'object') {
      if (value.$ref) {
        if (!value.$ref.startsWith('#/components/schemas/') || !document.components.schemas[value.$ref.slice('#/components/schemas/'.length)]) fail('unresolved contract schema');
      }
      Object.values(value).forEach(visit);
    }
  };
  visit(document);
}

// These are required wire inputs of the current handlers, including result
// lookups. Authentication is separately described by security schemes.
const requiredParameters = {
  adminCreateTokenCommand: ['header:Idempotency-Key'],
  adminCreateLifecyclePlan: ['header:Idempotency-Key'],
  adminCancelLifecyclePlan: ['header:Idempotency-Key'],
  adminTokenAccountRecoveryAction: ['header:Idempotency-Key'],
  adminTokenCommandByIdempotencyKey: ['header:Idempotency-Key', 'query:command_type'],
  adminTokenAccountRecoveryActionByKey: ['header:Idempotency-Key'],
  adminTokenOperationsOverview: ['query:window'],
  approvalEffectObservation: ['query:command_type', 'query:command_fingerprint'],
  tokenApprovalRecoveryResult: ['query:command_type', 'query:command_fingerprint', 'query:original_key_digest'],
  adminCatalogRollbackImpact: ['query:source_catalog_version'],
  adminTokenAccountStatusImpact: ['query:target_status'],
  adminTokenRouteImpact: ['query:action', 'query:model'],
  adminStableTokenRouteImpact: ['query:action'],
  adminTokenCredentialImpact: ['query:action'],
};

export function checkCoverage({ routeSource, specSource, handlerSource, credentialSource, dtoSources, manifest, management, credential }) {
  const routes = managementRoutes(routeSource);
  const commands = commandSpecs(specSource, handlerSource);
  const isolated = credentialRoutes(credentialSource);
  assert.deepEqual(sorted(manifest.routes), routes, 'management authority/handler coverage drift');
  assert.deepEqual(sorted(manifest.commands), commands, 'management command coverage drift');
  assert.deepEqual(sorted(manifest.credentialRoutes), isolated, 'credential route coverage drift');
  const ops = operations(management);
  assert.deepEqual(ops.map(({ op, ...entry }) => entry), routes.map(({ method, path }) => ({ method, path })), 'management OpenAPI path drift');
  for (const { method, path: url, op } of ops) {
    const route = routes.find((r) => r.method === method && r.path === url);
    assert.deepEqual(op['x-authority'], route.authority, 'OpenAPI permission drift');
    assert.deepEqual(op['x-governance-gates'], route.gates, 'OpenAPI gate drift');
    assert.equal(op['x-go-handler'], route.handler, 'OpenAPI handler drift');
    const scheme = route.authority.kind === 'workload' ? 'WorkloadBearer' : 'UserBearer';
    assert.deepEqual(op.security, [{ [scheme]: [] }], 'OpenAPI authentication drift');
    const declared = (op.parameters ?? []).filter((p) => p.in === 'path').map((p) => p.name).sort();
    const actual = [...url.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();
    assert.deepEqual(declared, actual, 'OpenAPI path parameters missing');
    const required = [...(requiredParameters[route.handler] ?? [])];
    if (route.handler.startsWith('approvedTokenCommandResult')) required.push('query:approval_request_id', 'query:command_fingerprint');
    if (route.handler.startsWith('registeredTokenHandler')) required.push('header:Idempotency-Key');
    for (const parameter of required) {
      const [location, name] = parameter.split(':');
      assert.equal(op.parameters?.find((p) => p.in === location && p.name === name)?.required, true, 'required lookup/query parameter missing');
    }
    if (route.handler === 'adminCancelLifecyclePlan') assert.deepEqual(Object.keys(op.responses).sort(), ['200', 'default']);
    if (route.handler === 'adminTokenAccountRecoveryAction') assert.deepEqual(Object.keys(op.responses).sort(), ['200', '202', 'default']);
    if (route.handler === 'adminRevokeCredentialLaunch') assert.deepEqual(Object.keys(op.responses).sort(), ['204', 'default']);
    if (route.handler === 'adminTokenCommandByIdempotencyKey') {
      const lookup = op.parameters.find((p) => p.in === 'query' && p.name === 'command_type');
      assert.deepEqual(sorted(lookup.schema.enum), sorted(commands.filter((c) => c.transport === 'COMMAND').map((c) => c.command)), 'key recovery command enum drift');
    }
  }
  assert.deepEqual(management.components.schemas.TokenCommandEnvelope.properties.command_type.enum.sort(), commands.map((c) => c.command).sort(), 'command enum drift');
  assert.deepEqual(management.components.schemas.TokenCommandPrecheckRequest.properties.command_type.enum.sort(), commands.filter((c) => c.transport === 'COMMAND').map((c) => c.command).sort(), 'direct precheck command enum drift');
  for (const schema of Object.values(management.components.schemas)) {
    if (!schema['x-go-type']) continue;
    const source = dtoSources[schema['x-go-source']];
    const body = source?.match(new RegExp('type ' + schema['x-go-type'] + ' struct \\{([\\s\\S]*?)\\n\\}'))?.[1];
    if (!body) fail('contract DTO declaration missing');
    const fields = [...body.matchAll(/`json:"([^",]+)(?:,[^"]*)?"`/g)].map((m) => m[1]).filter((name) => name !== '-');
    // This DTO embeds the public impact result; all its serialized fields are
    // part of the precheck contract too, not a second response envelope.
    if (schema['x-go-type'] === 'TokenCommandPrecheckResult') fields.push(...Object.keys(management.components.schemas.ImpactPrecheckResult.properties));
    assert.deepEqual(Object.keys(schema.properties).sort(), fields.sort(), 'selected DTO field coverage drift');
  }
  assert.deepEqual(operations(credential).map(({ op, ...entry }) => entry), isolated.map(({ method, path }) => ({ method, path })), 'credential OpenAPI path drift');
  assert.deepEqual(credential.components.schemas.CredentialSubmit.required.sort(), ['allowed_models', 'base_url', 'csrf', 'provider', 'secret']);
  assert.equal(credential.components.schemas.CredentialSubmit.additionalProperties, false);
  assert.equal(credential.components.schemas.CredentialSubmit.properties.secret.writeOnly, true);
  assert.equal(credential.components.schemas.LaunchForm.additionalProperties, false);
  assert.deepEqual(credential.components.schemas.LaunchForm.required, ['launch_id']);
  validateRefs(management);
  validateRefs(credential);
  return { managementRoutes: routes.length, credentialRoutes: isolated.length, commands: commands.length };
}

export function loadInputs(base = root) {
  const read = (relative) => fs.readFileSync(path.join(base, relative), 'utf8');
  const json = (relative) => JSON.parse(read(relative));
  return {
    routeSource: read('account-platform/internal/token/management_routes.go'),
    specSource: read('account-platform/internal/token/command_governance.go'),
    handlerSource: read('account-platform/internal/token/governance_handlers.go'),
    credentialSource: read('account-platform/internal/token/credential_origin.go'),
    dtoSources: Object.fromEntries(['command_governance.go', 'command_precheck.go', 'credential_isolation.go', 'lifecycle_plans.go', 'impact_precheck.go', 'account_provisioning_saga.go', 'approval_recovery_result.go'].map((name) => ['internal/token/' + name, read('account-platform/internal/token/' + name)])),
    manifest: json('account-platform/deploy/authorization-token-management-entrypoints.json'),
    management: json('account-platform/contracts/openapi/token-management.yaml'),
    credential: json('account-platform/contracts/openapi/token-credential-origin.yaml'),
  };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { console.log(JSON.stringify({ status: 'PASS', ...checkCoverage(loadInputs()) })); }
  catch { console.error('Token management authorization contract coverage failed'); process.exitCode = 1; }
}
