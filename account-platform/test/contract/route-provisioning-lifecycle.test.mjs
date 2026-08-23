import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const root = new URL('../../', import.meta.url);
const read = path => readFileSync(new URL(path, root), 'utf8');

test('managed route provisioning gates model eligibility and exposes bounded repair', () => {
  const migration = read('migrations/token/000013_route_provisioning.up.sql');
  const provisioning = read('internal/token/provisioning.go');
  const proxy = read('internal/token/proxy.go');
  const repository = read('internal/token/postgres.go');
  const service = read('internal/token/service.go');
  const publicAPI = read('contracts/openapi/token-gateway.yaml');
  const adminAPI = read('contracts/openapi/admin.yaml');
  const portal = read('internal/portal/service.go');
  const adminUI = read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx');
  const accountUI = read('../pinvou3-app/src/features/account-platform/AccountPlatformView.jsx');
  const desktop = read('../pinvou3-app/src-tauri/src/features/account_platform/mod.rs');

  for (const table of ['route_provisionings', 'route_provisioning_attempts', 'route_provisioning_commands', 'route_provisioning_audit', 'organization_model_policies', 'organization_model_policy_commands', 'organization_model_policy_audit']) assert.match(migration, new RegExp(`CREATE TABLE ${table}`));
  assert.match(migration, /PENDING.*READY.*FAILED.*REPAIR_REQUIRED/);
  assert.match(provisioning, /FOR UPDATE SKIP LOCKED/);
  assert.match(provisioning, /provisioningBackoff/);
  assert.match(provisioning, /SLA_EXCEEDED/);
  assert.match(provisioning, /REPAIR_REQUESTED/);
  assert.match(provisioning, /OrganizationModelAllowed/);
  assert.match(proxy, /ResolveCatalogModel[\s\S]*defaultRoutes\.Ensure[\s\S]*ListCandidates[\s\S]*Price[\s\S]*Hold/);
  assert.match(repository, /JOIN route_provisionings provision/);
  assert.match(service, /\/admin\/v1[\s\S]*\/provisionings/);
  assert.match(publicAPI, /organizations\/\{organization_id\}\/model-policy/);
  assert.match(adminAPI, /provisionings\/\{provisioning_id\}\/repair/);
  assert.match(portal, /model-policy/);
  assert.match(adminUI, /repairProvisioning/);
  assert.match(accountUI, /ModelPolicy/);
  assert.match(desktop, /model-policy/);
});
