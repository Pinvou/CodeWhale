import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import test from 'node:test';
import { navigateFixture } from './helpers/navigate-fixture.mjs';

const url = 'http://127.0.0.1:43210/tests/fixture.html';
const failure = (overrides = {}) => ({
  failure: () => ({ errorText: 'net::ERR_NETWORK_CHANGED' }),
  method: () => 'GET', resourceType: () => 'script', url: () => url,
  ...overrides,
});
function pageWithFailures(failures) {
  const page = new EventEmitter();
  page.visits = 0;
  page.waitConditions = [];
  page.goto = async (_url, { waitUntil }) => {
    page.waitConditions.push(waitUntil);
    const request = failures[page.visits++];
    if (request) page.emit('requestfailed', request);
  };
  return page;
}
const options = { settleMs: 0, warn: () => {} };

test('interrupted module GET restarts fixture load before a case acts', async () => {
  const page = pageWithFailures([failure()]);
  let ready = 0;
  await navigateFixture(page, url, signal => {
    signal.throwIfAborted();
    ready++;
  }, { ...options, waitUntil: 'networkidle0' });
  assert.equal(page.visits, 2);
  assert.deepEqual(page.waitConditions, ['networkidle0', 'networkidle0']);
  assert.equal(ready, 1);
  assert.equal(page.listenerCount('requestfailed'), 0);
});

test('an interruption during readiness cancels its wait and retries', async () => {
  const page = pageWithFailures([]);
  let aborted = false;
  await navigateFixture(page, url, signal => {
    if (page.visits > 1) return;
    return new Promise((_, reject) => {
      signal.addEventListener('abort', () => { aborted = true; reject(signal.reason); }, { once: true });
      queueMicrotask(() => page.emit('requestfailed', failure()));
    });
  }, options);
  assert.equal(aborted, true);
  assert.equal(page.visits, 2);
});

test('continued network changes fail within three attempts and remove listeners', async () => {
  const page = pageWithFailures([failure(), failure(), failure()]);
  await assert.rejects(navigateFixture(page, url, signal => signal.throwIfAborted(), options));
  assert.equal(page.visits, 3);
  assert.equal(page.listenerCount('requestfailed'), 0);
});

for (const [name, request] of [
  ['application POST', failure({ method: () => 'POST' })],
  ['application fetch', failure({ resourceType: () => 'fetch' })],
  ['external script', failure({ url: () => 'https://example.org/script.js' })],
  ['connection refused', failure({ failure: () => ({ errorText: 'net::ERR_CONNECTION_REFUSED' }) })],
]) test(`${name} does not replay navigation or mask readiness failure`, async () => {
  const page = pageWithFailures([request]);
  const broken = new Error('fixture did not load');
  await assert.rejects(navigateFixture(page, url, () => { throw broken; }, options), error => error === broken);
  assert.equal(page.visits, 1);
  assert.equal(page.listenerCount('requestfailed'), 0);
});

test('product assertion/JavaScript failures are never retried', async () => {
  const page = pageWithFailures([]);
  const regression = new Error('React regression');
  await assert.rejects(navigateFixture(page, url, () => { throw regression; }, options), error => error === regression);
  assert.equal(page.visits, 1);
});

test('non-loopback destinations fail before navigation', async () => {
  const page = pageWithFailures([]);
  await assert.rejects(navigateFixture(page, 'https://example.org/', () => {}, options), /loopback/);
  assert.equal(page.visits, 0);
});
