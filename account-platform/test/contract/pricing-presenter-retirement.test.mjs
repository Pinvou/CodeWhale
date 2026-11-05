import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';
import * as state from '../../../pinvou3-app/src/features/account-platform/pricing-unknown-state.js';
import * as recovery from '../../../pinvou3-app/src/features/account-platform/pricing-unknown-recovery.js';
import * as mutation from '../../../pinvou3-app/src/features/account-platform/pricing-mutation.js';
import * as lifecycle from '../../../pinvou3-app/src/features/account-platform/pricing-lifecycle.js';
import { integerMoney } from '../../../pinvou3-app/src/features/account-platform/finance-contract.js';
import { secureRequestID } from '../../../pinvou3-app/src/features/account-platform/request-id.js';

// Execute the production presenter/controller body, including its actual handlers
// and state setters. This scheduler caches memo/callback dependencies and can
// defer functional updaters; passive effects run only when explicitly enabled.
// It is not a DOM/React render test.
const source = await readFile(new URL('../../../pinvou3-app/src/features/account-platform/PricingLifecycleWorkspace.jsx', import.meta.url), 'utf8');
const begin = source.indexOf('export function PricingLifecycleWorkspace(');
const end = source.indexOf('  // Hide every previous-owner presenter', begin);
assert.ok(begin >= 0 && end > begin);
const body = source.slice(begin, end).replace('export function', 'function');
const constants = source.slice(source.indexOf('const mutationKey'), source.indexOf('function PricingStatus'));
const expose = 'return { editor, unitForm, pendingAction, editorErrors, errors, formulaPage, formulaAccessDenied, formulaAuthority, loadProtocols, unknownConfiguration, unknownMutation, setEditor, setUnitForm, setPendingAction, setProtocols, openEdit, openCreate, openTransition, submitDraft, submitUnit, runPendingAction, reloadUnknownConfiguration, startAfterUnknown, verifyUnknown }; }';
function presenter(client, owner, { effects = false } = {}) {
  let index = 0, defer = false, dirty = false;
  const slots = [], queued = [], pendingEffects = [];
  const sameDeps = (a, b) => a && b && a.length === b.length && a.every((v, i) => Object.is(v, b[i]));
  const hooks = {
    useState(initial) {
      const slot = index++;
      if (!(slot in slots)) slots[slot] = typeof initial === 'function' ? initial() : initial;
      const set = value => {
        const apply = () => { const next = typeof value === 'function' ? value(slots[slot]) : value; if (!Object.is(next, slots[slot])) dirty = true; slots[slot] = next; };
        if (defer) queued.push(apply); else apply();
      };
      return [slots[slot], set];
    },
    useRef(initial) { const slot = index++; slots[slot] ??= { current: initial }; return slots[slot]; },
    useMemo(fn, deps) {
      const slot = index++, old = slots[slot];
      if (!old || !sameDeps(deps, old.deps)) slots[slot] = { deps, value: fn() };
      return slots[slot].value;
    },
    useCallback(fn, deps) {
      const slot = index++, old = slots[slot];
      if (!old || !sameDeps(deps, old.deps)) slots[slot] = { deps, value: fn };
      return slots[slot].value;
    },
    useEffect(fn, deps) {
      const slot = index++, old = slots[slot];
      if (effects && (!old || !sameDeps(deps, old.deps))) {
        slots[slot] = { deps, cleanup: old?.cleanup };
        pendingEffects.push(() => { old?.cleanup?.(); slots[slot].cleanup = fn(); });
      }
    },
    useSyncExternalStore: (_subscribe, get) => get(),
  };
  const render = new Function('scope', `with (scope) { ${constants} ${body} ${expose}; return PricingLifecycleWorkspace; }`)({ ...hooks, ...state, ...recovery, ...mutation, ...lifecycle, integerMoney, secureRequestID });
  const labels = { errors: {}, actionFailed: 'controlled failure', permissionLost: 'permission lost' };
  const driver = {
    render() { index = 0; return render({ client, owner, permissions: ['platform.pricing.manage'], copy: { billing: { pricingLifecycle: labels } } }); },
    setOwner(next) { owner = next; },
    async settle() {
      for (let turn = 0; turn < 20; turn++) {
        dirty = false;
        const view = driver.render();
        for (const effect of pendingEffects.splice(0)) effect();
        await new Promise(resolve => setImmediate(resolve));
        if (!dirty && !pendingEffects.length) return view;
      }
      throw new Error('PRESENTER_EFFECT_LOOP');
    },
    defer() { defer = true; },
    flush() { defer = false; for (const apply of queued.splice(0)) apply(); },
  };
  return driver;
}
const id = '00000000-0000-4000-8000-000000000001';
const item = { price_version_id: id, scope_type: 'GLOBAL', protocol_code: 'CHAT', formula_version_id: id, credit_unit: 'MICROCREDIT', lifecycle_status: 'DRAFT', derived_status: 'DRAFT', space_etag: 1, effective_from: '2026-10-30T00:00:00+08:00', units: [{ unit_definition_id: id, config_type: 'FIXED_PRICE', fixed_price_per_denominator: '1' }] };
const formula = { formula_version_id: id, protocol_code: 'CHAT', status: 'PUBLISHED', version: 1, required_unit_versions: { TOKEN: id } };
const unitForm = { unit_code: 'TOKEN', minimum_precision: '1', price_denominator: '1', rounding_rule: 'CEILING' };
const action = { type: 'PUBLISH', item, idempotencyKey: 'original-confirmation-key', error: '' };
const event = { preventDefault() {} };
function seed(p) { const view = p.render(); view.setProtocols({ formulas: [formula] }); view.setEditor(lifecycle.priceDraftForm(item)); view.setUnitForm(unitForm); view.setPendingAction(action); return p.render(); }

test('peer UNKNOWN completion retires production form handlers before passive effects; fresh selection remains required', async () => {
  const originalFetch = globalThis.fetch, originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'sessionStorage');
  const memory = new Map();
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, value: { getItem: key => memory.get(key) ?? null, setItem: (key, value) => memory.set(key, value), removeItem: key => memory.delete(key) } });
  const writes = [];
  let status = 503;
  let holdProtocol = false, holdSpace = false;
  const held = [];
  globalThis.fetch = async (url, options = {}) => {
    if (options.method && options.method !== 'GET') {
      writes.push({ body: options.body, key: options.headers['Idempotency-Key'] });
      return new Response(JSON.stringify(status === 200 ? item : { code: 'CONTROLLED_FAILURE' }), { status });
    }
    const u = String(url);
    if (holdProtocol && u.endsWith('/admin/v1/protocols')) {
      holdProtocol = false;
      return new Promise(resolve => held.push(() => resolve(new Response(JSON.stringify({ protocols: [], formulas: [formula] })))));
    }
    if (holdSpace && u.includes('scope_type=')) {
      holdSpace = false;
      return new Promise(resolve => held.push(() => resolve(new Response(JSON.stringify({ items: [], has_more: false, current_space_etag: 2 })))));
    }
    return new Response(JSON.stringify(u.includes('price_version_id=') ? { item } : u.includes('pricing-protocol') ? { formulas: [formula] } : { items: [], has_more: false, current_space_etag: 2 }));
  };
  try {
    const client = new AdminPlatformClient();
    for (const completion of ['archive', 'recovery']) {
      const owner = `public-peer-${completion}`;
      const key = mutation.pricingUnknownStorageKey(owner, client.financeSessionEpoch);
      const a = presenter(client, owner), b = presenter(client, owner);
      let av = seed(a); seed(b);
      const c = presenter(new AdminPlatformClient(), owner), d = presenter(new AdminPlatformClient(), owner);
      holdProtocol = true;
      const lateEdit = c.render().openEdit(item);
      holdSpace = true;
      const lateTransition = d.render().openTransition({ ...item, derived_status: 'ACTIVE' });
      assert.equal(held.length, 2, 'both real AdminClient reads are held before UNKNOWN');
      status = 503;
      await av.submitDraft(event);
      const operation = state.pricingUnknownSnapshot(key);
      assert.ok(operation?.idempotencyKey);
      assert.equal(writes.at(-1).key, operation.idempotencyKey);
      av = a.render();
      let bv = b.render();
      if (completion === 'archive') { await bv.reloadUnknownConfiguration(); bv = b.render(); assert.ok(bv.unknownConfiguration); }
      // A's queued updater and C/D's already-started reads cannot restore intent
      // after B clears it, even when no passive effects run anywhere.
      a.defer(); av.setEditor(current => ({ ...current, effective_from: '2026-11-01' }));

      if (completion === 'archive') bv.startAfterUnknown();
      else { status = 200; await bv.verifyUnknown(); }
      assert.equal(state.pricingUnknownSnapshot(key), null);
      const count = writes.length;
      await av.submitDraft(event); await av.submitUnit(event); await av.runPendingAction(event);
      assert.equal(writes.length, count, 'old same-frame closures cannot dispatch a new key');
      for (const release of held.splice(0)) release();
      await Promise.all([lateEdit, lateTransition]); a.flush();
      assert.equal(c.render().editor, null, 'late formula lookup cannot reopen an editor');
      assert.equal(d.render().pendingAction, null, 'late transition lookup cannot reopen confirmation');
      av.setEditor(lifecycle.priceDraftForm(item)); av.setUnitForm(unitForm); av.setPendingAction(action);
      for (const p of [a, b]) {
        const next = p.render();
        assert.equal(next.editor, null); assert.equal(next.unitForm, null); assert.equal(next.pendingAction, null);
      }
      if (completion === 'archive') assert.deepEqual(recovery.readPricingUnknownHistory(key)[0].operation, operation);
      // Explicit re-selection grants a fresh intent; ordinary initial 422 must
      // preserve user input/errors and must not retire another pre-open form.
      let fresh = a.render(); await fresh.openEdit(item); fresh = a.render(); assert.ok(fresh.editor);
      const peer = b.render(); peer.setUnitForm(unitForm);
      for (const deniedStatus of [412, 422]) {
        status = deniedStatus; await fresh.submitDraft(event);
        fresh = a.render(); assert.ok(fresh.editor); assert.ok(fresh.editorErrors.form);
        assert.deepEqual(b.render().unitForm, unitForm);
        assert.equal(state.pricingUnknownSnapshot(key), null);
      }
      status = 200; await fresh.submitDraft(event);
      assert.equal(a.render().editor, null, 'ordinary success is a validated acknowledgement');
      assert.equal(state.pricingUnknownSnapshot(key), null);
      assert.deepEqual(b.render().unitForm, unitForm, 'ordinary success does not retire unrelated intent');
    }
  } finally {
    globalThis.fetch = originalFetch;
    if (originalStorage) Object.defineProperty(globalThis, 'sessionStorage', originalStorage); else delete globalThis.sessionStorage;
    lifecycle.pricingLifecycleMutationLock.release();
  }
});

test('terminal revision obeys CAS, owner/session isolation and recovery 412/422 semantics', async () => {
  const key = 'public-retirement-cas', other = 'public-other-owner';
  const operation = { phase: 'DISPATCHED', kind: 'UNIT_DEACTIVATE', targetID: id, idempotencyKey: 'same-original-key', input: {} };
  state.storePricingUnknown(key, operation);
  const before = state.pricingUnknownStateSnapshot(key).revision;
  assert.equal(state.storePricingUnknown(key, null, {}, { retireIntents: true }), false);
  assert.equal(state.pricingUnknownStateSnapshot(key).revision, before);
  state.storePricingUnknown(key, null, operation);
  assert.equal(state.pricingUnknownStateSnapshot(key).revision, before, 'ordinary pending completion is not UNKNOWN retirement');
  for (const status of [412, 422]) {
    state.storePricingUnknown(key, operation);
    // Recovery errors retain the original active operation and credentials.
    const p = presenter({ financeSessionEpoch: 1, deactivateMeteringUnit: async () => { throw { status }; } }, `public-conflict-${status}`);
    const view = p.render();
    const scoped = mutation.pricingUnknownStorageKey(`public-conflict-${status}`, 1);
    state.storePricingUnknown(scoped, operation);
    await view.verifyUnknown();
    assert.deepEqual(state.pricingUnknownSnapshot(scoped), operation);
    assert.equal(state.pricingUnknownStateSnapshot(scoped).revision, 0);
  }
  const otherRevision = state.pricingUnknownStateSnapshot(other).revision;
  state.storePricingUnknown(key, null, operation, { retireIntents: true });
  assert.equal(state.pricingUnknownStateSnapshot(key).revision, before + 1);
  assert.equal(state.pricingUnknownStateSnapshot(other).revision, otherRevision);
});

test('snapshot filter denial after UNKNOWN retirement closes new editor despite memoization; success cannot revive it', async () => {
  let denied = false, writes = 0, reads = 0;
  // Normalized DTO of the supported Token pagination client. These are the
  // visible Filter/Restart controls: aggregate mode deliberately hides them.
  const snapshot = { protocols: [], formulas: [formula], formula_page: { mode: 'snapshot', has_more: false, next_cursor: null } };
  const client = {
    financeSessionEpoch: 1,
    pricingProtocols: async () => { reads++; if (denied) throw { status: 403 }; return snapshot; },
    managedPrices: async () => ({ items: [], has_more: false }),
    meteringUnits: async () => ({ items: [] }),
    deactivateMeteringUnit: async () => {},
    updatePriceDraft: async () => { writes++; },
  };
  const owner = 'public-snapshot-memo', p = presenter(client, owner);
  let v = seed(p);
  await v.loadProtocols({ q: 'CHAT' }); v = p.render();
  assert.equal(v.formulaPage.mode, 'snapshot');
  assert.match(source, /!contextual && formulaPage\.mode !== 'aggregate'/);
  assert.equal(v.loadProtocols, p.render().loadProtocols, 'unchanged dependencies really cache the callback');
  const key = mutation.pricingUnknownStorageKey(owner, 1);
  const original = { phase: 'DISPATCHED', kind: 'UNIT_DEACTIVATE', targetID: id, idempotencyKey: 'original-snapshot-recovery', input: {} };
  state.storePricingUnknown(key, original);
  await p.render().verifyUnknown();
  v = p.render();
  await v.openEdit(item); v = p.render(); assert.ok(v.editor);
  denied = true;
  await assert.rejects(() => v.loadProtocols({ q: 'CHAT' }));
  v = p.render(); assert.equal(v.formulaAccessDenied, true);
  assert.equal(v.editor, null, 'a current snapshot Filter/Restart403 must close an ordinary editor after recovery');
  denied = false; await v.loadProtocols({ q: 'CHAT' }); v = p.render();
  assert.equal(v.formulaAccessDenied, false); assert.equal(v.editor, null);
  await v.submitDraft(event); assert.equal(writes, 0, 'successful retry cannot revive the rejected edit intent');
  await v.openEdit(item); v = p.render(); await v.submitDraft(event);
  assert.equal(writes, 1, 'explicit new selection remains usable');
  assert.equal(reads, 3);
});

test('revision dependency refresh settles once and preserves an already dispatched operation with original facts', async () => {
  let reads = 0, release, updateStarted;
  const started = new Promise(resolve => { updateStarted = resolve; });
  const pending = new Promise(resolve => { release = resolve; });
  const snapshot = { protocols: [], formulas: [formula], formula_page: { mode: 'snapshot', has_more: false, next_cursor: null } };
  const client = {
    financeSessionEpoch: 1,
    pricingProtocols: async () => { reads++; return snapshot; },
    managedPrices: async () => ({ items: [], has_more: false }), meteringUnits: async () => ({ items: [] }),
    deactivateMeteringUnit: async () => {},
    updatePriceDraft: async () => { updateStarted(); await pending; throw { status: 503 }; },
  };
  const owner = 'public-effect-refresh', p = presenter(client, owner, { effects: true });
  let v = await p.settle(); assert.equal(reads, 1);
  const initial = v.loadProtocols;
  const key = mutation.pricingUnknownStorageKey(owner, 1);
  state.storePricingUnknown(key, { phase: 'DISPATCHED', kind: 'UNIT_DEACTIVATE', targetID: id, idempotencyKey: 'original-before-refresh', input: {} });
  await p.render().verifyUnknown();
  // Select/dispatch before the newly eligible passive load effect has run.
  v = p.render(); assert.notEqual(v.loadProtocols, initial);
  await v.openEdit(item); v = p.render();
  const writing = v.submitDraft(event); await started;
  const dispatched = state.pricingUnknownSnapshot(key);
  assert.ok(dispatched?.idempotencyKey); const facts = JSON.stringify(dispatched);
  await p.settle(); assert.equal(reads, 2, 'revision causes exactly one new ordinary load');
  assert.equal(state.pricingUnknownSnapshot(key), dispatched);
  assert.equal(JSON.stringify(dispatched), facts);
  release(); await writing;
  assert.equal(state.pricingUnknownSnapshot(key), dispatched, '503 retains the original accepted command');
  const stable = await p.settle(); const callback = stable.loadProtocols;
  await p.settle(); assert.equal(reads, 2, 'state updates do not loop the load effect');
  assert.equal(p.render().loadProtocols, callback);
  assert.equal(JSON.stringify(state.pricingUnknownSnapshot(key)), facts);
});

test('cached formula denial follows owner and session scope changes', async () => {
  let denied = false;
  const client = { financeSessionEpoch: 1,
    pricingProtocols: async () => { if (denied) throw { status: 403 }; return { protocols: [], formulas: [formula], formula_page: { mode: 'snapshot', has_more: false, next_cursor: null } }; },
    managedPrices: async () => ({ items: [], has_more: false }), meteringUnits: async () => ({ items: [] }),
  };
  const p = presenter(client, 'public-first-owner', { effects: true });
  let v = await p.settle();
  for (const change of [() => p.setOwner('public-second-owner'), () => { client.financeSessionEpoch++; }]) {
    const prior = v.loadProtocols;
    change(); v = await p.settle(); assert.notEqual(v.loadProtocols, prior);
    await v.openEdit(item); v = p.render(); assert.ok(v.editor);
    denied = true; await assert.rejects(() => v.loadProtocols({ q: 'CHAT' }));
    v = p.render(); assert.equal(v.editor, null);
    denied = false; await v.loadProtocols({ q: 'CHAT' }); v = await p.settle();
    assert.equal(v.editor, null, 'scope-rebound callback cannot revive the rejected form');
  }
});
