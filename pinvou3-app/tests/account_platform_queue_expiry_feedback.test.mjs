import assert from 'node:assert/strict';
import { test } from 'node:test';
import { adminComponent, adminDOM } from './helpers/admin-component-fixture.mjs';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { adminLayoutCopy, financeCopy } from '../src/shared/i18n.js';

for (const language of ['zh', 'en', 'ja']) for (const anomaly of [false, true]) test('first-batch expiry has explicit localized recovery without automatic retry: ' + language + '/' + anomaly, async t => {
  const { AdminPaymentReviewQueue } = await adminComponent('AdminPaymentReviewQueue.jsx');
  const ui = await adminDOM(t), previousFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = previousFetch; });
  const calls = [], navigations = [], client = new AdminPlatformClient();
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return calls.length === 1
      ? new Response(JSON.stringify({ code: 'PAGINATION_CURSOR_EXPIRED' }), { status: 410 })
      : new Response(JSON.stringify({ items: [], next_cursor: '', snapshot_as_of: '2026-10-07T10:00:00Z' }));
  };
  const prefix = anomaly ? 'anomaly' : 'late';
  const route = { section: 'finance', search: '?' + prefix + '_q=synthetic-needle&' + prefix + '_severity=P2', navigate: (...args) => navigations.push(args) };
  const words = adminLayoutCopy[language][anomaly ? 'anomalyQueueResults' : 'lateReviewResults'];
  await ui.render(AdminPaymentReviewQueue, { client, copy: financeCopy[language], me: { user_id: '11111111-1111-4111-8111-111111111111', platform_permissions: ['platform.billing.orders.read'] }, route, anomaly, language });
  assert.equal(calls.length, 1);
  assert.equal(ui.document.querySelector('[role=alert]').textContent, words.cursorExpired);
  const recover = Array.from(ui.document.querySelectorAll('button')).find(node => node.textContent === words.firstBatch);
  assert.ok(recover && !recover.disabled);
  await ui.click(recover);
  assert.equal(calls.length, 2, 'one explicit first-batch recovery');
  assert.equal(calls[0].url, calls[1].url, 'retain all filters and empty cursor');
  assert.ok(calls.every(call => !call.options.method || call.options.method === 'GET'));
  assert.equal(navigations.length, 1);
  assert.equal(navigations[0][1][prefix + '_cursor'], null);
  assert.equal(navigations[0][1][prefix + '_q'], 'synthetic-needle');
  assert.equal(navigations[0][1][prefix + '_severity'], 'P2');
  assert.ok(!ui.document.querySelector('[role=alert]'));
});
