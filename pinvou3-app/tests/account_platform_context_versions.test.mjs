import assert from 'node:assert/strict';
import test from 'node:test';
import { mergeObservedVersions } from '../src/features/account-platform/authorization-ui.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';

const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; };
const versions = (context, organization, personal = 3, platform = 4) => ({ context_version: context, organization_authorization_version: organization, personal_authorization_version: personal, platform_authorization_version: platform });
const response = (body = {}, value = {}) => new Response(JSON.stringify(body), { status: 200, headers: Object.fromEntries([
  ['X-Context-Version', value.context_version], ['X-Organization-Authorization-Version', value.organization_authorization_version],
  ['X-Personal-Authorization-Version', value.personal_authorization_version], ['X-Platform-Authorization-Version', value.platform_authorization_version],
].filter(([, item]) => item !== undefined).map(([key, item]) => [key, String(item)])) });
const nativeResponse = (body = {}, value = {}) => ({ status: 200, body, contextVersion: value.context_version, organizationAuthorizationVersion: value.organization_authorization_version, personalAuthorizationVersion: value.personal_authorization_version, platformAuthorizationVersion: value.platform_authorization_version });
const A = '11111111-1111-4111-8111-111111111111';
const B = '22222222-2222-4222-8222-222222222222';

test('new context replaces the previous organizations larger counter', () => {
  assert.deepEqual(mergeObservedVersions(versions(1, 99), versions(2, 1)), versions(2, 1));
});
test('new personal context removes an absent organization counter', () => {
  assert.deepEqual(mergeObservedVersions(versions(1, 99), { context_version: 2 }), { context_version: 2, personal_authorization_version: 3, platform_authorization_version: 4 });
});
test('old context cannot advance any current authorization counter', () => {
  assert.deepEqual(mergeObservedVersions(versions(2, 1), versions(1, 999, 999, 999)), versions(2, 1));
});
test('same-context counters retain monotonic observations and partial headers', () => {
  assert.deepEqual(mergeObservedVersions(versions(2, 9), versions(2, 1, 4, 2)), versions(2, 9, 4, 4));
  assert.deepEqual(mergeObservedVersions(versions(2, 9), { personal_authorization_version: 5 }), versions(2, 9, 5, 4));
});
test('organization counter without its positive context cannot update observations', () => {
  for (const context_version of [undefined, 0, -1, 'invalid', 1.5]) {
    assert.deepEqual(mergeObservedVersions(versions(2, 9), { context_version, organization_authorization_version: 999 }), versions(2, 9));
  }
  assert.deepEqual(mergeObservedVersions({}, { organization_authorization_version: 99 }), {});
});

async function withClient(surface, action) {
  const original = globalThis.fetch;
  const harness = { send: async () => ({ body: {}, versions: {} }), logout: async () => null, session: async () => ({ authenticated: true, csrf_token: 'login-a' }), login: async () => ({ authenticated: true }) };
  const client = surface === 'BFF' ? new AccountPlatformBffClient() : new AccountPlatformDesktopClient({
    request: async input => { const result = await harness.send(input.resource); return nativeResponse(result.body, result.versions); },
    logout: () => harness.logout(), session: () => harness.session(), login: () => harness.login(), loginSms: () => harness.login(),
  });
  if (surface === 'BFF') {
    client.csrfToken = 'login-a';
    globalThis.fetch = async url => {
      if (url === '/api/logout') { await harness.logout(); return new Response(null, { status: 204 }); }
      if (url === '/api/session') return response(await harness.session());
      const result = await harness.send(url.replace('/api/account', ''));
      return response(result.body, result.versions);
    };
  }
  try { await action(client, harness); } finally { globalThis.fetch = original; }
}

for (const surface of ['BFF', 'Desktop']) {
  test(`${surface} actual response path separates organizations and ignores late context headers`, () => withClient(surface, async (client, harness) => {
    const receipts = [versions(1, 99), versions(2, 1), versions(1, 999, 999, 999)];
    harness.send = async () => ({ body: {}, versions: receipts.shift() });
    const observed = []; client.onAuthorizationVersion(value => observed.push(value));
    await client.request('account', '/a'); await client.request('account', '/b'); await client.request('account', '/late-a');
    assert.deepEqual(client.authorizationVersions, versions(2, 1));
    assert.equal(client.contextVersion, 2); assert.equal(observed.length, 2);
  }));

  test(`${surface} logout resets counters and rejects a prior request after a new login`, () => withClient(surface, async (client, harness) => {
    harness.send = async () => ({ body: {}, versions: versions(10, 99) });
    await client.request('account', '/initial');
    const old = deferred(); harness.send = () => old.promise;
    const pending = client.request('account', '/old-login');
    await client.logout();
    assert.equal(client.contextVersion, 0); assert.deepEqual(client.authorizationVersions, {});
    if (surface === 'BFF') { harness.session = async () => ({ authenticated: true, csrf_token: 'login-b' }); await client.session(); }
    else await client.login('fixture-next-user', 'fixture-password');
    harness.send = async () => ({ body: {}, versions: versions(1, 1) });
    await client.request('account', '/current-login');
    old.resolve({ body: {}, versions: versions(100, 999) }); await pending;
    assert.deepEqual(client.authorizationVersions, versions(1, 1)); assert.equal(client.contextVersion, 1);
  }));

  test(`${surface} actual me identity change retires the previous users pending observation`, () => withClient(surface, async (client, harness) => {
    harness.send = async () => ({ body: { user_id: A }, versions: versions(10, 99) });
    await client.request('account', '/v1/me');
    const old = deferred(); harness.send = () => old.promise;
    const pending = client.request('account', '/old-actor');
    harness.send = async () => ({ body: { user_id: B }, versions: versions(1, 1) });
    await client.request('account', '/v1/me');
    old.resolve({ body: {}, versions: versions(100, 999) }); await pending;
    assert.equal(client.versionObservationUserID, B);
    assert.deepEqual(client.authorizationVersions, versions(1, 1)); assert.equal(client.contextVersion, 1);
  }));

  test(`${surface} first me binding retires requests dispatched before the actor was known`, () => withClient(surface, async (client, harness) => {
    const old = deferred(); harness.send = () => old.promise;
    const pending = client.request('account', '/unbound-old-actor');
    harness.send = async () => ({ body: { user_id: B }, versions: versions(1, 1) });
    await client.request('account', '/v1/me');
    old.resolve({ body: {}, versions: versions(100, 999) }); await pending;
    assert.equal(client.versionObservationUserID, B);
    assert.deepEqual(client.authorizationVersions, versions(1, 1)); assert.equal(client.contextVersion, 1);
  }));
}

test('ordinary same-CSRF session refresh preserves current observations and pending response', () => withClient('BFF', async (client, harness) => {
  harness.send = async () => ({ body: {}, versions: versions(1, 2) }); await client.request('account', '/initial');
  const old = deferred(); harness.send = () => old.promise; const pending = client.request('account', '/same-login');
  const epoch = client.versionObservationEpoch; await client.session(); assert.equal(client.versionObservationEpoch, epoch);
  old.resolve({ body: {}, versions: versions(1, 3) }); await pending;
  assert.deepEqual(client.authorizationVersions, versions(1, 3));
}));

test('changed-CSRF session resets observed counters without logout', () => withClient('BFF', async (client, harness) => {
  harness.send = async () => ({ body: {}, versions: versions(10, 99) }); await client.request('account', '/initial');
  harness.session = async () => ({ authenticated: true, csrf_token: 'login-b' }); await client.session();
  assert.equal(client.contextVersion, 0); assert.deepEqual(client.authorizationVersions, {});
}));

test('late BFF session completion cannot restore the previous login or its counters', () => withClient('BFF', async (client, harness) => {
  const old = deferred(); harness.session = () => old.promise; const pending = client.session();
  harness.session = async () => ({ authenticated: true, csrf_token: 'login-b' }); await client.session();
  harness.send = async () => ({ body: {}, versions: versions(1, 1) }); await client.request('account', '/new-login');
  old.resolve({ authenticated: true, csrf_token: 'login-a' }); await pending;
  assert.equal(client.csrfToken, 'login-b'); assert.deepEqual(client.authorizationVersions, versions(1, 1));
}));

test('late BFF logout acknowledgement does not clear a newer browser login', () => withClient('BFF', async (client, harness) => {
  const old = deferred(); harness.logout = () => old.promise; const pending = client.logout();
  harness.session = async () => ({ authenticated: true, csrf_token: 'login-b' }); await client.session();
  harness.send = async () => ({ body: {}, versions: versions(1, 1) }); await client.request('account', '/new-login');
  old.resolve(); await pending;
  assert.equal(client.csrfToken, 'login-b'); assert.deepEqual(client.authorizationVersions, versions(1, 1));
}));

test('desktop failed password login retires old in-flight version observations', () => withClient('Desktop', async (client, harness) => {
  const old = deferred(); harness.send = () => old.promise; const pending = client.request('account', '/old-login');
  harness.login = async () => { throw new Error('invalid_credentials'); };
  await assert.rejects(client.login('fixture', 'fixture'), /invalid_credentials/);
  old.resolve({ body: {}, versions: versions(100, 999) }); await pending;
  assert.equal(client.contextVersion, 0); assert.deepEqual(client.authorizationVersions, {});
}));

test('desktop SMS login retires old in-flight version observations', () => withClient('Desktop', async (client, harness) => {
  const old = deferred(); harness.send = () => old.promise; const pending = client.request('account', '/old-login');
  await client.loginSms('fixture-mobile', 'fixture-challenge', 'fixture-code');
  old.resolve({ body: {}, versions: versions(100, 999) }); await pending;
  assert.equal(client.contextVersion, 0); assert.deepEqual(client.authorizationVersions, {}); assert.equal(client.financeSessionEpoch, 1);
}));

test('desktop unauthenticated session clears counters and retires prior responses', () => withClient('Desktop', async (client, harness) => {
  await client.session();
  harness.send = async () => ({ body: {}, versions: versions(10, 99) }); await client.request('account', '/initial');
  const old = deferred(); harness.send = () => old.promise; const pending = client.request('account', '/old-login');
  harness.session = async () => ({ authenticated: false }); await client.session();
  old.resolve({ body: {}, versions: versions(100, 999) }); await pending;
  assert.equal(client.contextVersion, 0); assert.deepEqual(client.authorizationVersions, {});
}));

test('late desktop logout acknowledgement does not retire a newer successful login', () => withClient('Desktop', async (client, harness) => {
  const old = deferred(); harness.logout = () => old.promise; const pending = client.logout();
  await client.login('fixture', 'fixture');
  harness.send = async () => ({ body: {}, versions: versions(1, 1) }); await client.request('account', '/new-login');
  old.resolve(); await pending;
  assert.equal(client.observedSessionAuthenticated, true); assert.deepEqual(client.authorizationVersions, versions(1, 1));
}));
