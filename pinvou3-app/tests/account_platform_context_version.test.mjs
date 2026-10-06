import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import { createContextVersionTracker } from '../src/features/account-platform/context-version.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';

test('versions are scoped to a user and login epoch, including authorization-only changes', () => {
  const tracker = createContextVersionTracker();
  const events = [];
  const unsubscribe = tracker.subscribe((context, authorization) => events.push([context, authorization]));
  assert.equal(tracker.bindUser('user-a', tracker.epoch), true);
  const oldEpoch = tracker.epoch;
  tracker.observe('7', '11', oldEpoch);
  tracker.observe('6', '12', oldEpoch);
  assert.deepEqual(events, [[7, 11]]);
  for (const invalid of ['1e2', '1.5', 'Infinity', '9007199254740993', null, true]) tracker.observe(invalid, invalid, oldEpoch);
  assert.deepEqual(events, [[7, 11]]);
  tracker.reset();
  assert.equal(tracker.bindUser('user-b', tracker.epoch), true);
  tracker.observe('200', '200', oldEpoch);
  assert.equal(tracker.contextVersion, 0);
  tracker.observe('1', '2', tracker.epoch);
  assert.deepEqual(events.at(-1), [1, 2]);
  unsubscribe();
  tracker.observe('2', '3', tracker.epoch);
  assert.equal(events.length, 2);
});

test('real BFF HTTP transports global PUT CAS and versions without leaking old-user response state', {timeout:10000}, async () => {
  let user = 'user-a';
  let version = 8;
  let releaseOld;
  let oldRequestArrived;
  const arrival = new Promise(resolve => { oldRequestArrived = resolve; });
  const requests = [];
  const server = createServer(async (request, response) => {
    let text = '';
    for await (const chunk of request) text += chunk;
    requests.push({ method: request.method, path: request.url, body: text ? JSON.parse(text) : null });
    const write = (body, context = version) => {
      response.writeHead(200, { 'Content-Type': 'application/json', 'X-Context-Version': String(context), 'X-Authorization-Version': '4' });
      response.end(JSON.stringify(body));
    };
    if (request.url === '/api/account/v1/account/bootstrap') write({ user_id: user, active_context: { context_version: version } });
    else if (request.url.startsWith('/api/token/v1/me/balance?')) { releaseOld = () => write({ available_microcredits: '100' }, 99); oldRequestArrived(); }
    else if (request.url === '/api/logout') { response.writeHead(204); response.end(); }
    else if (request.url === '/api/account/v1/me/active-context') write({ context_type: 'ORGANIZATION', organization_id: 'organization-a', context_version: ++version });
    else { response.writeHead(404); response.end(); }
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  try {
    const client = new AccountPlatformBffClient(`http://127.0.0.1:${server.address().port}`);
    const events = [];
    client.onContextVersion((context, authorization) => events.push([context, authorization]));
    await client.bootstrap();
    assert.equal(client.contextVersion, 8);
    await client.switchContext('ORGANIZATION', 'organization-a', 8);
    assert.deepEqual(requests.at(-1), { method: 'PUT', path: '/api/account/v1/me/active-context', body: { context_type: 'ORGANIZATION', organization_id: 'organization-a', expected_context_version: 8 } });
    const oldRequest = client.balance();
    await arrival;
    await client.logout();
    user = 'user-b'; version = 1;
    await client.bootstrap();
    releaseOld();
    await oldRequest;
    assert.equal(client.contextVersion, 1);
    assert.deepEqual(events.at(-1), [1, 4]);
  } finally {
    server.closeAllConnections();
    await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
  }
});

test('native client keeps version response metadata out of business payload and rejects late login observations', async () => {
  let releaseOld;
  let currentUser = 'user-a';
  const requests = [];
  const client = new AccountPlatformDesktopClient({
    async request(request) {
      requests.push(request);
      if (request.resource.endsWith('/bootstrap')) return { status: 200, body: { user_id: currentUser }, contextVersion: currentUser === 'user-a' ? 7 : 1, authorizationVersion: 2 };
      if (request.resource.includes('/balance')) return new Promise(resolve => { releaseOld = resolve; });
      return { status: 200, body: { context_version: 8 }, contextVersion: 8, authorizationVersion: 3 };
    },
    async login() { return { enabled: true, configured: true, authenticated: true }; },
  });
  await client.bootstrap();
  const pending = client.balance();
  await client.login('fixture-user', 'fixture-password');
  currentUser = 'user-b';
  await client.bootstrap();
  releaseOld({ status: 200, body: { available_microcredits: '100' }, contextVersion: 99, authorizationVersion: 99 });
  await pending;
  assert.equal(client.contextVersion, 1);
  const result = await client.switchContext('PERSONAL', null, 1);
  assert.deepEqual(result, { context_version: 8 });
  assert.equal(requests.at(-1).method, 'PUT');
  assert.deepEqual(requests.at(-1).body, { context_type: 'PERSONAL', organization_id: null, expected_context_version: 1 });
});


test('real late BFF session responses cannot clear a newer user or replace its CSRF', {timeout:10000}, async () => {
  for (const status of [401, 200]) {
    let release;
    let arrived;
    const arrival = new Promise(resolve => { arrived=resolve; });
    const server = createServer((request,response)=>{
      release=()=>{ response.writeHead(status,{'Content-Type':'application/json'});response.end(JSON.stringify({authenticated:true,csrf_token:'old-controlled-csrf'})); };
      arrived();
    });
    server.listen(0,'127.0.0.1');await once(server,'listening');
    const client=new AccountPlatformBffClient(`http://127.0.0.1:${server.address().port}`);
    try {
      const late=client.session();const rejected=assert.rejects(late,{name:'AbortError'});await arrival;
      client.contextVersions.reset();
      client.contextVersions.bindUser('new-controlled-user',client.contextVersions.epoch);
      client.observeContextVersion(1,2);client.csrfToken='new-controlled-csrf';
      const reply=release;release=null;reply();await rejected;
      assert.equal(client.csrfToken,'new-controlled-csrf');assert.equal(client.contextVersion,1);
    } finally { release?.();server.closeAllConnections();await new Promise(resolve=>server.close(resolve)); }
  }
});


test('real BFF responses bind authorization versions to the current global context', {timeout:10000}, async () => {
  let context = 7;
  let authorization = 100;
  const server = createServer((request, response) => {
    response.writeHead(200, {'Content-Type':'application/json', 'X-Context-Version':String(context), 'X-Authorization-Version':String(authorization)});
    response.end(JSON.stringify(request.url.endsWith('/bootstrap') ? {user_id:'user-a',active_context:{context_version:context,authorization_version:authorization}} : {items:[]}));
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  try {
    const client = new AccountPlatformBffClient(`http://127.0.0.1:${server.address().port}`);
    const events = [];
    client.onContextVersion((version, auth) => events.push([version,auth]));
    await client.bootstrap();
    context = 8; authorization = 1;
    await client.bootstrap();
    authorization = 2;
    await client.organizations();
    authorization = 1;
    await client.organizations();
    context = 7; authorization = 200;
    await client.organizations();
    assert.deepEqual(events, [[7,100],[8,1],[8,2],[8,1]]);
    assert.equal(client.contextVersion,8);
  } finally {
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
  }
});
