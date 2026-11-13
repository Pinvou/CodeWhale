import assert from 'node:assert/strict';
import { test } from 'node:test';
import { adminComponent, adminDOM } from './helpers/admin-component-fixture.mjs';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { tokenManagementCopy } from '../src/shared/i18n.js';

for (const surface of ['account', 'model', 'operations']) test('provisioning ' + surface + ' preserves int64 source digits through the actual client and table', async t => {
  const exports = surface === 'account' ? await adminComponent('AdminTokenAccountDetailWorkspace.jsx', 'ProvisioningsTab')
    : surface === 'model' ? await adminComponent('AdminModelDetailWorkspace.jsx', 'OperationsTab')
      : await adminComponent('AdminModelOperationsWorkspace.jsx');
  const Component = exports.ProvisioningsTab || exports.OperationsTab || exports.AdminModelOperationsWorkspace;
  const ui = await adminDOM(t), previousFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = previousFetch; });
  const client = new AdminPlatformClient(), calls = [];
  const id = '11111111-1111-4111-8111-111111111111';
  const body = '{"items":[{"provisioning_id":"synthetic-provisioning","status":"READY","stage":"READY","actual_generation":9007199254740993,"desired_generation":9223372036854775807,"updated_at":"2026-10-07T10:00:00Z"}],"has_more":false,"next_cursor":""}';
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return new Response(body); };
  await ui.render(Component, { client, copy: tokenManagementCopy.en.modelToken, accountID: id, modelID: id, query: {}, onQueryChange() {}, cursorHistory: { current: {} }, permissions: ['platform.tokens.read'], routeVersion: 0, onRouteChange() {} });
  assert.equal(calls.length, 1);
  const params = new URL(calls[0].url, 'https://example.test').searchParams;
  if (surface === 'account') assert.equal(params.get('token_account_id'), id);
  if (surface === 'model') assert.equal(params.get('model_id'), id);
  assert.ok(ui.document.querySelector('tbody').textContent.includes('9007199254740993 / 9223372036854775807'));
  const list = await client.provisioningsForPresentation({ token_account_id: id });
  assert.equal(list.items[0].actual_generation, '9007199254740993');
  assert.equal(list.items[0].desired_generation, '9223372036854775807');
  assert.equal(list.items[0].status, 'READY');
});
