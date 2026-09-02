import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const sourceURL = new URL('../../internal/portal/static/assets/approval-requests.js', import.meta.url);
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

async function waitFor(predicate) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    if (predicate()) return;
    await tick();
  }
  throw new Error('condition_not_reached');
}

const response = (body) => ({ ok: true, status: 200, json: async () => body });
const approval = (id, target) => ({
  approval_request_id: id,
  command_type: 'WALLET_ADJUSTMENT',
  policy_version: 1,
  risk_class: 'FINANCIAL_FACT_CHANGE',
  status: 'SUBMITTED',
  version: 2,
  requester_user_id: '00000000-0000-0000-0000-000000000001',
  target_resource_type: 'MANUAL_ADJUSTMENT',
  target_resource_id: target,
  created_at: '2026-09-02T00:00:00Z',
  expires_at: '2026-09-03T00:00:00Z',
  expected_impact: {},
  normalized_command: {},
  critical_resource_versions: {},
  redacted_evidence: [],
  allowed_actions: [],
});

test('approval route changes ignore stale unfiltered and popstate responses', async () => {
  const source = await readFile(sourceURL, 'utf8');
  const listeners = new Map();
  const pendingApprovals = [];
  const location = { search: '?section=overview' };
  const refreshButton = {};
  const root = {
    innerHTML: '',
    querySelector: (selector) => selector === '[data-refresh]' ? refreshButton : null,
    querySelectorAll: () => [],
  };
  const window = {
    addEventListener: (name, listener) => listeners.set(name, [...(listeners.get(name) || []), listener]),
  };
  const fetch = async (url) => {
    if (url === '/api/session') return response({ csrf_token: 'csrf' });
    if (String(url).includes('/api/admin/billing/admin/v1/payment-provider-controls')) return response({});
    if (String(url).includes('/api/admin/account/admin/v1/approval-requests?')) {
      return new Promise((resolve) => pendingApprovals.push({ url: String(url), resolve }));
    }
    throw new Error(`unexpected_fetch:${url}`);
  };

  vm.runInNewContext(source, {
    console,
    crypto: { randomUUID: () => '00000000-0000-0000-0000-000000000099' },
    document: { getElementById: () => root },
    fetch,
    localStorage: { getItem: () => 'en' },
    location,
    URLSearchParams,
    window,
  });

  await waitFor(() => pendingApprovals.length === 1);
  location.search = '?section=approvals&approval_command_type=WALLET_ADJUSTMENT&approval_target=target-new';
  listeners.get('pinvou-admin-route-change')[0]();
  await waitFor(() => pendingApprovals.length === 2);
  assert.match(pendingApprovals[1].url, /command_type=WALLET_ADJUSTMENT/);
  assert.match(pendingApprovals[1].url, /target=target-new/);
  pendingApprovals[1].resolve(response({ items: [approval('approval-new', 'target-new')] }));
  await waitFor(() => root.innerHTML.includes('target-new'));
  pendingApprovals[0].resolve(response({ items: [approval('approval-old', 'target-old')] }));
  await tick();
  assert.match(root.innerHTML, /target-new/);
  assert.doesNotMatch(root.innerHTML, /target-old/);

  location.search = '?section=approvals';
  listeners.get('popstate')[0]();
  await waitFor(() => pendingApprovals.length === 3);
  location.search = '?section=approvals&approval_command_type=WALLET_ADJUSTMENT&approval_target=target-next';
  listeners.get('popstate')[0]();
  await waitFor(() => pendingApprovals.length === 4);
  pendingApprovals[3].resolve(response({ items: [approval('approval-next', 'target-next')] }));
  await waitFor(() => root.innerHTML.includes('target-next'));
  pendingApprovals[2].resolve(response({ items: [approval('approval-back', 'target-back')] }));
  await tick();
  assert.match(root.innerHTML, /target-next/);
  assert.doesNotMatch(root.innerHTML, /target-back/);
});
