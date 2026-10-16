import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { validateCatalog, runAuthorizationCoverage, authorizationConsumers, authorizationRoutes } from '../check-account-platform-authorization-coverage.mjs';

const source = 'account-platform/internal/token/personal_execution_routes.go';
function fixture() {
  const route = { source, receiver: 'protected', method: 'POST', path: '/chat/completions', registration: 'protected.POST("/chat/completions", httpserver.AdaptFunc(s.chatCompletions))' };
  const catalog = { version: 1, http_sources: [source], http_policies: [{ ...route, scope: 'personal', permissions: ['personal.model.invoke'], strategy: 'domain.personal-context-and-subject-owned-execution' }], tasks: [], events: [], cli: [] };
  const discovered = { routes: [route], httpSources: [source], tasks: [], events: [], cli: [], textBySource: new Map() };
  return { catalog, discovered, permissions: new Set(['personal.model.invoke']) };
}

test('an unclassified execution route blocks coverage', () => {
  const f = fixture(); f.catalog.http_policies = [];
  assert.ok(validateCatalog(f.catalog, f.discovered, f.permissions).some(error => error.startsWith('unclassified_http_entrypoint:')));
});

test('an installed personal policy cannot cite an undefined permission', () => {
  const f = fixture(); f.permissions.clear();
  assert.ok(validateCatalog(f.catalog, f.discovered, f.permissions).some(error => error.startsWith('unknown_human_permission:')));
});

test('a personal entrypoint cannot use a platform permission', () => {
  const f = fixture(); f.catalog.http_policies[0].permissions = ['platform.models.manage']; f.permissions.add('platform.models.manage');
  assert.ok(validateCatalog(f.catalog, f.discovered, f.permissions).some(error => error.startsWith('permission_scope_mismatch:')));
});

test('conjunctive inline middleware checks both permission sets', () => {
  const f = fixture(); f.catalog.http_policies = [];
  f.discovered.routes[0].registration = 'admin.GET("/resource-audit", s.authorizer.RequireAnyAnd([]string{"platform.models.read"}, []string{"platform.audit.read"}), httpserver.AdaptFunc(s.adminResourceAudit))';
  f.permissions = new Set(['platform.models.read']);
  assert.ok(validateCatalog(f.catalog, f.discovered, f.permissions).some(error => error.endsWith(':platform.audit.read')));
  f.permissions.add('platform.audit.read');
  assert.deepEqual(validateCatalog(f.catalog, f.discovered, f.permissions), []);
});

test('missing scheduled jobs, event consumers and operator commands all block', () => {
  const f = fixture();
  f.discovered.tasks.push({ source, name: 'usage-reconciliation' });
  f.discovered.events.push({ source, durable: 'public-durable', subject: 'public.subject' });
  f.discovered.cli.push({ source: 'account-platform/cmd/operator/main.go', name: 'operator' });
  const errors = validateCatalog(f.catalog, f.discovered, f.permissions);
  for (const prefix of ['unclassified_task:', 'unclassified_event:', 'unclassified_cli:']) assert.ok(errors.some(error => error.startsWith(prefix)));
});

test('the actual assembled production entrypoints are all classified', async () => {
  const result = await runAuthorizationCoverage();
  for (const key of ['routes', 'tasks', 'events', 'cli']) assert.ok(result[key] > 0);
});

async function withActualCatalog(mutator, check) {
  const catalog = JSON.parse(await readFile(new URL('../../account-platform/deploy/authorization-entrypoints.json', import.meta.url), 'utf8'));
  mutator(catalog);
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-authorization-coverage-'));
  try {
    const file = path.join(directory, 'catalog.json');
    await writeFile(file, JSON.stringify(catalog), 'utf8');
    await check(file);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

test('actual wildcard proxy routes cannot disappear from the authorization catalog', async () => {
  await withActualCatalog(catalog => {
    catalog.http_policies = catalog.http_policies.filter(item => item.method !== 'ANY');
  }, async file => {
    await assert.rejects(runAuthorizationCoverage(file), /unclassified_http_entrypoint:.*\|ANY\|/);
  });
});

test('actual registry-backed Billing authorization consumers cannot disappear from the catalog', async () => {
  await withActualCatalog(catalog => {
    catalog.events = catalog.events.filter(item => !(item.source === 'account-platform/cmd/billing-service/main.go' && item.subject === 'account.approval-command.authorized.v1'));
  }, async file => {
    await assert.rejects(runAuthorizationCoverage(file), /unclassified_event:.*billing-wallet-adjustment-executor-p1-v1/);
  });
});

const billingSourceName = 'account-platform/cmd/billing-service/main.go';
async function actualBillingSources() {
  return Promise.all([
    readFile(new URL('../../account-platform/cmd/billing-service/main.go', import.meta.url), 'utf8'),
    readFile(new URL('../../account-platform/internal/platform/approvalconsumer/registry.go', import.meta.url), 'utf8'),
  ]);
}

test('actual Billing loop resolves every closed registry durable, excluding Token and legacy consumers', async () => {
  const [main, registry] = await actualBillingSources();
  const events = authorizationConsumers(billingSourceName, main, registry);
  const catalog = JSON.parse(await readFile(new URL('../../account-platform/deploy/authorization-entrypoints.json', import.meta.url), 'utf8'));
  const registered = catalog.events.filter(event => event.source === billingSourceName && event.subject === 'account.approval-command.authorized.v1');
  assert.deepEqual(events.map(event => event.durable).sort(), registered.map(event => event.durable).sort());
  for (const durable of ['billing-payment-product-resume-executor-p1-v1', 'billing-payment-provider-resume-executor-p2-v1', 'billing-wallet-adjustment-executor-p1-v1']) assert.ok(events.some(event => event.durable === durable));
  assert.ok(events.every(event => event.durable !== 'billing-command-executor-v1' && !event.durable.startsWith('token-')));
  assert.ok(events.every(event => event.source === billingSourceName && event.subject === 'account.approval-command.authorized.v1'));
});

test('new closed Billing descriptors are discovered rather than frozen to the original three consumers', async () => {
  const [main, registry] = await actualBillingSources();
  const next = registry.replace('var descriptors = []Descriptor{', 'var descriptors = []Descriptor{\n{CommandType: "TEST_ADDED_COMMAND", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "test-added-durable", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},');
  const events = authorizationConsumers(billingSourceName, main, next);
  assert.equal(events.length, authorizationConsumers(billingSourceName, main, registry).length + 1);
  assert.equal(events.filter(event => event.durable === 'test-added-durable').length, 1);
});

test('unknown dynamic authorization consumer calls and registry shapes fail closed', async () => {
  const [main, registry] = await actualBillingSources();
  for (const expression of ['runtimeDurable()', 'other.Durable', 'descriptor.Durable + "suffix"']) {
    assert.throws(() => authorizationConsumers(billingSourceName, main.replace('descriptor.Durable, consumer.Handle', `${expression}, consumer.Handle`), registry), /unrecognized_authorization_consumer:/);
  }
  assert.throws(() => authorizationConsumers(billingSourceName, 'broker.ConsumeAuthorization(ctx, descriptor.Durable, handler)', registry), /unrecognized_authorization_consumer:/);
  assert.throws(() => authorizationConsumers(billingSourceName, 'consume := broker.ConsumeAuthorization', registry), /unrecognized_authorization_consumer:/);
  assert.throws(() => authorizationConsumers(billingSourceName, main, registry.replace('descriptor.Workload == BillingWorkload', 'descriptor.Workload != BillingWorkload')), /unrecognized_billing_consumer_registry/);
  assert.throws(() => authorizationConsumers(billingSourceName, main, registry.replace('Durable: "billing-wallet-adjustment-executor-p1-v1"', 'Durable: dynamicDurable()')), /unrecognized_billing_consumer_descriptor/);
  assert.throws(() => authorizationConsumers(billingSourceName, main, registry + '\nfunc init() { descriptors = dynamicallyLoaded() }'), /unrecognized_billing_consumer_registry/);
});

test('literal authorization consumers remain supported and comments cannot register consumers', () => {
  const events = authorizationConsumers('literal.go', '// broker.ConsumeAuthorization(ctx, "comment", handle)\nbroker.ConsumeAuthorization(ctx, "real", handle)', '');
  assert.deepEqual(events, [{ source: 'literal.go', durable: 'real', subject: 'account.approval-command.authorized.v1' }]);
});

test('actual Portal wildcard registrations preserve all six routes and reject unknown dynamic paths', async () => {
  const name = 'account-platform/internal/portal/service.go';
  const portal = await readFile(new URL(`../../${name}`, import.meta.url), 'utf8');
  const wildcard = authorizationRoutes(name, portal).filter(route => route.method === 'ANY');
  assert.deepEqual(wildcard.map(route => route.path).sort(), [
    '/api/account/*resource', '/api/admin/account/*resource', '/api/admin/approvals/requests/*resource',
    '/api/admin/billing/*resource', '/api/admin/token/*resource', '/api/token/*resource',
  ]);
  assert.throws(() => authorizationRoutes(name, portal.replace('router.Any("/api/account/*resource"', 'router.Any(accountPath')), /unrecognized_wildcard_http_entrypoint:/);
  assert.equal(authorizationRoutes('comment.go', '// router.Any("/comment", handler)').length, 0);
  assert.throws(() => authorizationRoutes(name, 'register := router.Any'), /unrecognized_wildcard_http_entrypoint:/);
});

test('phase coverage unit tests are selected and actually executed by the account-platform CI gate', async () => {
  const workflow = await readFile(new URL('../../.github/workflows/pr-check.yml', import.meta.url), 'utf8');
  assert.match(workflow, /- 'scripts\/tests\/token-management-phase-coverage\.test\.mjs'/);
  assert.match(workflow, /^\s+node --test scripts\/tests\/token-management-phase-coverage\.test\.mjs\s*$/m);
});


test('quoted and raw Go examples cannot invent wildcard routes or authorization consumers', () => {
  const examples = 'var quoted = "router.Any(\\"/quoted\\", handler)"\nvar raw = `router.Any("/raw", handler)\nbroker.ConsumeAuthorization(ctx, "raw", handle)`';
  assert.deepEqual(authorizationRoutes('examples.go', examples), []);
  assert.deepEqual(authorizationConsumers('examples.go', examples, ''), []);
});


for (const [name, mutation] of [
  ['Durable field assignment', 'descriptors[0].Durable = "changed-durable"'],
  ['Workload field assignment', 'descriptors[0].Workload = TokenWorkload'],
  ['compound field assignment', 'descriptors[0].Durable += "-changed"'],
  ['multiple assignment with registry first', 'var other string; descriptors[0].Durable, other = "changed-durable", "other"; _ = other'],
  ['multiple assignment with registry second', 'var other string; other, descriptors[0].Durable = "other", "changed-durable"; _ = other'],
  ['field increment', 'descriptors[0].PolicyVersion++'],
  ['element swap', 'descriptors[0], descriptors[1] = descriptors[1], descriptors[0]'],
  ['append', 'descriptors = append(descriptors, descriptors[0])'],
  ['copy', 'copy(descriptors[0:], descriptors[1:])'],
  ['clear', 'clear(descriptors)'],
  ['writable slice alias escape', 'alias := descriptors; alias[0].Durable = "changed-durable"'],
  ['writable element address escape', 'alias := &descriptors[0]; alias.Durable = "changed-durable"'],
]) {
  test(`closed consumer registry rejects ${name}`, async () => {
    const [main, registry] = await actualBillingSources();
    assert.throws(() => authorizationConsumers(billingSourceName, main, `${registry}\nfunc init() { ${mutation} }`), /unrecognized_billing_consumer_registry/);
  });
}


for (const method of ['GET', 'POST', 'PUT', 'PATCH', 'DELETE']) {
  test(`ordinary ${method} registration rejects a nonliteral path on an actual router`, () => {
    const input = `router := httpserver.NewRouter()\nrouter.${method}(runtimePath, handler)`;
    assert.throws(() => authorizationRoutes('dynamic.go', input), /unrecognized_http_entrypoint:dynamic.go/);
  });
}

test('ordinary registration accounting follows actual typed routers and nested groups', () => {
  for (const input of [
    'func register(router *httpserver.Router) { router.GET(runtimePath, handler) }',
    'func register(protected *httpserver.RouteGroup) { protected.POST(runtimePath, handler) }',
    'router := httpserver.NewRouter()\nprotected := router.Group("/v1")\nadmin := protected.Group("/admin")\nadmin.PATCH(runtimePath, handler)',
  ]) assert.throws(() => authorizationRoutes('dynamic.go', input), /unrecognized_http_entrypoint:/);
});

test('ordinary method values and unsupported whitespace grammar cannot disappear', () => {
  for (const input of [
    'router := httpserver.NewRouter()\nregister := router.GET',
    'router := httpserver.NewRouter()\nrouter.GET ("/resource", handler)',
    'router := httpserver.NewRouter()\nrouter . POST("/resource", handler)',
    'router := httpserver.NewRouter()\nrouter.GET(`raw-path`, handler)',
  ]) assert.throws(() => authorizationRoutes('dynamic.go', input), /unrecognized_http_entrypoint:/);
});

test('ordinary literal route grammar remains discovered with complete registration evidence', () => {
  const input = 'router := httpserver.NewRouter()\nrouter.GET("/resource", authorizer.Require("personal.read"), handler)';
  assert.deepEqual(authorizationRoutes('literal.go', input), [{ source: 'literal.go', receiver: 'router', method: 'GET', path: '/resource', registration: 'router.GET("/resource", authorizer.Require("personal.read"), handler)' }]);
});

test('ordinary accounting ignores Go examples, unrelated receivers and router forwarding internals', () => {
  const examples = [
    '// router.GET(runtimePath, handler)',
    '/* protected.POST(runtimePath, handler) */',
    'var quoted = "router.GET(runtimePath, handler)"',
    'var raw = `router.POST(runtimePath, handler)`',
    'client.GET(runtimePath, handler)',
    'recorder.POST(runtimePath, handler)',
    'router.Engine.GET(relativePath, handlers...)',
    'group.RouterGroup.GET(relativePath, handlers...)',
  ].join('\n');
  assert.deepEqual(authorizationRoutes('examples.go', examples), []);
});

test('a dynamic mutation of an actual ordinary production registration fails closed', async () => {
  const name = 'account-platform/internal/token/personal_execution_routes.go';
  const production = await readFile(new URL(`../../${name}`, import.meta.url), 'utf8');
  const anchor = 'protected.POST("/chat/completions"';
  assert.equal(production.split(anchor).length, 2);
  assert.throws(() => authorizationRoutes(name, production.replace(anchor, 'protected.POST(runtimePath')), /unrecognized_http_entrypoint:/);
});
