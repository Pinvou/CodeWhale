import test from 'node:test';
import assert from 'node:assert/strict';
import { validateCatalog, runAuthorizationCoverage } from '../check-account-platform-authorization-coverage.mjs';

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
