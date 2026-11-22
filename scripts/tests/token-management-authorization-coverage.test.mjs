import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import { readFileSync } from 'node:fs';
import { checkCoverage, loadInputs, organizationFinanceRoutes } from '../check-token-management-authorization-coverage.mjs';
import { authorizationRoutes } from '../check-account-platform-authorization-coverage.mjs';

const inputs = loadInputs();

test('actual usage waiver route retains body identity, exact workload permission and versioned DTOs', () => {
  assert.doesNotThrow(() => checkCoverage(inputs));
  const op = inputs.management.paths['/internal/v1/usage-waivers'].post;
  assert.deepEqual(op.security, [{ WorkloadBearer: [] }]);
  assert.deepEqual(op['x-authority'], { kind: 'workload', all: ['internal.token.usage.waive'] });
  assert.equal(op['x-idempotency'].conflicting_reference_status, 403);
  assert.equal(op['x-idempotency'].unknown_result_status, 503);
});

test('usage waiver inventory cannot omit the installed route or weaken its actual authority', () => {
  const missing = structuredClone(inputs.manifest);
  missing.routes = missing.routes.filter(route => route.path !== '/internal/v1/usage-waivers');
  assert.throws(() => checkCoverage({ ...inputs, manifest: missing }), /management authority/);
  for (const mutate of [
    document => { delete document.paths['/internal/v1/usage-waivers']; },
    document => { document.paths['/internal/v1/usage-waivers'].post.security = [{ UserBearer: [] }]; },
    document => { document.paths['/internal/v1/usage-waivers'].post['x-authority'].all = ['internal.token.approval-proof.read']; },
  ]) {
    const management = structuredClone(inputs.management); mutate(management);
    assert.throws(() => checkCoverage({ ...inputs, management }));
  }
});

test('usage waiver rejects DTO, original-reference and HTTP status contract drift', () => {
  for (const mutate of [
    document => { document.paths['/internal/v1/usage-waivers'].post.requestBody.required = false; },
    document => { document.paths['/internal/v1/usage-waivers'].post.responses['200'].content['application/json'].schema.$ref = '#/components/schemas/Error'; },
    document => { const op = document.paths['/internal/v1/usage-waivers'].post; op.responses['409'] = op.responses['403']; delete op.responses['403']; },
    document => { document.paths['/internal/v1/usage-waivers'].post['x-idempotency'].unknown_result_status = 200; },
    document => { document.paths['/internal/v1/usage-waivers'].post.parameters.push({ in: 'header', name: 'Idempotency-Key', required: true }); },
    document => { document.components.schemas.UsageWaiverCommand.properties.maximum_microcredits.type = 'number'; },
    document => { delete document.components.schemas.UsageWaiverCommand.properties.request_digest; },
    document => { document.components.schemas.UsageWaiverResult.properties.outcome.enum.push('UNKNOWN_SUCCESS'); },
    document => { document.components.schemas.UsageWaiverDecision.additionalProperties = true; },
    document => { document.components.schemas.UsageWaiverDecision.allOf = []; },
    document => { document.components.schemas.UsageWaiverResult.allOf = []; },
  ]) {
    const management = structuredClone(inputs.management); mutate(management);
    assert.throws(() => checkCoverage({ ...inputs, management }));
  }
});

test('usage waiver DTO and actual command chain source must remain bound, not only comments', () => {
  const dtoSources = { ...inputs.dtoSources };
  dtoSources['internal/platform/usagewaiver/contract.go'] = dtoSources['internal/platform/usagewaiver/contract.go'].replace('json:"request_digest"', 'json:"changed_digest"');
  assert.throws(() => checkCoverage({ ...inputs, dtoSources }), /DTO field coverage/);
  const call = 'writer.ExecuteUsageWaiver(r.Context(), command)';
  assert.ok(inputs.usageWaiverSource.includes(call));
  const usageWaiverSource = inputs.usageWaiverSource.replace(call, 'writer.OtherOperation(r.Context(), command)') + '\n// ' + call;
  assert.throws(() => checkCoverage({ ...inputs, usageWaiverSource }), /actual original-command chain/);
});
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
  assert.equal(result.organizationFinanceRoutes, 14);
});

test('organization finance recognizes the path guard without weakening its middleware grammar', () => {
  const route = 'routes.POST("/organizations/:organization_id/quota-grants:batch", httpserver.RequirePathSuffix("/quota-grants:batch"), httpserver.AdaptFunc(s.allocateOrganizationQuotaBatch))';
  const registrations = authorizationRoutes('organization finance', inputs.organizationFinanceSource)
    .filter(({ method, path }) => method === 'POST' && path === '/organizations/:organization_id/quota-grants:batch');
  assert.deepEqual(registrations.map(({ registration }) => registration.trim()), [route]);
  const legacy = route.replace('httpserver.RequirePathSuffix("/quota-grants:batch"), ', '');
  const legacySource = inputs.organizationFinanceSource.replace(route, legacy);
  assert.ok(legacySource.includes(legacy));
  assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: legacySource }), /organization action path guard/);
  const guardedSource = legacySource.replace(legacy, route);
  assert.equal(checkCoverage({ ...inputs, organizationFinanceSource: guardedSource }).organizationFinanceRoutes, 14);
  for (const replacement of [
    route.replace('httpserver.RequirePathSuffix("/quota-grants:batch")', 'httpserver.RequirePathSuffix("/quota-grants:batchOther")'),
    route.replace('httpserver.RequirePathSuffix("/quota-grants:batch"), ', 'httpserver.RequirePathSuffix("/quota-grants:batch"), httpserver.RequirePathSuffix("/quota-grants:batch"), '),
    route.replace('httpserver.RequirePathSuffix("/quota-grants:batch"), ', 'middleware.Require("quota"), '),
  ]) {
    const source = guardedSource.replace(route, replacement);
    assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: source }), /organization handler/);
  }
});

test('every installed organization action requires one exact first path guard', () => {
  for (const suffix of ['/quota-grants:batch', '/account-closed-grants:reclaim-batch']) {
    const guard = `httpserver.RequirePathSuffix("${suffix}"), `;
    const line = inputs.organizationFinanceSource.split(/\r?\n/).find(value => value.includes(guard));
    assert.ok(line, suffix);
    for (const changed of [
      line.replace(guard, ''),
      line.replace(guard, `httpserver.RequirePathSuffix("${suffix.slice(suffix.indexOf(':'))}"), `),
      line.replace(guard, `httpserver.RequirePathSuffix("/other${suffix}"), `),
      line.replace(guard, guard + guard),
      line.replace(guard, `approvalpolicy.Require("TOKEN_ORGANIZATION_MODEL_POLICY_UPDATE", approvalpolicy.AuditOnly), ${guard}`),
      line.replace(guard, '').replace(/\)$/, `, ${guard.slice(0, -2)})`),
    ]) {
      assert.notEqual(changed, line);
      assert.throws(() => checkCoverage({ ...inputs,
        organizationFinanceSource: inputs.organizationFinanceSource.replace(line, changed) }), /organization (?:action path guard|handler)/);
    }
  }
});

test('new literal organization actions obey the same guard rule without treating parameters as actions', () => {
  const original = inputs.organizationFinanceSource.split(/\r?\n/).find(line => line.includes('"/quota-grants:batch"'));
  assert.ok(original);
  const next = original.replaceAll('quota-grants:batch', 'quota-grants:preview');
  const source = inputs.organizationFinanceSource.replace(/\n\}\s*$/, '\n' + next + '\n}\n');
  assert.notEqual(source, inputs.organizationFinanceSource);
  const parsed = organizationFinanceRoutes(source, inputs.organizationHandlerSource);
  assert.equal(parsed.length, 15);
  assert.ok(parsed.some(route => route.path === '/v1/organizations/{organization_id}/quota-grants:preview'));
  assert.throws(() => organizationFinanceRoutes(source.replace('httpserver.RequirePathSuffix("/quota-grants:preview"), ', ''), inputs.organizationHandlerSource), /organization action path guard/);
  // Unsupported mixed parameter/action and non-final action shapes must demand
  // review rather than silently falling back to the ordinary parameter case.
  for (const path of ['/:grant_id:preview', '/quota-grants:preview/receipt', '/quota-grants:preview:again']) {
    const changed = source.replaceAll('/quota-grants:preview', path);
    assert.throws(() => organizationFinanceRoutes(changed, inputs.organizationHandlerSource), /unrecognized organization action path/);
  }
  assert.equal(organizationFinanceRoutes(inputs.organizationFinanceSource, inputs.organizationHandlerSource).length, 14);
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

// These use the installed source and production checker. String literals are
// input examples only; they must not become evidence of executable authority.
const goAuthorityStandIns = statement => {
  const inline = statement.replace(/\s+/g, ' ');
  return {
    lineComment: `// ${inline}`,
    blockComment: `/* ${statement} */`,
    rawString: `_ = \`\n${statement}\n\``,
    quotedString: `_ = ${JSON.stringify(inline)}`,
  };
};
const ordinaryQuotaAuthority = 's.resolveOrganizationBilling(w, r, "org.quota.allocate")';
const ordinaryQuotaStatement = 'resolved, organizationID, client, ok := ' + ordinaryQuotaAuthority;
for (const [name, standIn] of Object.entries(goAuthorityStandIns(ordinaryQuotaStatement))) {
  test(`ordinary organization authority cannot be supplied by ${name}`, () => {
    assert.ok(inputs.organizationHandlerSource.includes(ordinaryQuotaStatement));
    const source = inputs.organizationHandlerSource.replace(ordinaryQuotaStatement,
      `${standIn}\n\t${ordinaryQuotaStatement.replace('org.quota.allocate', 'org.wallet.read')}`);
    assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), /organization/);
  });
}
test('ordinary organization authority ignores examples before its real call and rejects two real resolvers', () => {
  const wrong = ordinaryQuotaStatement.replace('org.quota.allocate', 'org.wallet.read');
  const examples = Object.values(goAuthorityStandIns(wrong)).join('\n\t');
  const source = inputs.organizationHandlerSource.replace(ordinaryQuotaStatement, `${examples}\n\t${ordinaryQuotaStatement}`);
  assert.doesNotThrow(() => checkCoverage({ ...inputs, organizationHandlerSource: source }));
  const duplicate = inputs.organizationHandlerSource.replace(ordinaryQuotaStatement, `${ordinaryQuotaStatement}\n\t${ordinaryQuotaAuthority}`);
  assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: duplicate }), /organization permission source ambiguous/);
});

const modelAuthorityStatements = [
  ['read-initialization', 'permission := "org.models.read"', 'permission := "org.models.manage"'],
  ['put-selection', 'if r.Method == http.MethodPut {\n\t\tpermission = "org.models.manage"\n\t}', 'if r.Method == http.MethodPut {\n\t\t_ = permission\n\t}'],
  ['authorizer-call', 'actor, authorizationVersion, err := s.authorizer.AuthorizedOrganizationUserID(r.Context(), principal, permission, organizationID)', 'actor, authorizationVersion, err := s.authorizer.AuthorizedOrganizationUserID(r.Context(), principal, "org.models.read", organizationID)\n\t_ = permission'],
];
for (const [boundary, original, replacement] of modelAuthorityStatements) {
  for (const [name, standIn] of Object.entries(goAuthorityStandIns(original))) {
    test(`organization model ${boundary} cannot be supplied by ${name}`, () => {
      const normalized = inputs.organizationHandlerSource.replace(/\r\n/g, '\n');
      assert.ok(normalized.includes(original));
      const source = normalized.replace(original, `${standIn}\n\t${replacement}`);
      assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), /organization model authority changed/);
    });
  }
}
test('organization model authority ignores examples while retaining its real three-stage authority', () => {
  let source = inputs.organizationHandlerSource.replace(/\r\n/g, '\n');
  for (const [, original, replacement] of modelAuthorityStatements) {
    source = source.replace(original, `${Object.values(goAuthorityStandIns(replacement)).join('\n\t')}\n\t${original}`);
  }
  assert.doesNotThrow(() => checkCoverage({ ...inputs, organizationHandlerSource: source }));
});

// These are finite receiver-shape checks, not Go alias or control-flow
// analysis. The source contracts support the installed root Service receiver.
const organizationHandlerVariant = (handler, transform) => {
  const source = inputs.organizationHandlerSource.replace(/\r\n/g, '\n');
  const pattern = new RegExp('func \\(s \\*Service\\) ' + handler + '\\([^\\n]*\\) \\{([\\s\\S]*?)\\n\\}', 'g');
  const matches = [...source.matchAll(pattern)];
  assert.equal(matches.length, 1);
  const match = matches[0];
  const body = transform(match[1]);
  return source.slice(0, match.index) + match[0].replace(match[1], body) + source.slice(match.index + match[0].length);
};
const receiverShapes = [
  ['qualified', 'forwarded := struct { s *Service }{s}', 'forwarded.s'],
  ['dot-space', 'forwarded := struct { s *Service }{s}', 'forwarded. s'],
  ['dot-newline', 'forwarded := struct { s *Service }{s}', 'forwarded.\n\t\ts'],
  ['dot-block-comment', 'forwarded := struct { s *Service }{s}', 'forwarded. /* selector */ s'],
  ['dot-line-comment', 'forwarded := struct { s *Service }{s}', 'forwarded. // selector\n\t\ts'],
  ['unicode-letter', '他s := s', '他s'],
  ['unicode-astral-letter', '𐐀s := s', '𐐀s'],
  ['unicode-astral-digit', 'a𝟙s := s', 'a𝟙s'],
];
const withReceiver = (handler, call, declaration, receiver) => organizationHandlerVariant(handler, body => {
  assert.equal(body.split(call).length, 2);
  return `\n\t${declaration}\n` + body.replace(call, receiver + call.slice(1));
});
const ordinaryHandlers = inputs.manifest.organizationFinanceRoutes.filter(route =>
  !['organizationModelPolicy', 'reclaimClosedGrant', 'reclaimClosedGrantsBatch'].includes(route.handler));
assert.equal(ordinaryHandlers.length, 10);
for (const route of ordinaryHandlers) {
  test(`ordinary ${route.handler} rejects a qualified field instead of its root receiver`, () => {
    const call = `s.resolveOrganizationBilling(w, r, "${route.authority.all[0]}")`;
    const source = withReceiver(route.handler, call, receiverShapes[0][1], receiverShapes[0][2]);
    assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), /organization permission source missing/);
  });
}
for (const [name, declaration, receiver] of receiverShapes.slice(1)) {
  test(`ordinary recharge authority rejects ${name} receiver prefixes`, () => {
    const call = 's.resolveOrganizationBilling(w, r, "org.entitlements.purchase")';
    const source = withReceiver('createOrganizationRechargeOrder', call, declaration, receiver);
    assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), /organization permission source missing/);
  });
}
for (const [name, declaration, receiver] of receiverShapes) {
  test(`organization model authorizer rejects ${name} receiver prefixes`, () => {
    const call = 's.authorizer.AuthorizedOrganizationUserID(r.Context(), principal, permission, organizationID)';
    const source = withReceiver('organizationModelPolicy', call, declaration, receiver);
    assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), /organization model authority changed/);
  });
}
for (const name of ['他permission', '𐐀permission']) {
  test(`organization model read initialization rejects another identifier ${name}`, () => {
    const source = organizationHandlerVariant('organizationModelPolicy', body => body.replace('permission := "org.models.read"',
      `permission := "org.models.manage"\n\t${name} := "org.models.read"\n\t_ = ${name}`));
    assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), /organization model authority changed/);
  });
}
for (const [handler, call] of [
  ['reclaimClosedGrant', 's.closedGrantClient(w, r)'],
  ['closedGrantClient', 's.resolveOrganizationBilling(w, r, "org.grants.reclaim_closed")'],
]) {
  test(`closed-grant ${handler} retains its existing strict receiver rejection`, () => {
    const source = withReceiver(handler, call, receiverShapes[0][1], receiverShapes[0][2]);
    assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), /closed grant authority/);
  });
}
test('real organization authority remains valid after harmless literals and unrelated identifiers', () => {
  const source = organizationHandlerVariant('organizationModelPolicy', body => '\n\t_ = 1.\n\t_ = "forwarded.s."\n\t他s := s\n\t_ = 他s\n' + body);
  assert.doesNotThrow(() => checkCoverage({ ...inputs, organizationHandlerSource: source }));
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
