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

test('unknown approval decisions stay locked until the frozen target detail is verified', async () => {
  const source = `${await readFile(sourceURL, 'utf8')}
globalThis.__approvalTest = {
  load,
  render,
  refreshUncertainDecision,
  lock(value) { uncertainDecision = value; pendingDecision = value; },
  open: openDecision,
  state() { return { uncertainDecision, pendingDecision }; },
};`;
  const pendingApprovals = [];
  const pendingDetails = [];
  const errorNode = { textContent: '' };
  const refreshButton = {};
  const root = {
    innerHTML: '',
    querySelector: (selector) => selector === '[data-refresh]' ? refreshButton : selector === '#approval-decision-error' ? errorNode : null,
    querySelectorAll: () => [],
  };
  const fetch = async (url) => {
    if (url === '/api/session') return response({ csrf_token: 'csrf' });
    if (String(url).includes('/api/admin/account/admin/v1/approval-requests?')) return new Promise((resolve, reject) => pendingApprovals.push({ resolve, reject }));
    if (String(url).includes('/api/admin/account/admin/v1/approval-requests/')) return new Promise((resolve, reject) => pendingDetails.push({ resolve, reject }));
    throw new Error(`unexpected_fetch:${url}`);
  };
  const listeners = new Map();
  const context = {
    console,
    document: { getElementById: () => root },
    fetch,
    FormData,
    localStorage: { getItem: () => 'en' },
    location: { search: '?section=approvals' },
    URLSearchParams,
    window: { addEventListener: (name, listener) => listeners.set(name, [...(listeners.get(name) || []), listener]) },
  };
  vm.runInNewContext(source, context);
  await waitFor(() => pendingApprovals.length === 1);
  const current = { ...approval('approval-locked', 'target-locked'), allowed_actions: ['APPROVE', 'REJECT'] };
  pendingApprovals[0].resolve(response({ items: [current] }));
  await waitFor(() => root.innerHTML.includes('data-decision="APPROVED"'));

  const uncertain = { item: current, decision: 'APPROVED', outcomeUnknown: true };
  context.__approvalTest.lock(uncertain);
  context.__approvalTest.render();
  assert.doesNotMatch(root.innerHTML, /data-decision="APPROVED"/);
  const lockedMarkup = root.innerHTML;

  const failedRefresh = context.__approvalTest.load();
  await waitFor(() => pendingApprovals.length === 2);
  pendingApprovals[1].reject(new Error('network down'));
  assert.equal(await failedRefresh, false);
  assert.equal(root.innerHTML, lockedMarkup);
  assert.match(errorNode.textContent, /unknown/i);
  context.__approvalTest.open(current, 'REJECTED');
  assert.equal(context.__approvalTest.state().pendingDecision.decision, 'APPROVED');
  listeners.get('pinvou-admin-language-change')[0]();
  assert.equal(root.innerHTML, lockedMarkup);

  const failedTargetRefresh = context.__approvalTest.refreshUncertainDecision();
  await waitFor(() => pendingDetails.length === 1);
  pendingDetails[0].reject(new Error('target lookup failed'));
  assert.equal(await failedTargetRefresh, false);
  assert.equal(context.__approvalTest.state().uncertainDecision.item.approval_request_id, 'approval-locked');

  const malformedDetails = [
    {},
    { ...current, approval_request_id: 'approval-other' },
    { ...current, version: 1 },
    { ...current, status: 'UNKNOWN' },
    { ...current, allowed_actions: null },
  ];
  for (const malformed of malformedDetails) {
    const attempt = context.__approvalTest.refreshUncertainDecision();
    await waitFor(() => pendingDetails.length >= 2 + malformedDetails.indexOf(malformed));
    pendingDetails.at(-1).resolve(response(malformed));
    assert.equal(await attempt, false);
    assert.equal(context.__approvalTest.state().uncertainDecision.item.approval_request_id, 'approval-locked');
  }

  const unrelatedRefresh = context.__approvalTest.load();
  await waitFor(() => pendingApprovals.length === 3);
  pendingApprovals[2].resolve(response({ items: [approval('approval-other', 'target-other')] }));
  assert.equal(await unrelatedRefresh, true);
  assert.equal(context.__approvalTest.state().uncertainDecision.item.approval_request_id, 'approval-locked');
  context.__approvalTest.open(current, 'REJECTED');
  assert.equal(context.__approvalTest.state().pendingDecision.decision, 'APPROVED');

  const successfulRefresh = context.__approvalTest.refreshUncertainDecision();
  await waitFor(() => pendingDetails.length === 2 + malformedDetails.length);
  pendingDetails.at(-1).resolve(response({ ...current, status: 'APPROVED', version: 3, allowed_actions: [] }));
  assert.equal(await successfulRefresh, true);
  assert.equal(context.__approvalTest.state().uncertainDecision, null);
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
