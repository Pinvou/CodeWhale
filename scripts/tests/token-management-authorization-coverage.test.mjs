import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import { readFileSync } from 'node:fs';
import { checkCoverage, loadInputs } from '../check-token-management-authorization-coverage.mjs';

const inputs = loadInputs();
const closedGrantAuthority = 'resolved, org, _, ok := s.resolveOrganizationBilling(w, r, "org.grants.reclaim_closed")';
const changedClosedGrantAuthority = 'resolved, org, _, ok := s.resolveOrganizationBilling(w, r, "org.grants.allocate")';
assert.equal(inputs.organizationHandlerSource.split(closedGrantAuthority).length - 1, 1);
const anchors = {
  routeSource: inputs.routeSource.split(/\r?\n/).find(line => /^\s*internal\.POST\("\/holds\/expiry-proof"/.test(line)),
  credentialSource: inputs.credentialSource.split(/\r?\n/).find(line => /^\s*router\.GET\("\/assets\/credential\.css"/.test(line)),
};
for (const anchor of Object.values(anchors)) assert.ok(anchor);
const declarations = {
  routeSource: { receiver: 'internal', args: '"/coverage-unlisted", serviceauth.Require("internal.token.hold-expiry.prove"), httpserver.AdaptFunc(s.expiredHoldProof)' },
  credentialSource: { receiver: 'router', args: '"/coverage-unlisted", httpserver.AdaptFunc(s.stylesheet)' },
};
function insert(field, extra) {
  // Add within the real receiver's scope, preserving all original declarations,
  // manifest, OpenAPI, policies and DTO inputs. This does not execute Go routes.
  const anchor = anchors[field];
  const indent = /^\s*/.exec(anchor)[0];
  return { ...inputs, [field]: inputs[field].replace(anchor, `${anchor}\n${extra.map(line => indent + line).join('\n')}`) };
}

test('actual complete F04 inputs retain exact declared routes and policies', () => {
  assert.deepEqual(checkCoverage(inputs), {
    managementRoutes: inputs.manifest.routes.length,
    credentialRoutes: inputs.manifest.credentialRoutes.length,
    commands: inputs.manifest.commands.length,
    organizationFinanceRoutes: inputs.manifest.organizationFinanceRoutes.length,
  });
});

const unsupported = {
  HEAD: ({ receiver, args }) => [`${receiver}.HEAD(${args})`],
  OPTIONS: ({ receiver, args }) => [`${receiver}.OPTIONS(${args})`],
  Handle: ({ receiver, args }) => [`${receiver}.Handle("GET", ${args})`],
  Any: ({ receiver, args }) => [`${receiver}.Any(${args})`],
  Match: ({ receiver, args }) => [`${receiver}.Match([]string{"GET", "HEAD"}, ${args})`],
  Static: ({ receiver }) => [`${receiver}.Static("/coverage-unlisted", ".")`],
  StaticFS: ({ receiver }) => [`${receiver}.StaticFS("/coverage-unlisted", nil)`],
  StaticFile: ({ receiver }) => [`${receiver}.StaticFile("/coverage-unlisted", "fixture.json")`],
  StaticFileFS: ({ receiver }) => [`${receiver}.StaticFileFS("/coverage-unlisted", "fixture.json", nil)`],
  assignment: ({ receiver, args }) => [`_ = ${receiver}.GET(${args})`],
  conditional: ({ receiver, args }) => [`if true { ${receiver}.GET(${args}) }`],
  selector: ({ receiver, args }) => [`${receiver}.${receiver === 'router' ? 'Engine' : 'RouterGroup'}.GET(${args})`],
  methodValue: ({ receiver, args }) => [`coverageRegister := ${receiver}.GET`, `coverageRegister(${args})`],
  splitSelector: ({ receiver, args }) => [`${receiver}.`, `GET(${args})`],
  unicodeOffset: ({ receiver, args }) => ['_ = "\u{10000}"', `${receiver}.HEAD(${args})`],
  sameLineMultiple: ({ receiver, args }) => [`if true { ${receiver}.GET(${args}); ${receiver}.HEAD(${args}) }`],
};
for (const field of Object.keys(declarations)) {
  for (const [name, declaration] of Object.entries(unsupported)) {
    test(`${field} refuses an unclassified ${name} declaration without changing expected inventory`, () => {
      assert.throws(() => checkCoverage(insert(field, declaration(declarations[field]))), /unrecognized .*route/);
    });
  }
  test(`${field} preserves existing multiline declaration rejection`, () => {
    const { receiver, args } = declarations[field];
    assert.throws(() => checkCoverage(insert(field, [`${receiver}.GET(`, `${args})`])), /unrecognized .*route/);
  });
  test(`${field} preserves existing nonliteral path rejection`, () => {
    const { receiver, args } = declarations[field];
    assert.throws(() => checkCoverage(insert(field, ['coveragePath := "/coverage-unlisted"', `${receiver}.GET(${args.replace('"/coverage-unlisted"', 'coveragePath')})`])), /unrecognized .*route/);
  });
  test(`${field} ignores method-looking line/block comments and quoted/raw strings`, () => {
    const { receiver, args } = declarations[field];
    assert.deepEqual(checkCoverage(insert(field, [
      `// ${receiver}.HEAD(${args})`,
      '/*', `${receiver}.GET(${args})`, '*/',
      `_ = "${receiver}.GET(\\\"/not-a-route\\\")"`,
      '_ = `', `${receiver}.GET(${args})`, '`',
      "_ = '\"'",
    ])), checkCoverage(inputs));
  });
}

test('real CRLF route sources preserve declaration offsets and complete inventory', () => {
  assert.deepEqual(checkCoverage({
    ...inputs,
    routeSource: inputs.routeSource.replace(/\r?\n/g, '\r\n'),
    credentialSource: inputs.credentialSource.replace(/\r?\n/g, '\r\n'),
  }), checkCoverage(inputs));
});

const invocationDocument = JSON.parse(readFileSync(new URL('../../account-platform/contracts/openapi/token-invocation.yaml', import.meta.url), 'utf8'));
for (const handler of ['getOwnedUsageByIdempotencyKey', 'cancelUsageRequest']) {
  for (const [name, replacement] of Object.entries({
    currentContextResolver: 's.resolveModelUsagePermission(w, r, "personal.usage.cancel")',
    removedPersonalResolver: 's.authorizePersonalUsageUser(w, r, "personal.usage.cancel")',
    wrongPermission: 's.authorizeOriginalUsageUser(w, r, "personal.usage.read")',
  })) {
    test(`original-owner ${handler} rejects ${name} authority drift`, () => {
      const file = 'token/personal_execution_http.go';
      const original = inputs.invocationSources[file];
      const start = original.indexOf(`func (s *Service) ${handler}(`);
      assert.ok(start >= 0);
      const next = original.indexOf('\nfunc ', start + 1);
      const end = next < 0 ? original.length : next;
      const body = original.slice(start, end);
      const authority = 's.authorizeOriginalUsageUser(w, r, "personal.usage.cancel")';
      assert.equal(body.split(authority).length - 1, 1);
      const source = original.slice(0, start) + body.replace(authority, replacement) + original.slice(end);
      assert.throws(() => checkCoverage({ ...inputs, invocationSources: { ...inputs.invocationSources, [file]: source } }), /invocation handler authority/);
    });
  }
}

for (const [name, mutate] of Object.entries({
  missing: op => { op.parameters = op.parameters.filter(item => item.name !== 'Idempotency-Key'); },
  optional: op => { op.parameters.find(item => item.name === 'Idempotency-Key').required = false; },
  nonString: op => { op.parameters.find(item => item.name === 'Idempotency-Key').schema.type = 'number'; },
  shortMinimum: op => { op.parameters.find(item => item.name === 'Idempotency-Key').schema.minLength = 15; },
  longMaximum: op => { op.parameters.find(item => item.name === 'Idempotency-Key').schema.maxLength = 129; },
})) {
  test(`original-owner lookup rejects ${name} original idempotency key contract`, () => {
    const invocation = structuredClone(invocationDocument);
    mutate(invocation.paths['/v1/usage-requests/by-idempotency-key'].get);
    assert.throws(() => checkCoverage({ ...inputs, invocation }), /invocation idempotency key drift/);
  });
}

for (const [name, mutate] of Object.entries({
  missingRoute: document => { delete document.paths['/v1/models']; },
  extraRoute: document => { document.paths['/v1/uninstalled'] = document.paths['/v1/models']; },
  wrongHandler: document => { document.paths['/v1/models'].get['x-go-handler'] = 'uninstalled'; },
  wrongPermission: document => { document.paths['/v1/models'].get['x-permission'] = 'platform.models.manage'; },
  missingAuthentication: document => { document.paths['/v1/models'].get.security = []; },
  missingIdempotency: document => { document.paths['/v1/chat/completions'].post.parameters[0].required = false; },
  missingPathIdentity: document => { document.paths['/v1/usage-requests/{usage_request_id}'].get.parameters = []; },
  missingModel: document => { document.paths['/v1/chat/completions'].post.requestBody.content['application/json'].schema.required = []; },
  missingPositiveOperation: document => { document.paths['/internal/v1/holds/extend'].post.requestBody.content['application/json'].schema.oneOf[0].required = ['additional_microcredits']; },
  unrestrictedRenewalOperation: document => { delete document.paths['/internal/v1/holds/extend'].post.requestBody.content['application/json'].schema.oneOf[1].properties.operation_id.enum; },
})) {
  test(`finite invocation contract rejects ${name} without changing installed routes`, () => {
    const invocation = structuredClone(invocationDocument); mutate(invocation);
    assert.throws(() => checkCoverage({ ...inputs, invocation }), /invocation/);
  });
}

test('finite invocation checks actual route, authentication and handler authority sources', () => {
  for (const [file, from, to] of [
    ['token/personal_execution_routes.go', 'protected.GET("/models",', 'protected.GET("/uninstalled-models",'],
    ['token/personal_execution_routes.go', 'protected.POST("/chat/completions",', 'protected.POST(runtimePath,'],
    ['token/personal_execution_routes.go', 'internal.token.client-readiness.read', 'internal.token.unclassified'],
    ['token/service.go', 'protected.Use(identity.GinMiddleware(verify))', 'protected.Use(otherMiddleware)'],
    ['token/personal_catalog_http.go', '"personal.models.read"', '"platform.models.manage"'],
    ['billing/service.go', 'serviceauth.GinMiddleware(serviceToken)', 'otherMiddleware'],
    ['billing/personal_execution_holds_http.go', '(body.AdditionalMicrocredits > 0) == (body.OperationID == uuid.Nil)', 'false'],
  ]) {
    assert.ok(inputs.invocationSources[file].includes(from), from);
    assert.throws(() => checkCoverage({ ...inputs, invocationSources: { ...inputs.invocationSources, [file]: inputs.invocationSources[file].replace(from, to) } }), /invocation|unrecognized_http_entrypoint/);
  }
});
test('invocation handler comments and strings cannot substitute for real authority', () => {
  const name = 'token/personal_catalog_http.go';
  const removed = inputs.invocationSources[name].replace('s.resolveModelUsagePermission(w, r, "personal.models.read", "org.models.read")', 'unclassified()');
  const source = removed.replace('func (s *Service) listCatalogModels(w http.ResponseWriter, r *http.Request) {', `func (s *Service) listCatalogModels(w http.ResponseWriter, r *http.Request) {
 // s.resolveModelUsagePermission(w, r, "personal.models.read", "org.models.read")
 _ = \`s.resolveModelUsagePermission(w, r, "personal.models.read", "org.models.read")\``);
  assert.throws(() => checkCoverage({ ...inputs, invocationSources: { ...inputs.invocationSources, [name]: source } }), /invocation/);
});
test('actual invocation inputs support comment examples and CRLF without new operations', () => {
  const invocationSources = Object.fromEntries(Object.entries(inputs.invocationSources).map(([name, value]) => [name, ('// protected.GET(runtimePath, handler)\n' + value).replace(/\r?\n/g, '\r\n')]));
  assert.deepEqual(checkCoverage({ ...inputs, invocationSources }), checkCoverage(inputs));
});

test('invocation authentication, group and hold checks reject string or comment substitutes', () => {
  for (const [file, actual] of [
    ['token/service.go', 'protected := router.Group("/v1")'],
    ['token/service.go', 'protected.Use(identity.GinMiddleware(verify))'],
    ['token/personal_execution_routes.go', 'internal.Use(serviceauth.GinMiddleware(s.managementServiceToken))'],
    ['billing/service.go', 'protected.Use(serviceauth.GinMiddleware(serviceToken))'],
    ['account/service.go', 'internal.Use(serviceauth.GinMiddleware(s.serviceToken))'],
    ['billing/personal_execution_holds_http.go', '(body.AdditionalMicrocredits > 0) == (body.OperationID == uuid.Nil)'],
  ]) {
    const original = inputs.invocationSources[file];
    assert.ok(original.includes(actual));
    for (const example of ['var example = ' + JSON.stringify(actual), 'var example = `'+ actual +'`', '// ' + actual, '/* ' + actual + ' */']) {
      const source = original.replace(actual, 'unclassified()') + '\n' + example + '\n';
      assert.throws(() => checkCoverage({ ...inputs, invocationSources: { ...inputs.invocationSources, [file]: source } }), /invocation/, file + ': ' + example);
    }
  }
});

test('a raw-string function declaration cannot replace actual invocation handler authority', () => {
  const file = 'token/personal_catalog_http.go';
  const actual = inputs.invocationSources[file].replace('"personal.models.read"', '"platform.models.manage"');
  const fake = 'var example = `func (s *Service) listCatalogModels(w http.ResponseWriter, r *http.Request) {\n s.resolveModelUsagePermission(w, r, "personal.models.read", "org.models.read")\n}\n`\n';
  assert.throws(() => checkCoverage({ ...inputs, invocationSources: { ...inputs.invocationSources, [file]: actual.replace('func (s *Service) listCatalogModels(', fake + 'func (s *Service) listCatalogModels(') } }), /invocation/);
});

test('unused Go functions cannot replace installed invocation authentication or hold validation', () => {
  for (const [file, actual, declaration, statement] of [
    ['token/service.go', 'protected := router.Group("/v1")', 'func unused(router *httpserver.Router)', 'protected := router.Group("/v1"); _ = protected'],
    ['token/service.go', 'protected.Use(identity.GinMiddleware(verify))', 'func unused(protected *httpserver.RouteGroup, verify identity.VerifyFunc)', 'protected.Use(identity.GinMiddleware(verify))'],
    ['token/personal_execution_routes.go', 'internal.Use(serviceauth.GinMiddleware(s.managementServiceToken))', 'func unused(s *Service, internal *httpserver.RouteGroup)', 'internal.Use(serviceauth.GinMiddleware(s.managementServiceToken))'],
    ['billing/service.go', 'protected.Use(serviceauth.GinMiddleware(serviceToken))', 'func unused(protected *httpserver.RouteGroup, serviceToken string)', 'protected.Use(serviceauth.GinMiddleware(serviceToken))'],
    ['account/service.go', 'internal.Use(serviceauth.GinMiddleware(s.serviceToken))', 'func unused(s *Service, internal *httpserver.RouteGroup)', 'internal.Use(serviceauth.GinMiddleware(s.serviceToken))'],
    ['billing/personal_execution_holds_http.go', '(body.AdditionalMicrocredits > 0) == (body.OperationID == uuid.Nil)', 'func unused(body struct { AdditionalMicrocredits int64; OperationID uuid.UUID })', '_ = (body.AdditionalMicrocredits > 0) == (body.OperationID == uuid.Nil)'],
  ]) {
    const source = inputs.invocationSources[file].replace(actual, 'unclassified()') + '\n' + declaration + ' {\n' + statement + '\n}\n';
    assert.throws(() => checkCoverage({ ...inputs, invocationSources: { ...inputs.invocationSources, [file]: source } }), /invocation/, file);
  }
});
test('the installed Token Handler must still register the invocation routes', () => {
  const file = 'token/service.go'; const actual = 's.registerPersonalExecutionRoutes(router, protected)';
  assert.ok(inputs.invocationSources[file].includes(actual));
  for (const example of ['var example = `'+actual+'`', 'func unused(s *Service, router *httpserver.Router, protected *httpserver.RouteGroup) { '+actual+' }']) {
    const source = inputs.invocationSources[file].replace(actual, '') + '\n' + example + '\n';
    assert.throws(() => checkCoverage({ ...inputs, invocationSources: { ...inputs.invocationSources, [file]: source } }), /invocation/);
  }
});

test('organization finance registration cannot disappear outside the management inventory', () => {
  const result = checkCoverage(inputs);
  assert.equal(result.organizationFinanceRoutes, 13);
});

test('closed-grant authority helper cannot disappear or change permission', () => {
  for (const source of [
    inputs.organizationHandlerSource.replaceAll('s.closedGrantClient(w, r)', 's.unclassifiedClosedGrantClient(w, r)'),
    inputs.organizationHandlerSource.replace('func (s *Service) closedGrantClient(', 'func (s *Service) unclassifiedClosedGrantClient('),
    inputs.organizationHandlerSource.replace(closedGrantAuthority, changedClosedGrantAuthority),
  ]) assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), /closed grant authority/);
});

test('comments and strings cannot stand in for the closed-grant authority chain', () => {
  const caller = 'resolved, org, client, ok := s.closedGrantClient(w, r)';
  const authority = 'resolved, org, _, ok := s.resolveOrganizationBilling(w, r, "org.grants.reclaim_closed")';
  for (const [original, replacement, selector, error] of [
    [caller, 'resolved, org, client, ok := s.unclassifiedClosedGrantClient(w, r)', 's.closedGrantClient(w, r)', /closed grant authority helper/],
    [authority, 'resolved, org, _, ok := s.resolveOrganizationBilling(w, r, "org.grants.allocate")', 's.resolveOrganizationBilling(w, r, "org.grants.reclaim_closed")', /closed grant authority changed/],
  ]) {
    for (const comment of [`// ${selector}`, `/* ${original} */`, `_ = \`${original}\``, `_ = \`\n${original}\n\``]) {
      const source = inputs.organizationHandlerSource.replaceAll(original, `${replacement}\n\t${comment}`);
      assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), error);
    }
  }
});

test('a commented or stringified whole helper cannot hide changed real authority', () => {
  const helper = inputs.organizationHandlerSource.match(/func \(s \*Service\) closedGrantClient\([^\n]*\) [^\n]*\{[\s\S]*?\n\}/)?.[0];
  assert.ok(helper);
  const changed = inputs.organizationHandlerSource.replace(closedGrantAuthority, changedClosedGrantAuthority);
  for (const standIn of [`/*\n${helper}\n*/\n`, `var fakeClosedGrantHelper = \`\n${helper}\n\`\n`]) {
    assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: standIn + changed }), /closed grant authority changed/);
  }
});

test('both closed-grant paths must be present in the manifest and contract', () => {
  const paths = [
    '/v1/organizations/{organization_id}/account-closed-grants/{grant_id}/reclaim',
    '/v1/organizations/{organization_id}/account-closed-grants:reclaim-batch',
  ];
  for (const path of paths) {
    const manifest = structuredClone(inputs.manifest);
    manifest.organizationFinanceRoutes = manifest.organizationFinanceRoutes.filter(route => route.path !== path);
    assert.throws(() => checkCoverage({ ...inputs, manifest }), /organization route authority/);
    const organizationFinance = structuredClone(inputs.organizationFinance);
    delete organizationFinance.paths[path];
    assert.throws(() => checkCoverage({ ...inputs, organizationFinance }), /organization OpenAPI/);
  }
});

test('closed-grant routes cannot bypass their declared management gate', () => {
  for (const handler of ['reclaimClosedGrant', 'reclaimClosedGrantsBatch']) {
    const line = inputs.organizationFinanceSource.split(/\r?\n/).find(value => value.includes(`httpserver.AdaptFunc(s.${handler})`));
    assert.ok(line);
    const source = inputs.organizationFinanceSource.replace(line, '')
      .replace('if !s.managementEnabled {', `${line.trim()}\n\tif !s.managementEnabled {`);
    assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: source }), /precedes management assembly gate/);
  }
});

test('organization finance path and authority drift are rejected against frozen manifest', () => {
  assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: fs.readFileSync(new URL('../../account-platform/internal/token/organization_finance_routes.go', import.meta.url), 'utf8').replace('/balance', '/balance-unclassified') }), /organization/);
});

for (const [name, change] of Object.entries({
 omitted: value => value.replace(/.*routes.GET\("\/organizations\/:organization_id\/balance".*\r?\n/, ''),
 method: value => value.replace('routes.GET("/organizations/:organization_id/balance"', 'routes.POST("/organizations/:organization_id/balance"'),
 gate: value => value.replace('!s.managementEnabled', 's.managementEnabled'),
 conditional: value => value.replace('routes.GET("/organizations/:organization_id/balance",', 'if true { routes.GET("/organizations/:organization_id/balance",'),
 methodValue: value => value + '\n register := routes.GET\n',
})) test(`organization finance rejects ${name} registration drift`, () => {
 assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: change(inputs.organizationFinanceSource) }));
});
test('organization finance verifies permission inside the installed handler, not only declarations', () => {
 assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: inputs.organizationHandlerSource.replace('resolveOrganizationBilling(w, r, "org.wallet.read")', 'resolveOrganizationBilling(w, r, "org.units.read")') }), /organization/);
});
for (const part of ['path', 'authority', 'key', 'approval']) test(`organization finance OpenAPI ${part} drift fails closed`, () => {
 const contract = structuredClone(inputs.organizationFinance), op = contract.paths['/v1/organizations/{organization_id}/model-policy'].put;
 if (part === 'path') delete contract.paths['/v1/organizations/{organization_id}/balance'];
 if (part === 'authority') op['x-authority'].all = ['org.members.manage'];
 if (part === 'key') op.parameters = op.parameters.filter(p => p.name !== 'Idempotency-Key');
 if (part === 'approval') delete op['x-approval-policy'];
 assert.throws(() => checkCoverage({ ...inputs, organizationFinance: contract }), /organization/);
});

test('organization request properties match the actual handler body decoder', () => {
 const contract = structuredClone(inputs.organizationFinance);
 delete contract.components.schemas.ModelPolicyWrite.properties.expected_version;
 assert.throws(() => checkCoverage({ ...inputs, organizationFinance: contract }), /organization request fields/);
});

test('a commented-out organization assembly gate does not satisfy the checker', () => {
 const source = inputs.organizationFinanceSource.replace('if !s.managementEnabled {', '/* if !s.managementEnabled { return } */ if s.managementEnabled {');
 assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: source }), /organization management/);
});


test('organization quota contract rejects unsupported PLAN or missing concrete components', () => {
 for(const values of [['BASE','BONUS','PLAN'],['BASE'],['BONUS'],[]]) {
  const organizationFinance=structuredClone(inputs.organizationFinance);
  organizationFinance.components.schemas.QuotaItem.properties.component_type.enum=values;
  assert.throws(()=>checkCoverage({...inputs,organizationFinance}), /organization quota component/);
 }
});


test('member unit contract cannot omit or misstate the actual Admin AuditOnly command', () => {
  const doc = JSON.parse(fs.readFileSync(new URL('../../account-platform/contracts/openapi/organization-member-units.yaml', import.meta.url), 'utf8'));
  const organizationAccountSource = fs.readFileSync(new URL('../../account-platform/internal/account/organization_routes.go', import.meta.url), 'utf8');
  const ordinary = '/v1/organizations/{organization_id}/members/{user_id}/units';
  const admin = '/admin/v1/organizations/{organization_id}/members/{user_id}/units';
  for (const mutate of [
    value => { delete value.paths[admin].put['x-approval-policy']; },
    value => { value.paths[admin].put['x-approval-policy'] = { command: 'ACCOUNT_MEMBER_ADD', mode: 'AuditOnly' }; },
    value => { value.paths[admin].put['x-approval-policy'] = { command: 'ACCOUNT_MEMBER_UNITS_REPLACE', mode: 'ApprovalRequired' }; },
    value => { value.paths[ordinary].put['x-approval-policy'] = { command: 'ACCOUNT_MEMBER_UNITS_REPLACE', mode: 'AuditOnly' }; },
    value => { value.paths[admin].put['x-go-handler'] = 'replaceMemberUnits'; },
    value => { value.paths[admin].put['x-permission'] = 'org.members.manage'; },
    value => { value.paths[ordinary].put.security = []; },
    value => { value.paths[ordinary].post = value.paths[ordinary].put; delete value.paths[ordinary].put; },
    value => { value.paths[admin].put.requestBody.content['application/json'].schema = { $ref: '#/components/schemas/Missing' }; },
  ]) {
    const memberUnits = structuredClone(doc); mutate(memberUnits);
    assert.throws(() => checkCoverage({ ...inputs, organizationAccountSource, memberUnits }), /member unit/);
  }
});


test('member unit source binding rejects drift without treating commented routes as production', () => {
  const line = inputs.organizationAccountSource.split(/\r?\n/).find(value => value.includes('admin.PUT("/organizations/:organization_id/members/:user_id/units"'));
  assert.ok(line);
  for (const replacement of [line.replace('ACCOUNT_MEMBER_UNITS_REPLACE', 'ACCOUNT_MEMBER_ADD'), line.replace('approvalpolicy.AuditOnly', 'approvalpolicy.ApprovalRequired'), line.replace('PermissionPlatformOrganizationsMembersManage', 'PermissionPlatformOrganizationsRead'), line.replace('adminReplaceMemberUnits', 'adminReplaceMemberRoles')]) {
    assert.throws(() => checkCoverage({ ...inputs, organizationAccountSource: inputs.organizationAccountSource.replace(line, replacement) }), /member unit/);
  }
  assert.doesNotThrow(() => checkCoverage({ ...inputs, organizationAccountSource: inputs.organizationAccountSource + '\n// ' + line.trim() + '\n' }));
});
