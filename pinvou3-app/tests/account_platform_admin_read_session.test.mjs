import assert from 'node:assert/strict';
import test from 'node:test';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';

const deferred = () => {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
};
const json = (body, version = 1) => Response.json(body, {
  headers: { 'X-Context-Version': String(version), 'X-Platform-Authorization-Version': String(version) },
});
const aborted = error => error.name === 'AbortError';

async function withClient(run) {
  const original = globalThis.fetch;
  const client = new AdminPlatformClient();
  client.csrfToken = 'controlled-login-a';
  try { await run(client); } finally { globalThis.fetch = original; }
}

test('a replacement login cannot reuse an old users read or observe its late headers', () => withClient(async client => {
  const oldResponse = deferred();
  const currentResponse = deferred();
  const signals = [];
  globalThis.fetch = async (url, options) => {
    if (url === '/api/session') return Response.json({ authenticated: true, csrf_token: 'controlled-login-b' });
    signals.push(options.signal);
    return signals.length === 1 ? oldResponse.promise : currentResponse.promise;
  };
  const old = client.users();
  const oldRejected = assert.rejects(old, aborted);
  await client.session();
  assert.equal(signals[0].aborted, true);
  const current = client.users();
  assert.equal(signals.length, 2);
  oldResponse.resolve(json({ items: [{ user_id: 'old-user' }] }, 999));
  await oldRejected;
  // Old cleanup must not remove the new session's shared read.
  const shared = client.users();
  assert.equal(signals.length, 2);
  currentResponse.resolve(json({ items: [{ user_id: 'current-user' }] }));
  assert.deepEqual(await current, { items: [{ user_id: 'current-user' }] });
  assert.deepEqual(await shared, await current);
  assert.equal(client.authorizationVersions.platform_authorization_version, 1);
  assert.equal(client.inflightReads.size, 0);
}));

test('session replacement during body decoding rejects the old payload before observing headers', () => withClient(async client => {
  const reading = deferred();
  const body = deferred();
  globalThis.fetch = async url => {
    if (url === '/api/session') return Response.json({ authenticated: true, csrf_token: 'controlled-login-b' });
    const response = json({ items: [{ user_id: 'old-user' }] }, 999);
    const decode = response.json.bind(response);
    response.json = async () => { reading.resolve(); await body.promise; return decode(); };
    return response;
  };
  const pending = client.users();
  const rejected = assert.rejects(pending, aborted);
  await reading.promise;
  await client.session();
  body.resolve();
  await rejected;
  assert.deepEqual(client.authorizationVersions, {});
}));

test('same-session refresh preserves deduplication and one caller can abort independently', () => withClient(async client => {
  const response = deferred();
  let calls = 0;
  let signal;
  globalThis.fetch = async (url, options) => {
    if (url === '/api/session') return Response.json({ authenticated: true, csrf_token: 'controlled-login-a' });
    calls++; signal = options.signal; return response.promise;
  };
  const controller = new AbortController();
  const first = client.users('', controller.signal);
  const rejected = assert.rejects(first, aborted);
  await client.session();
  const second = client.users();
  controller.abort();
  await rejected;
  assert.equal(calls, 1);
  assert.equal(signal.aborted, false);
  response.resolve(json({ items: [] }, 3));
  assert.deepEqual(await second, { items: [] });
  assert.equal(client.authorizationVersions.platform_authorization_version, 3);
}));

for (const unauthenticated of [401, 200]) {
  test(`unauthenticated session (${unauthenticated}) retires pending reads and version observations`, () => withClient(async client => {
    const response = deferred();
    let signal;
    client.authorizationVersions = { context_version: 5, platform_authorization_version: 5 };
    globalThis.fetch = async (url, options) => {
      if (url === '/api/session') return Response.json({ authenticated: false }, { status: unauthenticated });
      signal = options.signal; return response.promise;
    };
    const pending = client.users();
    const rejected = assert.rejects(pending, aborted);
    assert.equal((await client.session()).authenticated, false);
    assert.equal(signal.aborted, true);
    assert.equal(client.csrfToken, '');
    assert.deepEqual(client.authorizationVersions, {});
    response.resolve(json({ items: [{ user_id: 'old-user' }] }, 999));
    await rejected;
    assert.deepEqual(client.authorizationVersions, {});
  }));
}

test('late session receipt cannot restore an earlier login', () => withClient(async client => {
  const response = deferred();
  let calls = 0;
  globalThis.fetch = async () => ++calls === 1 ? response.promise : Response.json({ authenticated: true, csrf_token: 'controlled-login-b' });
  const pending = client.session();
  const rejected = assert.rejects(pending, aborted);
  await client.session();
  response.resolve(Response.json({ authenticated: true, csrf_token: 'controlled-login-a' }));
  await rejected;
  assert.equal(client.csrfToken, 'controlled-login-b');
}));

test('logout retires ordinary reads even when a mock transport ignores abort', () => withClient(async client => {
  const response = deferred();
  globalThis.fetch = async url => url === '/api/logout' ? new Response(null, { status: 204 }) : response.promise;
  const pending = client.users();
  const rejected = assert.rejects(pending, aborted);
  await client.logout();
  assert.equal(client.inflightReads.size, 0);
  response.resolve(json({ items: [{ user_id: 'old-user' }] }, 999));
  await rejected;
  assert.deepEqual(client.authorizationVersions, {});
}));

test('late logout receipt cannot retire a newly discovered login', () => withClient(async client => {
  const response = deferred();
  globalThis.fetch = async url => url === '/api/logout' ? response.promise : Response.json({ authenticated: true, csrf_token: 'controlled-login-b' });
  const pending = client.logout();
  await client.session();
  response.resolve(new Response(null, { status: 204 }));
  await pending;
  assert.equal(client.csrfToken, 'controlled-login-b');
}));

test('a failed session refresh does not discard a valid in-flight read', () => withClient(async client => {
  const response = deferred();
  let signal;
  globalThis.fetch = async (url, options) => {
    if (url === '/api/session') return Response.json({ code: 'TEMPORARILY_UNAVAILABLE' }, { status: 503 });
    signal = options.signal; return response.promise;
  };
  const pending = client.users();
  await assert.rejects(client.session(), error => error.status === 503);
  assert.equal(signal.aborted, false);
  response.resolve(json({ items: [] }));
  assert.deepEqual(await pending, { items: [] });
}));

test('logout CSRF recovery confirmed unauthenticated also retires ordinary reads', () => withClient(async client => {
  const response = deferred();
  globalThis.fetch = async url => {
    if (url === '/api/logout') return Response.json({ code: 'CSRF_REJECTED' }, { status: 403 });
    if (url === '/api/session') return Response.json({ authenticated: false }, { status: 401 });
    return response.promise;
  };
  const pending = client.users();
  const rejected = assert.rejects(pending, aborted);
  await client.logout();
  response.resolve(json({ items: [{ user_id: 'old-user' }] }, 999));
  await rejected;
  assert.equal(client.csrfToken, '');
  assert.deepEqual(client.authorizationVersions, {});
}));

test('the last aborted consumer retires the read without removing a subsequent read', () => withClient(async client => {
  const old = deferred();
  const current = deferred();
  const signals = [];
  globalThis.fetch = async (_url, options) => { signals.push(options.signal); return signals.length === 1 ? old.promise : current.promise; };
  const controller = new AbortController();
  const first = client.users('', controller.signal);
  const rejected = assert.rejects(first, aborted);
  controller.abort();
  await rejected;
  assert.equal(signals[0].aborted, true);
  const second = client.users();
  old.resolve(json({ items: [{ user_id: 'abandoned-user' }] }, 999));
  // Run the retired fetch continuation and cleanup before joining its replacement.
  await new Promise(resolve => setImmediate(resolve));
  const third = client.users();
  assert.equal(signals.length, 2);
  current.resolve(json({ items: [] }));
  assert.deepEqual(await second, { items: [] });
  assert.deepEqual(await third, { items: [] });
  assert.equal(client.authorizationVersions.platform_authorization_version, 1);
}));

test('late mutation receipt preserves its result without poisoning the current session observations', () => withClient(async client => {
  const response = deferred();
  globalThis.fetch = async url => url === '/api/session' ? Response.json({ authenticated: true, csrf_token: 'controlled-login-b' }) : response.promise;
  const pending = client.mutate('account', '/controlled-command', 'POST', {});
  await client.session();
  response.resolve(json({ accepted: true }, 999));
  assert.deepEqual(await pending, { accepted: true });
  assert.deepEqual(client.authorizationVersions, {});
}));

test('late bootstrap cannot return an old actor to the current session', () => withClient(async client => {
  const response = deferred();
  globalThis.fetch = async url => url === '/api/session' ? Response.json({ authenticated: true, csrf_token: 'controlled-login-b' }) : response.promise;
  const pending = client.authorization();
  const rejected = assert.rejects(pending, aborted);
  await client.session();
  response.resolve(json({ user_id: 'old-user' }, 999));
  await rejected;
  assert.deepEqual(client.authorizationVersions, {});
}));
