import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { createServer } from 'node:http';
import test from 'node:test';
import { AdminPlatformClient } from './admin-client.js';

async function withServer(handler, check) {
  const server = createServer(handler);
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    await check(new AdminPlatformClient(`http://127.0.0.1:${server.address().port}`));
  } finally {
    const closed = new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    server.closeAllConnections();
    await closed;
  }
}

function reply(response, status, body) {
  response.writeHead(status, { 'Content-Type': 'application/json' });
  response.end(JSON.stringify(body));
}

test('a session transport failure is an error and does not erase a known session', async () => {
  await withServer((_request, response) => reply(response, 503, { code: 'DEPENDENCY_UNAVAILABLE' }), async client => {
    const csrf = randomUUID();
    client.csrfToken = csrf;
    await assert.rejects(client.session(), error => error.status === 503 && error.code === 'DEPENDENCY_UNAVAILABLE');
    assert.equal(client.csrfToken, csrf);
  });
});

test('an expired session clears stale CSRF material', async () => {
  await withServer((_request, response) => reply(response, 401, { code: 'LOGIN_REQUIRED' }), async client => {
    client.csrfToken = randomUUID();
    assert.deepEqual(await client.session(), { authenticated: false });
    assert.equal(client.csrfToken, '');
  });
});

test('a forbidden administrative read remains a permission error', async () => {
  await withServer((_request, response) => reply(response, 403, { code: 'PERMISSION_DENIED' }), async client => {
    const csrf = randomUUID();
    client.csrfToken = csrf;
    await assert.rejects(client.accountOverview(), error => error.status === 403 && error.code === 'PERMISSION_DENIED');
    assert.equal(client.csrfToken, csrf);
  });
});

test('an expired administrative mutation clears stale CSRF material', async () => {
  let observed;
  await withServer((request, response) => {
    observed = { method: request.method, csrf: request.headers['x-csrf-token'], path: request.url };
    reply(response, 401, { code: 'LOGIN_REQUIRED' });
  }, async client => {
    const csrf = randomUUID();
    client.csrfToken = csrf;
    await assert.rejects(client.renameOrganizationUnit('organization/fixture', 'unit?fixture', 'Fixture'), error => error.status === 401);
    assert.deepEqual(observed, {
      method: 'PATCH', csrf,
      path: '/api/admin/account/admin/v1/organizations/organization%2Ffixture/units/unit%3Ffixture',
    });
    assert.equal(client.csrfToken, '');
  });
});

test('a failed logout preserves the current CSRF material and rejects', async () => {
  await withServer((_request, response) => reply(response, 503, { code: 'DEPENDENCY_UNAVAILABLE' }), async client => {
    const csrf = randomUUID();
    client.csrfToken = csrf;
    await assert.rejects(client.logout(), error => error.status === 503);
    assert.equal(client.csrfToken, csrf);
  });
});

test('a successful logout clears CSRF only after the server accepts it', async () => {
  let observed;
  await withServer((request, response) => {
    observed = { method: request.method, csrf: request.headers['x-csrf-token'], path: request.url };
    response.writeHead(204);
    response.end();
  }, async client => {
    const csrf = randomUUID();
    client.csrfToken = csrf;
    await client.logout();
    assert.deepEqual(observed, { method: 'POST', csrf, path: '/api/logout' });
    assert.equal(client.csrfToken, '');
  });
});

for (const operation of ['users', 'renameOrganization']) {
  test(`a truncated successful ${operation} response rejects rather than reporting success`, async () => {
    await withServer((_request, response) => {
      response.writeHead(200, { 'Content-Type': 'application/json' });
      response.end('{"items":');
    }, async client => {
      const request = operation === 'users' ? client.users('fixture') : client.renameOrganization('fixture', 'Fixture');
      await assert.rejects(request, error => error.code === 'REQUEST_FAILED' && error.status === 200);
    });
  });
}

test('an unexpected successful JSON scalar rejects and retains the known session', async () => {
  await withServer((_request, response) => reply(response, 200, null), async client => {
    const csrf = randomUUID();
    client.csrfToken = csrf;
    await assert.rejects(client.session(), error => error.code === 'REQUEST_FAILED');
    assert.equal(client.csrfToken, csrf);
  });
});

test('an invalid error response retains its HTTP error classification', async () => {
  await withServer((_request, response) => {
    response.writeHead(403);
    response.end('not json');
  }, async client => {
    await assert.rejects(client.users(''), error => error.code === 'REQUEST_FAILED' && error.status === 403);
  });
});

for (const [status, payload] of [[200, {}], [204, undefined], [200, { authenticated: true, csrf_token: null }], [200, { authenticated: false, csrf_token: 'fixture' }]]) {
  test(`an incomplete session contract (${status}, ${JSON.stringify(payload)}) rejects without erasing the known session`, async () => {
    await withServer((_request, response) => reply(response, status, payload), async client => {
      const csrf = randomUUID();
      client.csrfToken = csrf;
      await assert.rejects(client.session(), error => error.code === 'REQUEST_FAILED' && error.status === status);
      assert.equal(client.csrfToken, csrf);
    });
  });
}

test('a valid session replaces stale CSRF after validating the complete session contract', async () => {
  const csrf = randomUUID();
  await withServer((_request, response) => reply(response, 200, { authenticated: true, csrf_token: csrf }), async client => {
    const payload = await client.session();
    assert.equal(payload.authenticated, true);
    assert.equal(client.csrfToken, csrf);
  });
});
