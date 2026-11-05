import assert from 'node:assert/strict';
import test from 'node:test';
import { createServer } from 'node:http';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';
import {
  replayPricingMutation,
  pricingUnknownStorageKey,
} from '../../../pinvou3-app/src/features/account-platform/pricing-mutation.js';
import {
  createPricingUnknownRecoveryGate,
  archivePricingUnknown,
  readPricingUnknownHistory,
  readPricingUnknownConfiguration,
} from '../../../pinvou3-app/src/features/account-platform/pricing-unknown-recovery.js';
const operation = {
  phase: 'DISPATCHED',
  kind: 'DRAFT_CREATE',
  idempotencyKey: 'public-original-key',
  input: {
    scope_type: 'GLOBAL',
    protocol_code: 'CHAT',
    formula_version_id: '00000000-0000-4000-8000-000000000001',
    credit_unit: 'MICROCREDIT',
    expected_etag: 1,
    units: [],
  },
};
const key = pricingUnknownStorageKey('public-owner', 1);
function storage() {
  const map = new Map([[key, JSON.stringify(operation)]]);
  return {
    getItem: (k) => map.get(k) ?? null,
    setItem: (k, v) => map.set(k, v),
    removeItem: (k) => map.delete(k),
  };
}
test('preserving UNKNOWN retains immutable facts and appends a second unknown', () => {
  const store = storage();
  const history = archivePricingUnknown(key, operation, store);
  assert.deepEqual(history[0], { status: 'UNKNOWN', operation });
  assert.equal(store.getItem(key), null);
  const next = { ...operation, idempotencyKey: 'public-second-key' };
  store.setItem(key, JSON.stringify(next));
  archivePricingUnknown(key, next, store);
  assert.deepEqual(
    readPricingUnknownHistory(key, store).map((x) => x.operation.idempotencyKey),
    ['public-original-key', 'public-second-key'],
  );
  assert.equal(
    readPricingUnknownHistory(pricingUnknownStorageKey('other-owner', 1), store).length,
    0,
  );
});
for (const fault of ['getItem', 'setItem', 'removeItem'])
  test(`storage ${fault} failure cannot discard active UNKNOWN`, () => {
    const store = storage();
    const broken = {
      ...store,
      [fault]: () => {
        throw new Error('CONTROLLED_STORAGE_FAILURE');
      },
    };
    assert.throws(() => archivePricingUnknown(key, operation, broken));
    assert.equal(store.getItem(key), JSON.stringify(operation));
  });
for (const status of [412, 422])
  test(`real AdminClient replay ${status} retains key/body; read current namespace without write`, async () => {
    const calls = [];
    const server = createServer(async (req, res) => {
      let body = '';
      for await (const chunk of req) body += chunk;
      const u = new URL(req.url, 'http://fixture.invalid');
      calls.push({
        method: req.method,
        key: req.headers['idempotency-key'],
        body,
        query: Object.fromEntries(u.searchParams),
      });
      res.setHeader('Content-Type', 'application/json');
      if (req.method === 'POST') {
        res.writeHead(status);
        res.end(JSON.stringify({ code: 'CONTROLLED_REPLAY_CONFLICT' }));
      } else res.end(JSON.stringify({ items: [], has_more: false, current_space_etag: 2 }));
    });
    await new Promise((done) => server.listen(0, '127.0.0.1', done));
    try {
      const client = new AdminPlatformClient(`http://127.0.0.1:${server.address().port}`);
      await assert.rejects(() => replayPricingMutation(client, operation));
      const configuration = await readPricingUnknownConfiguration(client, operation);
      assert.equal(configuration.current_space_etag, '2');
      assert.equal(calls.filter((c) => c.method === 'POST').length, 1);
      assert.equal(calls[0].key, operation.idempotencyKey);
      assert.deepEqual(JSON.parse(calls[0].body), operation.input);
      assert.equal(calls[1].query.scope_type, 'GLOBAL');
      assert.equal(calls[1].query.formula_version_id, operation.input.formula_version_id);
      assert.equal(operation.idempotencyKey, 'public-original-key');
    } finally {
      await new Promise((done) => server.close(done));
    }
  });
test('current configuration incomplete or epoch-retired response fails closed', async () => {
  const original = globalThis.fetch;
  try {
    for (const payload of [
      { items: [], has_more: true, current_space_etag: 2 },
      { items: [], has_more: false, current_space_etag: 0 },
    ]) {
      globalThis.fetch = async () => new Response(JSON.stringify(payload));
      await assert.rejects(() =>
        readPricingUnknownConfiguration(new AdminPlatformClient(), operation),
      );
    }
    const client = new AdminPlatformClient();
    globalThis.fetch = async () => {
      client.retireSession();
      return new Response(JSON.stringify({ items: [], has_more: false, current_space_etag: 2 }));
    };
    await assert.rejects(() => readPricingUnknownConfiguration(client, operation));
  } finally {
    globalThis.fetch = original;
  }
});
test('actual target read precedes namespace read; permission failure and cancellation stay failures', async () => {
  const base = { ...operation, kind: 'DRAFT_UPDATE', targetID: 'public-original-target' };
  const original = globalThis.fetch;
  try {
    for (const status of [401, 403, 404]) {
      globalThis.fetch = async () =>
        new Response(JSON.stringify({ code: 'CONTROLLED_READ_DENIED' }), { status });
      await assert.rejects(() => readPricingUnknownConfiguration(new AdminPlatformClient(), base));
    }
    const paths = [];
    globalThis.fetch = async (url) => {
      paths.push(String(url));
      return new Response(
        JSON.stringify(
          paths.length === 1
            ? { item: { ...operation.input, price_version_id: base.targetID } }
            : { items: [], has_more: false, current_space_etag: 3 },
        ),
      );
    };
    await readPricingUnknownConfiguration(new AdminPlatformClient(), base);
    assert.ok(paths[0].includes('price_version_id=public-original-target'));
    assert.ok(paths[1].includes('scope_type=GLOBAL'));
    const controller = new AbortController();
    controller.abort();
    await assert.rejects(() =>
      readPricingUnknownConfiguration(new AdminPlatformClient(), base, controller.signal),
    );
  } finally {
    globalThis.fetch = original;
  }
});
test('legacy active single-operation identity is unchanged after unsuccessful archive and retry', () => {
  const store = storage();
  const broken = {
    ...store,
    removeItem: () => {
      throw new Error('CONTROLLED_REMOVE_FAILURE');
    },
  };
  assert.throws(() => archivePricingUnknown(key, operation, broken));
  assert.deepEqual(JSON.parse(store.getItem(key)), operation);
  assert.deepEqual(readPricingUnknownHistory(key, store), [{ status: 'UNKNOWN', operation }]);
  assert.equal(archivePricingUnknown(key, operation, store).length, 1);
  assert.deepEqual(readPricingUnknownHistory(key, store)[0].operation, operation);
});

test('held configuration before actual same-key503 replay can never requalify; fresh read required', async () => {
  const original = globalThis.fetch;
  const gate = createPricingUnknownRecoveryGate();
  const client = new AdminPlatformClient();
  let release;
  let reached;
  const started = new Promise((resolve) => {
    reached = resolve;
  });
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({
      url: String(url),
      method: options.method || 'GET',
      body: options.body,
      key: options.headers['Idempotency-Key'],
    });
    if (options.method === 'POST')
      return new Response(JSON.stringify({ code: 'CONTROLLED_UNKNOWN_REPLAY' }), { status: 503 });
    reached();
    // Deliberately complete despite any transport cancellation: intent ordering
    // must reject this pre-replay snapshot independently of AbortController.
    return new Promise((resolve) => {
      release = () =>
        resolve(
          new Response(JSON.stringify({ items: [], has_more: false, current_space_etag: 2 })),
        );
    });
  };
  try {
    const oldTicket = gate.beginRead();
    const pending = readPricingUnknownConfiguration(client, operation);
    await started;
    const replayTicket = gate.beginReplay();
    assert.throws(() => gate.beginRead(), /REPLAY_IN_PROGRESS/);
    await assert.rejects(() => replayPricingMutation(client, operation));
    gate.endReplay(replayTicket);
    release();
    await pending;
    assert.equal(
      gate.readIsCurrent(oldTicket),
      false,
      'a pre-replay read must not restore archive authority after503',
    );
    const write = calls.find((call) => call.method === 'POST');
    assert.deepEqual(JSON.parse(write.body), operation.input);
    assert.equal(write.key, operation.idempotencyKey);
    const freshTicket = gate.beginRead();
    const fresh = readPricingUnknownConfiguration(client, operation);
    await Promise.resolve();
    release();
    await fresh;
    assert.equal(gate.readIsCurrent(freshTicket), true);
  } finally {
    globalThis.fetch = original;
  }
});
