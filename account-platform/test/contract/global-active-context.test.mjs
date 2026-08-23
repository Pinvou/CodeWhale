import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const rootLoad = path => readFile(new URL(`../../../${path}`, import.meta.url), 'utf8');
const load = path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

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
    rootLoad('pinvou3-app/vite.config.js'),
  ]);

  assert.match(accountAuth, /contextVersionHeaders/);
  assert.match(tokenService, /X-Context-Version/);
  assert.match(portal, /response\.Header\.Get\("X-Context-Version"\)/);
  assert.match(bff, /onContextVersion/);
  assert.match(desktop, /response\.contextVersion/);
  assert.match(rustBridge, /context_version: Option<u64>/);
  assert.match(view, /addEventListener\?\.\('focus'/);
  assert.match(view, /clearContextResources\(\)/);
  assert.match(view, /expectedVersion/);
  assert.match(popover, /client\.onContextVersion/);
  assert.match(popover, /addEventListener\('focus'/);
  assert.match(viteConfig, /emptyOutDir: !accountPortalBuild/);
});
