// Actual Token handler/DTO fixture bodies; the controlled HTTP boundary applies
// the existing exact UUID/protocol-code filter and page/rate contracts.
import assert from 'node:assert/strict';
import test from 'node:test';
import { createServer } from 'node:http';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
const workspace = fileURLToPath(new URL('../../../', import.meta.url));
const feature = resolve(workspace, 'pinvou3-app/src/features/account-platform');
const { AdminPlatformClient } = await import(pathToFileURL(resolve(feature, 'admin-client.js')));
const { publishedPricingFormulas, buildPriceDraftInput, priceDraftForm } = await import(
  pathToFileURL(resolve(feature, 'pricing-lifecycle.js'))
);
const producer = JSON.parse(
  readFileSync(new URL('./fixtures/pricing-producer-pages.json', import.meta.url), 'utf8'),
);
const first = producer['formula-page-one'].body.items[0],
  second = producer['formula-page-two'].body.items[0];
async function fixture(
  run,
  { legacy = false, nextStatus = 200, malformed = false, delay = false, retire = false, large = false } = {},
) {
  const calls = [];
  let nextStarted;
  const reachedNext = new Promise((done) => (nextStarted = done));
  const rows = large
    ? Array.from({ length: 3001 }, (_, i) => ({
        ...first,
        formula_version_id: `00000000-0000-4000-8000-${String(i + 1).padStart(12, '0')}`,
        version: i + 1,
      }))
    : null;
  const buckets = new Map();
  const server = createServer(async (req, res) => {
    const u = new URL(req.url, 'http://public.example'),
      resource = u.pathname.split('/').at(-1);
    calls.push({ resource, query: Object.fromEntries(u.searchParams) });
    const reply = (body, status = 200, headers = {}) => {
      if (!res.destroyed) {
        res.writeHead(status, { 'Content-Type': 'application/json', ...headers });
        res.end(JSON.stringify(body));
      }
    };
    if (resource === 'protocols')
      return reply(
        producer[legacy ? 'legacy-aggregate' : 'protocol-page-one'].body,
        200,
        retire ? { 'X-Platform-Authorization-Version': '1' } : {},
      );
    if (resource !== 'formulas') return reply({ code: 'UNEXPECTED_ROUTE' }, 404);
    assert.equal(u.searchParams.get('limit'), '50');
    assert.equal(u.searchParams.get('status'), 'PUBLISHED');
    const cursor = u.searchParams.get('cursor');
    if (cursor) {
      nextStarted();
      if (delay) await new Promise((done) => req.once('close', done));
      if (nextStatus !== 200)
        return reply(
          {
            code:
              nextStatus === 429
                ? 'RATE_LIMITED'
                : nextStatus === 410
                  ? 'CURSOR_EXPIRED'
                  : 'PERMISSION_DENIED',
          },
          nextStatus,
          { 'Retry-After': '1' },
        );
    }
    let page;
    const q = u.searchParams.get('q');
    if (large) {
      const key = Math.floor(Date.now() / 60000);
      const count = (buckets.get(key) || 0) + 1;
      buckets.set(key, count);
      if (count > 60) return reply({ code: 'RATE_LIMITED' }, 429, { 'Retry-After': '1' });
      const filtered = q
        ? rows.filter((x) => x.formula_version_id === q || x.protocol_code === q.toUpperCase())
        : rows;
      const start = Number(cursor || 0),
        end = Math.min(start + 50, filtered.length);
      page = {
        ...producer['formula-page-one'].body,
        items: filtered.slice(start, end),
        has_more: end < filtered.length,
        next_cursor: end < filtered.length ? String(end) : null,
      };
    } else if (q) {
      page = {
        ...producer['formula-page-two'].body,
        items: [first, second].filter(
          (x) => x.formula_version_id === q || x.protocol_code === q.toUpperCase(),
        ),
      };
    } else page = producer[cursor ? 'formula-page-two' : 'formula-page-one'].body;
    if (malformed) page = { ...page, has_more: true, next_cursor: cursor || null };
    reply(page, 200, retire ? { 'X-Platform-Authorization-Version': '1' } : {});
  });
  await new Promise((done) => server.listen(0, '127.0.0.1', done));
  const client = new AdminPlatformClient(`http://127.0.0.1:${server.address().port}`);
  if (retire) client.onAuthorizationVersion(() => client.retireSession());
  try {
    await run(client, calls, reachedNext);
  } finally {
    server.closeAllConnections();
    await new Promise((done) => server.close(done));
  }
}
test('pricing first and explicit next page preserve the actual producer and exact draft input', async () => {
  await fixture(async (client, calls) => {
    const page = await client.pricingProtocols();
    assert.equal(page.formula_page.has_more, true);
    assert.equal(calls.length, 2);
    assert.equal(page.formulas.length, 1);
    const next = await client.pricingProtocols({ cursor: page.formula_page.next_cursor });
    assert.equal(next.formula_page.has_more, false);
    assert.equal(calls.length, 3);
    assert.equal(calls.filter((x) => x.resource === 'protocols').length, 1);
    const formula = publishedPricingFormulas(next)[0];
    assert.equal(formula.formula_version_id, second.formula_version_id);
    const form = {
      ...priceDraftForm(),
      scope_type: 'GLOBAL',
      formula_version_id: formula.formula_version_id,
      effective_from: '2099-01-01',
      units: { '00000000-0000-4000-8000-000000000401': { fixed_price_per_denominator: '9007199254740993' } },
    };
    const result = buildPriceDraftInput(form, formula, '9007199254740993');
    assert.deepEqual(result.errors, {});
    assert.equal(result.input.expected_etag, '9007199254740993');
    assert.equal(result.input.units[0].fixed_price_per_denominator, '9007199254740993');
  });
});
test('current disabled-pagination aggregate is preserved without client-side catalogue filtering', async () => {
  await fixture(
    async (client, calls) => {
      assert.equal(publishedPricingFormulas(await client.pricingProtocols()).length, 2);
      const page = await client.pricingProtocols({ q: second.formula_version_id });
      assert.equal(page.formulas.length, 2);
      assert.equal(page.formula_page.mode, 'aggregate');
      assert.equal(page.formula_page.has_more, false);
      assert.equal(calls.filter((x) => x.resource === 'formulas').length, 0);
    },
    { legacy: true },
  );
});
test('429, expired cursor and permission denial reject only the requested page with original cursor retained', async () => {
  for (const status of [429, 410, 403])
    await fixture(
      async (client, calls) => {
        const firstPage = await client.pricingProtocols();
        const filters = { cursor: firstPage.formula_page.next_cursor };
        await assert.rejects(client.pricingProtocols(filters), (e) => e.status === status);
        await assert.rejects(client.pricingProtocols(filters), (e) => e.status === status);
        assert.equal(firstPage.formulas[0].formula_version_id, first.formula_version_id);
        assert.equal(firstPage.formula_page.has_more, true);
        assert.deepEqual(
          calls.slice(-2).map((x) => x.query.cursor),
          [filters.cursor, filters.cursor],
        );
        assert.equal(calls.filter((x) => x.resource === 'protocols').length, 1);
      },
      { nextStatus: status },
    );
});
test('malformed first and repeated continuation cursors fail closed without automatic scanning', async () => {
  await fixture(
    async (client) =>
      assert.rejects(client.pricingProtocols(), (e) => e.code === 'INVALID_PRICING_FORMULAS_RESPONSE'),
    { malformed: true },
  );
  await fixture(
    async (client) =>
      assert.rejects(
        client.pricingProtocols({ cursor: 'public-cursor' }),
        (e) => e.code === 'INVALID_PRICING_FORMULAS_RESPONSE',
      ),
    { malformed: true },
  );
});
test('aborting an explicit continuation cancels that actual HTTP request', async () => {
  await fixture(
    async (client, calls, reached) => {
      const controller = new AbortController();
      const pending = client.pricingProtocols({ cursor: 'public-fixture-cursor-two' }, controller.signal);
      await reached;
      controller.abort();
      await assert.rejects(pending, (e) => e.name === 'AbortError');
      assert.equal(calls.length, 1);
    },
    { delay: true },
  );
});
test('session retirement after capability response blocks formula publication and continuation', async () => {
  await fixture(
    async (client, calls) => {
      await assert.rejects(client.pricingProtocols(), (e) => e.name === 'AbortError');
      assert.equal(calls.length, 1);
    },
    { retire: true },
  );
});
test('exact UUID query locates an existing later formula without a fictitious detail endpoint', async () => {
  await fixture(async (client, calls) => {
    const page = await client.pricingProtocols({ q: second.formula_version_id });
    assert.equal(page.formula_page.has_more, false);
    assert.equal(page.formulas[0].formula_version_id, second.formula_version_id);
    assert.equal(calls.length, 2);
    assert.equal(calls[1].query.q, second.formula_version_id);
    assert.ok(calls.every((x) => ['protocols', 'formulas'].includes(x.resource)));
  });
});
test('3001 formula catalogue uses one page, then one user-selected page under the real60/min policy', async () => {
  await fixture(
    async (client, calls) => {
      const page = await client.pricingProtocols();
      assert.equal(page.formulas.length, 50);
      assert.equal(page.formula_page.has_more, true);
      assert.equal(calls.filter((x) => x.resource === 'formulas').length, 1);
      const next = await client.pricingProtocols({ cursor: page.formula_page.next_cursor });
      assert.equal(next.formulas.length, 50);
      assert.equal(next.formula_page.has_more, true);
      assert.equal(calls.filter((x) => x.resource === 'formulas').length, 2);
      assert.equal(calls.filter((x) => x.resource === 'protocols').length, 1);
      const target = '00000000-0000-4000-8000-000000003001';
      const exact = await client.pricingProtocols({ q: target });
      assert.equal(exact.formulas[0].formula_version_id, target);
      assert.equal(exact.formula_page.has_more, false);
      assert.equal(calls.filter((x) => x.resource === 'formulas').length, 3);
    },
    { large: true },
  );
});
