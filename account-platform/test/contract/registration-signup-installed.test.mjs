import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

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
