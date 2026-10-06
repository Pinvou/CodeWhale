import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const rootLoad = path => readFile(new URL(`../../../${path}`, import.meta.url), 'utf8');
const load = path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('internal organization authorization requires its local member version without changing global versions', async () => {
  const document = YAML.parse(await load('contracts/openapi/account.yaml'));
  const schema = document.paths['/me/authorization-context'].get.responses['200'].content['application/json'].schema;
  const ajv = new Ajv2020({ strict: true, allowUnionTypes: true, allErrors: true });
  addFormats(ajv);
  ajv.addFormat('int64', { type: 'number', validate: Number.isInteger });
  const validate = ajv.compile(schema);
  const personal = {
    user_id: '00000000-0000-4000-8000-000000000001',
    session_id: 'controlled-session', context_type: 'PERSONAL',
    membership_version: 7, context_version: 4, authorization_version: 3,
    unit_ids: [], ancestor_unit_ids: [], roles: [], permissions: [],
    platform_roles: [], platform_permissions: [], mobile_verified: false,
  };
  assert.equal(validate(personal), true, JSON.stringify(validate.errors));
  const organization = { ...personal, context_type: 'ORGANIZATION', organization_id: '00000000-0000-4000-8000-000000000002', member_version: 2 };
  assert.equal(validate(organization), true, JSON.stringify(validate.errors));
  for (const member_version of [0, -1, '2', null]) {
    assert.equal(validate({ ...organization, member_version }), false);
  }
  const missing = { ...organization };
  delete missing.member_version;
  assert.equal(validate(missing), false);
  assert.equal(validate({ ...organization, organization_id: null }), false);
  assert.equal(validate({ ...organization, unexpected_field: true }), false);
});

test('active context is a monotonic user-level aggregate with CAS switching', async () => {
  const [migration, repository, membership, service, openapi] = await Promise.all([
    load('migrations/account/000022_global_active_context.up.sql'),
    load('internal/account/postgres.go'),
    load('internal/account/membership_governance.go'),
    load('internal/account/service.go'),
    load('contracts/openapi/account.yaml'),
  ]);

  assert.match(migration, /CREATE TABLE user_active_contexts/);
  assert.match(migration, /user_id UUID PRIMARY KEY/);
  assert.match(migration, /COALESCE\(switched\.occurred_at,account_user\.updated_at\)/);
  assert.match(migration, /action IN \('session\.active_context\.set','user\.active_context\.set'\)/);
  assert.match(migration, /MAX\(membership_version\) AS context_version/);
  assert.doesNotMatch(migration, /FROM users user\b|COALESCE\(switched\.occurred_at,user\.updated_at\)/);
  assert.match(migration, /NEW\.context_version<=OLD\.context_version/);
  assert.match(repository, /JOIN user_active_contexts active ON active\.user_id=u\.id/);
  assert.match(repository, /FOR UPDATE OF active/);
  assert.match(repository, /contextVersion != expectedVersion/);
  assert.match(membership, /UPDATE user_active_contexts SET context_type='PERSONAL'/);
  assert.match(service, /expected_context_version/);
  assert.match(openapi, /CAS-select the user's global personal or organization context/);
});

test('authenticated responses propagate versions and clients invalidate stale context caches', async () => {
  const [accountAuth, tokenService, portal, bff, desktop, view, popover, rustBridge, viteConfig] = await Promise.all([
    load('internal/account/authorization.go'),
    load('internal/token/service.go'),
    load('internal/portal/service.go'),
    rootLoad('pinvou3-app/src/features/account-platform/bff-client.js'),
    rootLoad('pinvou3-app/src/features/account-platform/desktop-client.js'),
    rootLoad('pinvou3-app/src/features/account-platform/AccountPlatformView.jsx'),
    rootLoad('pinvou3-app/src/features/account-platform/AccountPlatformPopover.jsx'),
    rootLoad('pinvou3-app/src-tauri/src/features/account_platform/mod.rs'),
    rootLoad('pinvou3-app/vite.config.mjs'),
  ]);

  assert.match(accountAuth, /contextVersionHeaders/);
  assert.match(tokenService, /X-Context-Version/);
  assert.match(portal, /\[\]string\{"X-Context-Version", "X-Authorization-Version"\}/);
  assert.match(portal, /response\.Header\.Get\(name\)/);
  assert.match(bff, /onContextVersion/);
  assert.match(desktop, /response\.contextVersion/);
  assert.match(rustBridge, /context_version: Option<u64>/);
  assert.match(view, /addEventListener\?\.\('focus'/);
  assert.match(view, /clearContextResources\(\)/);
  assert.match(view, /expectedVersion/);
  assert.match(popover, /client\.onContextVersion/);
  assert.match(popover, /addEventListener(?:\?\.)?\('focus'/);
  assert.match(viteConfig, /emptyOutDir: true/);
  assert.match(viteConfig, /accountPortalBuild\s*\?\s*\{ account: resolve\(sourceRoot, 'account\.html'\), admin: resolve\(sourceRoot, 'admin\.html'\) \}/);
});
