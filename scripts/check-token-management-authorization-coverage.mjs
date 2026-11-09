import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { authorizationRoutes, goSource } from './check-account-platform-authorization-coverage.mjs';

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

// Keep the declarative route grammar strict, but find every real method
// selector before parsing it. Mask Go comments/literals with equal-length
// spaces so examples and raw strings cannot create or hide route candidates.
function lexicalRouteSource(source) {
  let code = ''; let masked = ''; let region = '';
  const blank = char => char === '\n' || char === '\r' ? char : ' ';
  for (let index = 0; index < source.length; index += 1) {
    const char = source[index]; const next = source[index + 1];
    if (region === 'line') {
      code += blank(char); masked += blank(char);
      if (char === '\n') region = '';
    } else if (region === 'block') {
      code += blank(char); masked += blank(char);
      if (char === '*' && next === '/') {
        code += ' '; masked += ' '; index += 1; region = '';
      }
    } else if (region) {
      code += char; masked += blank(char);
      if (region !== '`' && char === '\\' && next !== undefined) {
        code += next; masked += blank(next); index += 1;
      } else if (char === region) region = '';
    } else if (char === '/' && (next === '/' || next === '*')) {
      code += '  '; masked += '  '; index += 1;
      region = next === '/' ? 'line' : 'block';
    } else if (char === '"' || char === "'" || char === '`') {
      code += char; masked += ' '; region = char;
    } else {
      code += char; masked += char;
    }
  }
  if (region && region !== 'line') fail('unrecognized route source lexical region');
  return { code, lines: code.split(/\r?\n/), masked };
}

function routeDeclarationLines(source) {
  const { lines, masked } = lexicalRouteSource(source);
  const candidates = new Set();
  const selectors = /\.\s*(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|Handle|Any|Match|Static|StaticFS|StaticFile|StaticFileFS)\b/g;
  for (const match of masked.matchAll(selectors)) {
    if (match[0].includes('\n')) fail('unrecognized route declaration');
    const lineIndex = masked.slice(0, match.index).split('\n').length - 1;
    if (candidates.has(lineIndex)) fail('unrecognized multiple route declarations');
    candidates.add(lineIndex);
  }
  return [...candidates].map(index => lines[index].trimEnd());
}

export function managementRoutes(source) {
  const prefixes = { internal: '/internal/v1', protected: '', admin: '/admin/v1', adminV2: '/admin/v2' };
  const routes = [];
  for (const line of routeDeclarationLines(source)) {
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


export function organizationFinanceRoutes(source, handlers) {
  const masked = lexicalRouteSource(source).masked;
  const assemblyGate = /if !s\.managementEnabled \{\s*return\s*\}/.exec(masked);
  if (!assemblyGate) fail('organization management assembly gate missing');
  for (const declaration of masked.matchAll(/\broutes\.(?:GET|POST|PUT)\b/g)) {
    if (declaration.index < assemblyGate.index + assemblyGate[0].length) fail('organization route precedes management assembly gate');
  }
  const maskedHandlers = lexicalRouteSource(handlers).masked;
  const realFunctionBody = name => {
    const prefix = `func (s *Service) ${name}(`;
    const functions = new RegExp('func \\(s \\*Service\\) ' + name + '\\([^\\n]*\\) [^\\n]*\\{([\\s\\S]*?)\\n\\}', 'g');
    for (const match of handlers.matchAll(functions)) {
      if (maskedHandlers.startsWith(prefix, match.index)) return match[1];
    }
    return undefined;
  };
  // Keep literal arguments available for the finite supported call shapes,
  // but require each match to start in executable Go code. Examples in a
  // comment or string must never supply authority for a real handler.
  const realMatches = (text, pattern, rootSelector = false) => {
    const { code, masked } = lexicalRouteSource(text);
    return [...code.matchAll(pattern)].filter(match => {
      const before = masked.slice(0, match.index);
      // Go identifiers include Unicode letters/digits. JavaScript's ASCII
      // word boundary alone can match the tail of a different identifier.
      if (masked[match.index] === ' ' || /[\p{L}\p{Nd}_]$/u.test(before.slice(-2))) return false;
      // A selector may continue after whitespace or a comment following its
      // dot. Such a field named s is not this handler's root Service receiver.
      return !rootSelector || !before.trimEnd().endsWith('.');
    });
  };
  const routes = routeDeclarationLines(source).map(line => {
    const match = line.match(/^\s*routes\.(GET|POST|PUT)\("([^"]+)",\s*(.*)\)$/);
    if (!match) fail('unrecognized organization route declaration');
    const [, method, suffix, expression] = match;
    const shape = expression.match(/^(?:httpserver\.RequirePathSuffix\("([^"]+)"\), )?(?:approvalpolicy\.Require\("[A-Z_]+", approvalpolicy\.\w+\), )?httpserver\.AdaptFunc\(s\.(\w+)\)$/);
    const pathGuard = shape?.[1];
    const handler = shape?.[2];
    if (!handler || (pathGuard && !suffix.endsWith(pathGuard)) || !shape) fail('unrecognized organization handler');
    // A whole /:parameter is not an action. The supported action syntax is a
    // final literal resource:action segment, guarded before every other handler.
    const segments = suffix.split('/');
    const actions = segments.filter(segment => segment.includes(':') && !/^:[a-z_]+$/.test(segment));
    if (actions.length > 1 || (actions.length === 1 && (actions[0] !== segments.at(-1)
      || !/^[a-z][a-z0-9-]*:[a-z][a-z0-9-]*$/.test(actions[0])))) fail('unrecognized organization action path');
    if (actions.length === 1 && pathGuard !== '/' + actions[0]) fail('organization action path guard missing or mismatched');
    const body = realFunctionBody(handler);
    if (!body) fail('organization handler source missing');
    let permission;
    if (handler === 'organizationModelPolicy') {
      if (realMatches(body, /\bpermission := "org\.models\.read"/g).length !== 1
        || realMatches(body, /\bif r\.Method == http\.MethodPut \{\s*permission = "org\.models\.manage"/g).length !== 1
        || realMatches(body, /\bs\.authorizer\.AuthorizedOrganizationUserID\(r\.Context\(\), principal, permission, organizationID\)/g, true).length !== 1) fail('organization model authority changed');
      permission = method === 'PUT' ? 'org.models.manage' : 'org.models.read';
    } else if (handler === 'reclaimClosedGrant' || handler === 'reclaimClosedGrantsBatch') {
      // These two existing handlers share one explicit authority helper. Keep
      // the call chain strict; an unknown helper must not acquire an authority.
      const helper = realFunctionBody('closedGrantClient');
      const realStatement = (text, statement, selector) => {
        const lexical = lexicalRouteSource(text);
        const maskedLines = lexical.masked.split(/\r?\n/);
        return lexical.lines.some((line, index) => statement.test(line) && maskedLines[index].includes(selector));
      };
      if (!helper || !realStatement(body, /^\s*resolved, org, client, ok := s\.closedGrantClient\(w, r\)\s*$/, 's.closedGrantClient(w, r)')) fail('closed grant authority helper missing');
      if (!realStatement(helper, /^\s*resolved, org, _, ok := s\.resolveOrganizationBilling\(w, r, "org\.grants\.reclaim_closed"\)\s*$/, 's.resolveOrganizationBilling(w, r,')) fail('closed grant authority changed');
      permission = 'org.grants.reclaim_closed';
    } else {
      const resolvers = realMatches(body, /\bs\.resolveOrganizationBilling\(w, r, "(org\.[a-z.]+)"\)/g, true);
      if (resolvers.length > 1) fail('organization permission source ambiguous');
      permission = resolvers[0]?.[1];
    }
    if (!permission) fail('organization permission source missing');
    const policy = expression.match(/approvalpolicy\.Require\("([^"]+)", approvalpolicy\.(\w+)\)/);
    return { method: method.toLowerCase(), path: '/v1' + suffix.replace(/\/:([a-z_]+)/g, '/{$1}'), handler,
      authority: { kind: 'bearer_organization', all: [permission] }, gates: ['MANAGEMENT_ENABLED'],
      ...(policy ? { policy: { command: policy[1], mode: policy[2] } } : {}) };
  });
  if (!routes.length) fail('empty organization route set');
  return sorted(routes);
}

// F06 delivers exactly these ordinary and Admin member-unit PUT contracts.
// Admin is an AuditOnly command; ordinary authority remains in the member handler.
function checkMemberUnitsContract(document, source) {
  const suffix = '/organizations/:organization_id/members/:user_id/units';
  const definitions = [
    { group: 'protected', path: '/v1' + suffix.replace(/\/:([a-z_]+)/g, '/{$1}'), handler: 'replaceMemberUnits', permission: 'org.members.manage' },
    { group: 'admin', path: '/admin/v1' + suffix.replace(/\/:([a-z_]+)/g, '/{$1}'), handler: 'adminReplaceMemberUnits', permission: 'platform.organizations.members.manage', policy: { command: 'ACCOUNT_MEMBER_UNITS_REPLACE', mode: 'AuditOnly' } },
  ];
  const actual = [];
  for (const line of routeDeclarationLines(source)) {
    if (!line.includes(suffix) && !/s\.(?:adminReplaceMemberUnits|replaceMemberUnits)\b/.test(line)) continue;
    const match = line.match(/^\s*(protected|admin)\.PUT\("([^"]+)",\s*(.*)\)$/);
    assert.ok(match && match[2] === (match[1] === 'protected' ? '/v1' : '') + suffix, 'member unit route drift');
    const definition = definitions.find(item => item.group === match[1]);
    const prefix = definition.policy ? 's.requirePermission(PermissionPlatformOrganizationsMembersManage), approvalpolicy.Require("ACCOUNT_MEMBER_UNITS_REPLACE", approvalpolicy.AuditOnly), ' : '';
    assert.equal(match[3], prefix + `httpserver.AdaptFunc(s.${definition.handler})`, 'member unit handler or policy drift');
    actual.push(definition);
  }
  assert.deepEqual(sorted(actual), sorted(definitions), 'member unit route coverage drift');
  const ops = operations(document);
  assert.deepEqual(ops.map(({ method, path }) => ({ method, path })), sorted(definitions.map(({ path }) => ({ method: 'put', path }))), 'member unit contract route drift');
  for (const { path: url, op } of ops) {
    const definition = definitions.find(item => item.path === url);
    assert.equal(op['x-service'], 'account', 'member unit service drift');
    assert.equal(op.operationId, `account_${definition.handler}`, 'member unit operation drift');
    assert.equal(op['x-go-handler'], definition.handler, 'member unit handler drift');
    assert.equal(op['x-permission'], definition.permission, 'member unit permission drift');
    assert.deepEqual(op['x-approval-policy'], definition.policy, 'member unit approval policy drift');
    assert.deepEqual(op.security, [{ UserBearer: [] }], 'member unit authentication drift');
  }
  assert.doesNotThrow(() => validateRefs(document), 'member unit schema reference drift');
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
  for (const line of routeDeclarationLines(source)) {
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

// This is the finite installed invocation contract, not a general Go/OpenAPI
// interpreter. Its success responses remain intentionally partial projections.
export function checkInvocationCoverage({ invocation, invocationSources }) {
  const read = name => {
    const text = invocationSources[name];
    assert.equal(typeof text, 'string', `invocation source missing: ${name}`);
    return text;
  };
  const actualFunction = (sourceText, name) => {
    const source = goSource(sourceText);
    const declarations = [...source.code.matchAll(new RegExp('func \\(s \\*Service\\) ' + name + '\\([^\\n]+\\{([\\s\\S]*?)\\n\\}', 'g'))].filter(match => source.masked[match.index] !== ' ');
    assert.equal(declarations.length, 1, 'invocation actual handler declaration missing or duplicated');
    return declarations[0][1];
  };
  // Keep each compact character tied to its original lexical region. A code
  // sample inside a Go comment or string cannot stand in for an installed call.
  const containsCode = (sourceText, expected) => {
    const { code, masked } = goSource(sourceText);
    let text = ''; let mask = '';
    for (let index = 0; index < code.length; index += 1) {
      if (!/\s/.test(code[index])) { text += code[index]; mask += masked[index]; }
    }
    for (let index = text.indexOf(expected); index >= 0; index = text.indexOf(expected, index + 1)) {
      if (mask[index] !== ' ') return true;
    }
    return false;
  };
  const personalSource = actualFunction(read('token/personal_execution_routes.go'), 'registerPersonalExecutionRoutes');
  const tokenService = actualFunction(read('token/service.go'), 'Handler');
  assert.ok(containsCode(tokenService, 's.registerPersonalExecutionRoutes(router,protected)'), 'invocation route installation drift');
  assert.ok(containsCode(tokenService, 'protected:=router.Group("/v1")') && containsCode(tokenService, 'protected.Use(identity.GinMiddleware(verify))'), 'invocation user authentication drift');
  assert.ok(containsCode(personalSource, 'internal.Use(serviceauth.GinMiddleware(s.managementServiceToken))'), 'invocation internal authentication drift');
  assert.ok(containsCode(actualFunction(read('billing/service.go'), 'Handler'), 'protected.Use(serviceauth.GinMiddleware(serviceToken))'), 'invocation Billing authentication drift');
  assert.ok(containsCode(actualFunction(read('account/service.go'), 'Handler'), 'internal.Use(serviceauth.GinMiddleware(s.serviceToken))'), 'invocation Account authentication drift');
  const human = {
    listCatalogModels: ['personal.models.read', 'token/personal_catalog_http.go', 'resolveModelUsagePermission'],
    modelProtocols: ['personal.models.read', 'token/personal_execution_http.go', 'resolveModelUsagePermission'],
    chatCompletions: ['personal.model.invoke', 'token/proxy.go', 'resolveModelUsagePermission'],
    getOwnedUsageByIdempotencyKey: ['personal.usage.cancel', 'token/personal_execution_http.go', 'authorizeOriginalUsageUser'],
    getUsageRequest: ['personal.usage.read', 'token/personal_execution_http.go', 'resolveModelUsagePermission'],
    cancelUsageRequest: ['personal.usage.cancel', 'token/personal_execution_http.go', 'authorizeOriginalUsageUser'],
    listMyUsageRequests: ['personal.usage.read', 'token/personal_execution_http.go', 'resolveModelUsagePermission'],
    listMyUsageTurns: ['personal.usage.read', 'token/personal_execution_http.go', 'resolveModelUsagePermission'],
  };
  const routes = [];
  for (const [service, name, prefixes] of [
    ['token', 'token/personal_execution_routes.go', { protected: '/v1', internal: '/internal/v1', router: '' }],
    ['billing', 'billing/service.go', { protected: '' }],
    ['account', 'account/service.go', { internal: '/internal/v1' }],
  ]) {
    for (const route of authorizationRoutes(name, read(name))) {
      const handler = route.registration.match(/httpserver\.AdaptFunc\(s\.(\w+)\)/)?.[1];
      if (service === 'billing' && !['extendHold', 'commit'].includes(handler)) continue;
      if (service === 'account' && handler !== 'internalClientCompatibility') continue;
      assert.ok(Object.hasOwn(prefixes, route.receiver) && handler, 'invocation route shape drift');
      const url = (prefixes[route.receiver] + route.path).replace(/:([a-z_]+)/g, '{$1}');
      let permission = route.registration.match(/serviceauth\.Require\("([^"]+)"\)/)?.[1];
      if (human[handler]) {
        const [expected, file, resolver] = human[handler];
        const functionBody = actualFunction(read(file), handler);
        const { code, masked } = goSource(functionBody);
        const calls = [...code.matchAll(/\bs\.(resolveModelUsagePermission|authorizeOriginalUsageUser)\(\s*w,\s*r,\s*"([^"]+)"/g)].filter(match => masked[match.index] !== ' ');
        assert.equal(calls.length, 1, 'invocation handler authority shape drift');
        assert.equal(calls[0][1], resolver, 'invocation handler authority resolver drift');
        assert.equal(calls[0][2], expected, 'invocation handler authority drift');
        permission = expected;
      }
      if (handler === 'clientCompatibility') permission = 'PUBLIC';
      assert.ok(permission, 'invocation authority missing');
      routes.push({ service, method: route.method.toLowerCase(), path: url, handler, permission });
    }
  }
  assert.equal(routes.length, 14, 'invocation installed operation count drift');
  for (const item of Object.values(invocation.paths)) {
    assert.ok(Object.keys(item).every(method => ['get', 'post', 'put', 'patch', 'delete'].includes(method)), 'invocation operation grammar drift');
  }
  const ops = operations(invocation);
  assert.deepEqual(sorted(ops.map(({ method, path, op }) => ({ service: op['x-service'], method, path }))), sorted(routes.map(({ service, method, path }) => ({ service, method, path }))), 'invocation path/service drift');
  const keyed = new Set(['chatCompletions', 'getOwnedUsageByIdempotencyKey', 'cancelUsageRequest']);
  for (const { method, path: url, op } of ops) {
    const route = routes.find(item => item.service === op['x-service'] && item.method === method && item.path === url);
    assert.equal(op['x-go-handler'], route.handler, 'invocation handler drift');
    assert.equal(op['x-permission'], route.permission, 'invocation permission drift');
    const security = route.permission === 'PUBLIC' ? [] : [{ [route.permission.startsWith('internal.') ? 'WorkloadBearer' : 'UserBearer']: [] }];
    assert.deepEqual(op.security, security, 'invocation authentication drift');
    const paths = [...url.matchAll(/\{(\w+)\}/g)].map(match => match[1]).sort();
    assert.deepEqual((op.parameters ?? []).filter(item => item.in === 'path' && item.required).map(item => item.name).sort(), paths, 'invocation path identity drift');
    if (keyed.has(route.handler)) {
      const key = op.parameters?.find(item => item.in === 'header' && item.name === 'Idempotency-Key');
      assert.ok(key?.required && key.schema.type === 'string' && key.schema.minLength === 16 && key.schema.maxLength === 128, 'invocation idempotency key drift');
    }
  }
  const body = url => {
    const request = invocation.paths[url].post.requestBody;
    assert.equal(request?.required, true, 'invocation body required drift');
    return request.content['application/json'].schema;
  };
  const chat = body('/v1/chat/completions');
  assert.deepEqual(chat.required, ['model'], 'invocation model input drift');
  assert.deepEqual(chat.properties.model, { type: 'string', minLength: 1 }, 'invocation model type drift');
  const extension = body('/internal/v1/holds/extend');
  assert.deepEqual(extension.required, ['usage_request_id', 'hold_id'], 'invocation extension identity drift');
  assert.equal(extension.additionalProperties, false, 'invocation extension unknown field drift');
  assert.deepEqual(Object.keys(extension.properties).sort(), ['additional_microcredits', 'hold_id', 'lease_seconds', 'operation_id', 'usage_request_id'], 'invocation extension field drift');
  const nil = '00000000-0000-0000-0000-000000000000';
  assert.deepEqual(extension.oneOf, [
    { required: ['additional_microcredits', 'operation_id'], properties: { additional_microcredits: { type: 'integer', format: 'int64', minimum: 1 }, operation_id: { type: 'string', format: 'uuid', not: { enum: [nil] } } } },
    { properties: { additional_microcredits: { type: 'integer', format: 'int64', enum: [0] }, operation_id: { type: ['string', 'null'], format: 'uuid', enum: [null, nil] } } },
  ], 'invocation extension operation identity drift');
  assert.ok(containsCode(actualFunction(read('billing/personal_execution_holds_http.go'), 'extendHold'), '(body.AdditionalMicrocredits>0)==(body.OperationID==uuid.Nil)'), 'invocation extension handler operation validation drift');
  assert.deepEqual(body('/internal/v1/holds/commit').required, ['usage_request_id', 'hold_id', 'usage_snapshot'], 'invocation settlement input drift');
  assert.deepEqual(body('/internal/v1/client-readiness/attestations').required, ['artifact_sha256', 'client_version', 'environment'], 'invocation attestation input drift');
  validateRefs(invocation);
  return routes.length;
}

function checkUsageWaiverContract(document, source) {
  const op = document.paths['/internal/v1/usage-waivers']?.post;
  assert.ok(op, 'usage waiver operation missing');
  assert.equal(op.requestBody?.required, true, 'usage waiver body required');
  assert.equal(op.requestBody?.content?.['application/json']?.schema?.$ref, '#/components/schemas/UsageWaiverCommand', 'usage waiver command DTO drift');
  assert.equal(op.responses?.['200']?.content?.['application/json']?.schema?.$ref, '#/components/schemas/UsageWaiverDecision', 'usage waiver receipt DTO drift');
  assert.deepEqual(Object.keys(op.responses).sort(), ['200', '400', '401', '403', '422', '503', 'default'], 'usage waiver status contract drift');
  assert.deepEqual(op['x-idempotency'], { transport: 'body', identity: 'command_reference', fingerprint: 'request_digest', matching_replay: 'persisted_original_decision', conflicting_reference_status: 403, unknown_result_status: 503 }, 'usage waiver original identity drift');
  assert.ok(!(op.parameters || []).some(p => p.in === 'header' && p.name.toLowerCase() === 'idempotency-key'), 'usage waiver cannot require a new key');
  for (const name of ['UsageWaiverCommand', 'UsageWaiverDecision', 'UsageWaiverResult']) {
    assert.equal(document.components.schemas[name]?.additionalProperties, false, 'usage waiver unknown fields drift');
  }
  const schemas = document.components.schemas;
  assert.deepEqual(schemas.UsageWaiverDecision.allOf, [{"if": {"properties": {"admission": {"const": "ADMITTED"}}}, "then": {"required": ["result"], "properties": {"reason_code": {"const": ""}}}}, {"if": {"properties": {"admission": {"const": "REJECTED"}}}, "then": {"not": {"required": ["result"]}, "properties": {"reason_code": {"enum": ["EXECUTION_ACTIVE", "USAGE_IDENTITY_MISMATCH", "DEADLINE_RELEASE_OWNS_RECOVERY", "ANOTHER_WAIVER_INTENT", "USAGE_NOT_ELIGIBLE", "USAGE_NOT_FOUND"]}}}}], 'usage waiver admission receipt drift');
  assert.deepEqual(schemas.UsageWaiverResult.allOf, [{"if": {"properties": {"outcome": {"const": "WAIVED"}}}, "then": {"required": ["actual_microcredits"], "properties": {"hold_status": {"const": "RELEASED"}, "actual_microcredits": {"const": "0"}}}}, {"if": {"properties": {"outcome": {"const": "COMMITTED"}}}, "then": {"required": ["actual_microcredits"], "properties": {"hold_status": {"const": "COMMITTED"}}}}, {"if": {"properties": {"outcome": {"const": "RELEASED_OTHER"}}}, "then": {"properties": {"hold_status": {"const": "RELEASED"}, "actual_microcredits": {"const": "0"}}}}], 'usage waiver financial outcome drift');
  for (const [name, field] of [['UsageWaiverCommand', 'maximum_microcredits'], ['UsageWaiverCommand', 'authorization_version'], ['UsageWaiverResult', 'maximum_microcredits'], ['UsageWaiverResult', 'actual_microcredits']]) assert.equal(schemas[name]?.properties?.[field]?.type, 'string', 'usage waiver int64 wire drift');
  assert.deepEqual(schemas.UsageWaiverResult.properties.outcome.enum, ['WAIVED', 'COMMITTED', 'RELEASED_OTHER'], 'usage waiver outcome drift');
  const code = goSource(source).code;
  assert.match(code, /var command usagewaiver\.Command/, 'usage waiver actual body binding missing');
  assert.match(code, /api\.DecodeJSON\(w, r, &command\)/, 'usage waiver actual decoder missing');
  assert.match(code, /if !command\.Valid\(\)/, 'usage waiver actual command validation missing');
  for (const call of ['store.PrepareUsageWaiver(r.Context(), command)', 'writer.ExecuteUsageWaiver(r.Context(), command)', 'store.FinishUsageWaiver(r.Context(), command, result)']) assert.ok(code.includes(call), 'usage waiver actual original-command chain missing');
}

export function checkCoverage({ routeSource, specSource, handlerSource, credentialSource, dtoSources, manifest, management, credential, organizationFinanceSource, organizationHandlerSource, organizationFinance, organizationAccountSource, memberUnits, invocation, invocationSources, usageWaiverSource }) {
  checkInvocationCoverage({ invocation, invocationSources });
  checkMemberUnitsContract(memberUnits, organizationAccountSource);
  checkUsageWaiverContract(management, usageWaiverSource);
  const routes = managementRoutes(routeSource);
  const organization = organizationFinanceRoutes(organizationFinanceSource, organizationHandlerSource);
  assert.deepEqual(sorted(manifest.organizationFinanceRoutes), organization, 'organization route authority coverage drift');
  const organizationOps = operations(organizationFinance);
  assert.deepEqual(organizationOps.map(({ op, ...entry }) => entry), organization.map(({ method, path }) => ({ method, path })), 'organization OpenAPI path drift');
  for (const { method, path: url, op } of organizationOps) {
    const route = organization.find(item => item.method === method && item.path === url);
    assert.deepEqual(op['x-authority'], route.authority, 'organization permission drift');
    assert.deepEqual(op['x-governance-gates'], route.gates, 'organization gate drift');
    assert.equal(op['x-go-handler'], route.handler, 'organization handler drift');
    assert.deepEqual(op['x-approval-policy'], route.policy, 'organization approval policy drift');
    assert.deepEqual(op.security, [{ UserBearer: [] }], 'organization authentication drift');
    assert.deepEqual((op.parameters || []).filter(p => p.in === 'path').map(p => p.name).sort(), [...url.matchAll(/\{(\w+)\}/g)].map(m => m[1]).sort(), 'organization path parameter drift');
    for (const parameter of op.parameters.filter(p => p.in === 'path')) assert.equal(parameter.required, true, 'organization path required');
    if (method !== 'get') assert.equal(op.parameters.find(p => p.in === 'header' && p.name === 'Idempotency-Key')?.required, true, 'organization key missing');
    if (method !== 'get') {
      const handlerBody = organizationHandlerSource.match(new RegExp('func \\(s \\*Service\\) ' + route.handler + '\\([^\\n]*\\) \\{([\\s\\S]*?)\\n\\}'))?.[1];
      const requestFields = [...(handlerBody?.match(/var body struct \{([\s\S]*?)\n\t\}/)?.[1] || '').matchAll(/`json:"([^",]+)(?:,[^"]*)?"`/g)].map(match => match[1]);
      const schemaName = op.requestBody?.content?.['application/json']?.schema?.$ref?.split('/').at(-1);
      assert.ok(requestFields.length && op.requestBody.required === true, 'organization request body missing');
      assert.deepEqual(Object.keys(organizationFinance.components.schemas[schemaName]?.properties || {}).sort(), requestFields.sort(), 'organization request fields drift');
    }

  }
  validateRefs(organizationFinance);
  assert.deepEqual(organizationFinance.components.schemas.QuotaItem?.properties?.component_type?.enum, ['BASE', 'BONUS'], 'organization quota component contract drift');
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
  return { managementRoutes: routes.length, credentialRoutes: isolated.length, commands: commands.length, organizationFinanceRoutes: organization.length };
}

export function loadInputs(base = root) {
  const read = (relative) => fs.readFileSync(path.join(base, relative), 'utf8');
  const json = (relative) => JSON.parse(read(relative));
  return {
    routeSource: read('account-platform/internal/token/management_routes.go'),
    organizationFinanceSource: read('account-platform/internal/token/organization_finance_routes.go'),
    organizationHandlerSource: read('account-platform/internal/token/organization_billing.go') + '\n' + read('account-platform/internal/token/organization_model_policy.go') + '\n' + read('account-platform/internal/token/account_closed_grants.go'),
    organizationFinance: json('account-platform/contracts/openapi/organization-finance.yaml'),
    organizationAccountSource: read('account-platform/internal/account/organization_routes.go'),
    memberUnits: json('account-platform/contracts/openapi/organization-member-units.yaml'),
    specSource: read('account-platform/internal/token/command_governance.go'),
    handlerSource: read('account-platform/internal/token/governance_handlers.go'),
    credentialSource: read('account-platform/internal/token/credential_origin.go'),
    usageWaiverSource: read('account-platform/internal/token/usage_waiver.go'),
    dtoSources: { ...Object.fromEntries(['command_governance.go', 'command_precheck.go', 'credential_isolation.go', 'lifecycle_plans.go', 'impact_precheck.go', 'account_provisioning_saga.go', 'approval_recovery_result.go', 'organization_lifecycle.go', 'organization_unit_blockers.go'].map((name) => ['internal/token/' + name, read('account-platform/internal/token/' + name)])), 'internal/platform/usagewaiver/contract.go': read('account-platform/internal/platform/usagewaiver/contract.go') },
    manifest: json('account-platform/deploy/authorization-token-management-entrypoints.json'),
    management: json('account-platform/contracts/openapi/token-management.yaml'),
    credential: json('account-platform/contracts/openapi/token-credential-origin.yaml'),
    invocation: json('account-platform/contracts/openapi/token-invocation.yaml'),
    invocationSources: Object.fromEntries(['token/personal_execution_routes.go', 'token/service.go', 'token/personal_catalog_http.go', 'token/personal_execution_http.go', 'token/proxy.go', 'token/client_readiness.go', 'billing/service.go', 'billing/personal_execution_holds_http.go', 'account/service.go'].map(name => [name, read('account-platform/internal/' + name)])),
  };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { console.log(JSON.stringify({ status: 'PASS', ...checkCoverage(loadInputs()) })); }
  catch { console.error('Token management authorization contract coverage failed'); process.exitCode = 1; }
}
