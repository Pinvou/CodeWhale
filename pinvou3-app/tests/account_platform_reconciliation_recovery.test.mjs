import test from 'node:test';
import assert from 'node:assert/strict';
import React, { act } from 'react';
import domino from '@mixmark-io/domino';
import { useFinanceOperation } from '../src/features/account-platform/useFinanceOperation.js';
import { reconciliationRepairHost } from '../src/features/account-platform/reconciliation-repair-context.js';
import { financialRepairDraftFromProjection } from '../src/features/account-platform/admin-financial-repair-form.js';

const CASE_B = '00000000-0000-4000-8000-000000000002';
const CASE_C = '00000000-0000-4000-8000-000000000003';
const OWNER = 'controlled-actor:1';
const COPY = { unknown: 'Unconfirmed', saved: 'Saved', permissions: 'Denied', sourceChanged: 'Changed' };
const unknownResult = () => Promise.reject(Object.assign(new Error('controlled unknown result'), { code: 'COMMAND_RESULT_UNKNOWN', status: 503 }));
// A producer-shaped closed owner projection is passed through the actual draft
// builder. The action is controlled; no financial owner or DB execution is claimed.
const frozen = (caseID, key) => {
  const projection = {
    command_type: 'RECONCILIATION_FINANCIAL_REPAIR', target_resource_type: 'RECONCILIATION_CASE', target_resource_id: caseID,
    policy_version: 1, snapshot_schema_version: 1,
    normalized_command: { reconciliation_case_id: caseID, payment_evidence_id: '00000000-0000-4000-8000-000000000004', business_date: '2026-10-09', resolution_code: 'PROVIDER_REFUND_CONFIRMED' },
    critical_resource_versions: { run_id: '00000000-0000-4000-8000-000000000005', case_id: caseID, provider_snapshot_digest: 'b'.repeat(64), facts_digest: 'a'.repeat(64), policy_version: 1 },
    financial_snapshot: { amount_fen: '9007199254740993', currency: 'CNY', provider_transaction_id: 'controlled-transaction', ledger_effect: 'NONE' },
  };
  const prepared = financialRepairDraftFromProjection(projection, 'Controlled recovery facts', key.slice('approval:'.length));
  assert.ok(prepared.draft, 'controlled projection must satisfy the actual closed draft contract');
  return { ...prepared, projection };
};
const manualEntry = copy => ({ name: 'manual', key: 'legacy-manual', host: reconciliationRepairHost(), copy });
const caseEntry = (id, scopeOverride) => ({ name: id, key: id, host: scopeOverride ? { ...reconciliationRepairHost(id), scope: scopeOverride } : reconciliationRepairHost(id) });

// Real React/Fiber and the production hook. Only external action completion is
// controlled. This in-memory DOM has no browser, layout, HTTP listener or DB.
async function withHookHost(action) {
  const window = domino.createWindow('<!doctype html><html><body></body></html>');
  const names = ['window', 'document', 'navigator', 'HTMLElement', 'Node', 'IS_REACT_ACT_ENVIRONMENT'];
  const previous = new Map(names.map(name => [name, Object.getOwnPropertyDescriptor(globalThis, name)]));
  const replacements = { window, document: window.document, navigator: window.navigator, HTMLElement: window.HTMLElement, Node: window.Node, IS_REACT_ACT_ENVIRONMENT: true };
  const latest = new Map(), tasks = [], held = [];
  let root;
  const client = { financeSessionEpoch: 0 };
  function Probe({ entry, currentClient, owner }) {
    const operation = useFinanceOperation(owner, entry.copy || COPY, currentClient, entry.host.scope);
    latest.set(entry.name, operation);
    return React.createElement('div', { 'data-host': entry.name }, operation.unknown ? 'unknown' : 'ready');
  }
  try {
    for (const name of names) Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: replacements[name] });
    const { createRoot } = await import('react-dom/client');
    const container = window.document.createElement('main');
    window.document.body.appendChild(container);
    root = createRoot(container);
    const harness = {
      client,
      current: name => { assert.ok(latest.has(name), 'probe was rendered'); assert.ok(container.querySelector('[data-host="' + name + '"]'), 'probe is currently mounted'); return latest.get(name); },
      async render(entries, currentClient = client, owner = OWNER) {
        await act(async () => { root.render(React.createElement(React.Fragment, null, ...entries.map(entry => React.createElement(Probe, { key: entry.key, entry, currentClient, owner })))); });
      },
      defer() {
        let resolve;
        const promise = new Promise(accept => { resolve = accept; });
        const item = { promise, resolve }; held.push(item); return item;
      },
      async start(name, facts, external) {
        let promise;
        await act(async () => { promise = latest.get(name).mutate(facts, external); tasks.push(promise); void promise.catch(() => {}); });
        return { promise };
      },
      async run(name, facts, external) {
        await act(async () => { const promise = latest.get(name).mutate(facts, external); tasks.push(promise); await promise; });
      },
      async finish(invocation, deferred, result = { confirmed: true }) {
        await act(async () => { deferred.resolve(result); await invocation.promise; });
      },
    };
    await action(harness);
  } finally {
    // Settle every owned completion even if an assertion fails, then retire the
    // actual effects before restoring the process-local DOM globals.
    try {
      if (root) await act(async () => {
        for (const item of held) item.resolve({ cleanup: true });
        await Promise.allSettled(tasks);
        root.unmount();
      });
    } finally {
      latest.clear();
      for (const name of names) {
        const descriptor = previous.get(name);
        if (descriptor) Object.defineProperty(globalThis, name, descriptor); else delete globalThis[name];
      }
    }
  }
}

async function lateManualCompletion(scopeOverride, check) {
  await withHookHost(async h => {
    const entries = [manualEntry(), caseEntry(CASE_B, scopeOverride)];
    await h.render(entries); // Both instances initialize before either invokes.
    const held = h.defer();
    const manual = await h.start('manual', frozen('00000000-0000-4000-8000-000000000001', 'approval:manual'), () => held.promise);
    const original = frozen(CASE_B, 'approval:case-b');
    await h.run(CASE_B, original, unknownResult);
    assert.equal(h.current(CASE_B).unknown, true);
    const key = h.current(CASE_B).pendingKey;
    await h.finish(manual, held);
    await h.render([manualEntry()]);
    await h.render(entries);
    await check(h, original, key);
  });
}

test('Old shared-scope fault input exposes lost case recovery after a late peer success', { concurrency: false, timeout: 10000 }, async () => {
  // Explicit old caller configuration; no product hook/Map algorithm is mocked.
  await lateManualCompletion('financial-repair:RECONCILIATION_FINANCIAL_REPAIR', async h => {
    assert.equal(h.current(CASE_B).unknown, false);
    assert.equal(h.current(CASE_B).pendingFacts, null);
  });
});

test('Stable manual and case hosts keep the exact unknown intent after peer success and remount', { concurrency: false, timeout: 10000 }, async () => {
  await lateManualCompletion(undefined, async (h, original, key) => {
    assert.equal(h.current(CASE_B).unknown, true);
    assert.strictEqual(h.current(CASE_B).pendingFacts, original);
    assert.equal(h.current(CASE_B).pendingKey, key);
    assert.equal(h.current(CASE_B).pendingFacts.draft.idempotency_key, 'approval:case-b');
    await h.run(CASE_B, frozen(CASE_C, 'approval:must-not-replace'), async (facts, replayKey) => {
      assert.strictEqual(facts, original); assert.equal(replayKey, key); return { confirmed: true };
    });
    assert.equal(h.current(CASE_B).unknown, false);
  });
});

test('Retired case completion cannot remove another case or the same-case reentry intent', { concurrency: false, timeout: 10000 }, async () => {
  await withHookHost(async h => {
    const originalB = frozen(CASE_B, 'approval:original-b'), originalC = frozen(CASE_C, 'approval:original-c');
    await h.render([caseEntry(CASE_B)]);
    const held = h.defer();
    const oldB = await h.start(CASE_B, originalB, () => held.promise);
    const keyB = h.current(CASE_B).pendingKey;
    await h.render([caseEntry(CASE_C)]);
    await h.run(CASE_C, originalC, unknownResult);
    const keyC = h.current(CASE_C).pendingKey;
    await h.render([caseEntry(CASE_B)]);
    assert.strictEqual(h.current(CASE_B).pendingFacts, originalB);
    await h.finish(oldB, held); // Wait for actual old hook settlement, not a delay.
    assert.equal(h.current(CASE_B).unknown, true); assert.equal(h.current(CASE_B).pendingKey, keyB);
    await h.render([caseEntry(CASE_C)]);
    assert.strictEqual(h.current(CASE_C).pendingFacts, originalC); assert.equal(h.current(CASE_C).pendingKey, keyC);
    await h.render([caseEntry(CASE_B)]);
    await h.run(CASE_B, {}, async (facts, key) => { assert.strictEqual(facts, originalB); assert.equal(key, keyB); return { confirmed: true }; });
    await h.render([caseEntry(CASE_C)]);
    assert.equal(h.current(CASE_C).unknown, true); assert.strictEqual(h.current(CASE_C).pendingFacts, originalC);
  });
});

test('Legacy manual intent survives remount and copy changes while actor, epoch and client stay isolated', { concurrency: false, timeout: 10000 }, async () => {
  await withHookHost(async h => {
    const original = frozen(CASE_B, 'approval:legacy-original');
    await h.render([manualEntry()]); await h.run('manual', original, unknownResult);
    const key = h.current('manual').pendingKey;
    await h.render([]); await h.render([manualEntry({ ...COPY, unknown: '未確認' })]);
    assert.equal(h.current('manual').unknown, true); assert.strictEqual(h.current('manual').pendingFacts, original); assert.equal(h.current('manual').pendingKey, key);
    await h.render([manualEntry()], h.client, 'another-controlled-actor:1');
    assert.equal(h.current('manual').pendingFacts, null);
    h.client.financeSessionEpoch = 1;
    await h.render([manualEntry()]); assert.equal(h.current('manual').pendingFacts, null);
    await h.render([manualEntry()], { financeSessionEpoch: 0 }); assert.equal(h.current('manual').pendingFacts, null);
  });
});
