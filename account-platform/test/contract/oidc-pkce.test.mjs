import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { createServer } from 'node:http';
import { once } from 'node:events';
import test from 'node:test';
import { authorizationCodeLogin } from '../integration/oidc-pkce.mjs';

const jwt = (claims) => `fixture.${Buffer.from(JSON.stringify(claims)).toString('base64url')}.fixture`;
async function fixture(t, mode = '', lifetimeOverrides = {}) {
  let authorization;
  let exchanges = 0;
  const server = createServer(async (req, res) => {
    const origin = `http://127.0.0.1:${server.address().port}`;
    const target = new URL(req.url, origin);
    if (target.pathname.endsWith('/auth')) {
      authorization = target.searchParams;
      assert.equal(authorization.get('response_type'), 'code');
      assert.equal(authorization.get('code_challenge_method'), 'S256');
      res.setHeader('Set-Cookie', 'fixture_session=opaque; Path=/realms/test/; Secure; HttpOnly');
      res.end(`<form id="kc-form-login" method="post" action="${mode === 'foreign' ? 'http://localhost:1/' : origin + '/realms/test/login-actions/authenticate?test=1&amp;test2=2'}"><input type="hidden" name="credentialId" value=""><input name="username"><input name="password" type="password"></form>`);
      return;
    }
    let raw = '';
    for await (const chunk of req) raw += chunk;
    const body = new URLSearchParams(raw);
    if (target.pathname.endsWith('/authenticate')) {
      assert.equal(req.headers.cookie, 'fixture_session=opaque');
      assert.equal(target.searchParams.get('test2'), '2');
      assert.equal(body.get('username'), 'fixture-user');
      assert.equal(body.get('password'), 'fixture-password');
      const callback = new URL(authorization.get('redirect_uri'));
      callback.search = new URLSearchParams({ code: 'fixture-code', state: mode === 'state' ? 'wrong' : authorization.get('state') });
      res.writeHead(302, { location: callback.href });
      res.end();
      return;
    }
    if (target.pathname.endsWith('/token')) {
      exchanges++;
      assert.equal(body.get('grant_type'), 'authorization_code');
      assert.equal(body.get('code'), 'fixture-code');
      assert.equal(createHash('sha256').update(body.get('code_verifier')).digest('base64url'), authorization.get('code_challenge'));
      const common = { iss: origin + '/realms/test', sub: 'fixture-subject', aud: 'fixture-client' };
      res.setHeader('Content-Type', 'application/json');
      // 1e999 is valid JSON number syntax that parses as Infinity. Preserve this
      // wire-level counterexample rather than JSON.stringify's conversion to null.
      const response = { access_token: jwt({ ...common, typ: 'Bearer', sid: 'fixture-session' }), id_token: jwt({ ...common, nonce: authorization.get('nonce') }), refresh_token: 'fixture-refresh', expires_in: 300, refresh_expires_in: 600, ...lifetimeOverrides };
      res.end(JSON.stringify(response, (_key, value) => value === Infinity ? 'fixture-infinite-number' : value).replaceAll('"fixture-infinite-number"', '1e999'));
      return;
    }
    res.writeHead(404).end();
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => new Promise((resolve) => server.close(resolve)));
  const options = { tokenURL: `http://127.0.0.1:${server.address().port}/realms/test/protocol/openid-connect/token`, clientID: 'fixture-client', username: 'fixture-user', password: 'fixture-password' };
  return { options, exchanges: () => exchanges };
}

test('PKCE protocol helper submits actual form, retains local cookie and exchanges verifier', async (t) => {
  const testFixture = await fixture(t);
  const tokens = await authorizationCodeLogin(testFixture.options);
  assert.equal(tokens.expires_in, 300);
  assert.equal(testFixture.exchanges(), 1);
});

test('PKCE protocol helper rejects wrong state before exchanging code', async (t) => {
  const testFixture = await fixture(t, 'state');
  await assert.rejects(authorizationCodeLogin(testFixture.options), /OIDC_TEST_CALLBACK_REJECTED/);
  assert.equal(testFixture.exchanges(), 0);
});

test('PKCE protocol helper refuses to send credentials to a foreign form action', async (t) => {
  const testFixture = await fixture(t, 'foreign');
  await assert.rejects(authorizationCodeLogin(testFixture.options), /OIDC_TEST_REDIRECT_OR_FORM_OUTSIDE_REALM/);
  assert.equal(testFixture.exchanges(), 0);
});

test('PKCE protocol helper rejects invalid provider lifetimes after actual code exchange', async (t) => {
  for (const field of ['expires_in', 'refresh_expires_in']) {
    for (const [name, value] of [['missing', undefined], ['nonnumeric', 'invalid'], ['numeric-string', '300'], ['null', null], ['boolean', true], ['zero', 0], ['negative', -1], ['nonfinite', Infinity]]) {
      await t.test(`${field}/${name}`, async (t) => {
        const testFixture = await fixture(t, '', { [field]: value });
        await assert.rejects(authorizationCodeLogin(testFixture.options), /OIDC_TEST_INVALID_TOKEN_RESPONSE/);
        assert.equal(testFixture.exchanges(), 1);
      });
    }
  }
});

test('PKCE protocol helper accepts positive provider lifetimes without a local cap', async (t) => {
  const testFixture = await fixture(t, '', { expires_in: 31_536_000, refresh_expires_in: 31_536_000 });
  const tokens = await authorizationCodeLogin(testFixture.options);
  assert.equal(tokens.expires_in, 31_536_000);
  assert.equal(tokens.refresh_expires_in, 31_536_000);
  assert.equal(testFixture.exchanges(), 1);
});
