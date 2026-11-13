import assert from 'node:assert/strict';
import { test } from 'node:test';
import { adminComponent, adminDOM } from './helpers/admin-component-fixture.mjs';
import { tokenManagementCopy } from '../src/shared/i18n.js';

test('provisioning clear removes an unsubmitted draft even when the applied query stays empty', async t => {
  const { ProvisioningFilters } = await adminComponent('AdminProvisioningWorkspace.jsx', 'ProvisioningFilters');
  const ui = await adminDOM(t), calls = [];
  const query = { q: '', status: '', model_id: '', token_account_id: '', scope_type: '', scope_id: '', sla_overdue: '', cursor: '' };
  const props = { query, items: [], copy: tokenManagementCopy.en.modelToken, onQueryChange: (source, value) => calls.push({ source, value }) };
  await ui.render(ProvisioningFilters, props);
  const form = () => ui.document.querySelector('form');
  await ui.change(form().querySelector('input'), 'unsubmitted-model');
  await ui.click(form().querySelector('button[type=button]'));
  assert.equal(form().querySelector('input').value, '');
  await ui.submit(form());
  assert.equal(calls.at(-1).source, 'provisionings');
  assert.deepEqual(calls.at(-1).value, query);
  await ui.render(ProvisioningFilters, { ...props, query: { ...query, q: 'restored-history', status: 'FAILED' } });
  assert.equal(form().querySelector('input').value, 'restored-history');
  await ui.click(form().querySelector('button[type=button]'));
  assert.equal(form().querySelector('input').value, '');
  await ui.submit(form());
  assert.deepEqual(calls.at(-1).value, query);
});

test('shared model-token clear also resets a draft when no route change occurs', async t => {
  const { FilterForm } = await adminComponent('AdminModelTokenPrimitives.jsx');
  const ui = await adminDOM(t), calls = [];
  await ui.render(FilterForm, { source: 'models', query: { q: '', cursor: '' }, fields: [{ key: 'q', label: 'Model' }], copy: tokenManagementCopy.en.modelToken, onQueryChange: (source, query) => calls.push({ source, query }) });
  const form = ui.document.querySelector('form');
  await ui.change(form.querySelector('input'), 'unsubmitted-model');
  await ui.click(form.querySelector('button[type=button]'));
  assert.equal(form.querySelector('input').value, '');
  await ui.submit(form);
  assert.deepEqual(calls.at(-1), { source: 'models', query: { q: '', cursor: '' } });
});
