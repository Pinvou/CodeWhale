import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { authorizationRoutes, goSource } from '../../../scripts/check-account-platform-authorization-coverage.mjs';

const load = async relative => JSON.parse(await readFile(new URL(`../../${relative}`, import.meta.url), 'utf8'));
const routeKey = (method, path) => `${method.toUpperCase()} ${path.replace(/:[^/]+/g, match => `{${match.slice(1)}}`)}`;

test('registration signup contract stays aligned with installed account routes', async () => {
  const [contract, inventory] = await Promise.all([
    load('contracts/openapi/registration-signup.json'),
    load('deploy/authorization-entrypoints.json'),
  ]);
  const contractRoutes = new Set(
    Object.entries(contract.paths).flatMap(([path, operations]) =>
      Object.keys(operations).filter(method => !method.startsWith('x')).map(method => routeKey(method, path)),
    ),
  );
  const installed = inventory.http_policies.filter(entry =>
    entry.source === 'account-platform/internal/account/service.go' &&
    (entry.path.startsWith('/v1/auth/') || entry.path.startsWith('/admin/v1/registration-') || entry.path.startsWith('/internal/v1/registration-')),
  );
  assert.ok(installed.length > 0, 'registration routes must be present in the authorization inventory');
  for (const entry of installed) {
    assert.ok(entry.strategy, `${entry.method} ${entry.path} must have an authorization strategy`);
    assert.ok(contractRoutes.has(routeKey(entry.method, entry.path)), `${entry.method} ${entry.path} is missing from registration-signup.json`);
  }
});

test('identity dispatch OpenAPI covers both protected creation routes', async () => {
  const [contract, service] = await Promise.all([
    load('contracts/openapi/identity-registration-dispatch.json'),
    readFile(new URL('../../internal/identityadapter/service.go', import.meta.url), 'utf8'),
  ]);
  assert.match(goSource(service).code, /internal\s*:=\s*router\.Group\("\/internal\/v1"\)/);
  const routes = authorizationRoutes('account-platform/internal/identityadapter/service.go', service);
  for (const [path, mode] of [
    ['/users/mobile', 'user-v1'],
    ['/users/mobile/sms-only', 'sms-only-v1'],
  ]) {
    const route = routes.find(entry => entry.receiver === 'internal' && entry.method === 'POST' && entry.path === path);
    assert.ok(route, 'missing protected POST ' + path);
    assert.match(route.registration, /serviceauth\.Require\("internal\.identity\.user\.create"\)/);
    const operation = contract.paths['/internal/v1' + path]?.post;
    assert.ok(operation, 'missing OpenAPI operation for POST ' + path);
    assert.deepEqual(operation.security, [{ workloadBearer: [] }]);
    const schema = operation.requestBody.content['application/json'].schema;
    assert.equal(operation.requestBody.required, true);
    assert.ok(schema.required.includes('first_attempt'), 'the durable grant attestation is required');
    assert.equal(schema.properties.first_attempt.type, 'boolean');
    assert.equal(schema.properties.password_policy_version.const, mode);
    assert.ok(operation.responses['410'], 'the contract must include the explicit non-dispatch result');
    if (mode === 'sms-only-v1') {
      assert.equal(schema.additionalProperties, false);
      assert.equal(schema.properties.password_handle.const, '');
    }
  }
});
