import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import test from 'node:test';
import { credentialOriginPairs, validateCredentialEdge } from '../../../scripts/account-platform-credential-edge.mjs';

const script = await readFile(new URL('../../internal/portal/credential-handoff.v1.js', import.meta.url), 'utf8');
const baseline = JSON.parse(await readFile(new URL('../../deploy/nginx/credential-edge.example.json', import.meta.url), 'utf8'));

// Execute the actual pinned source, not a reimplementation of its origin check.
// Controlled VM proves the protocol branch and POST target, not browser TLS or DOM security.
async function executeHandoff(portalOrigin, credentialOrigin) {
  const facts = { ready: [], acknowledgments: [], posts: [], attached: 0, closed: 0 };
  const listeners = new Map();
  const opener = { postMessage: (message, target) => facts.ready.push({ message, target }) };
  const window = {
    opener, setTimeout: () => 1, clearTimeout() {},
    addEventListener: (name, callback) => listeners.set(name, callback),
    removeEventListener: name => listeners.delete(name),
    close: () => facts.closed++,
  };
  const document = {
    querySelector: () => ({ content: credentialOrigin }),
    body: { append: () => facts.attached++ },
    createElement: tag => tag === 'input' ? {} : {
      append(input) { this.input = input; },
      submit() {
        assert.equal(window.opener, null);
        facts.posts.push({ action: this.action, method: this.method, target: this.target, enctype: this.enctype, name: this.input.name, value: this.input.value });
      },
    },
  };
  vm.runInNewContext(script, { URL, window, document, location: new URL('/credential-handoff', portalOrigin), navigator: { serviceWorker: { controller: null, getRegistrations: async () => [] } } }, { timeout: 1000 });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(facts.ready.length, 1);
  assert.equal(facts.ready[0].message.type, 'HANDOFF_READY');
  assert.equal(facts.ready[0].target, portalOrigin);
  const port = { start() {}, close() {}, postMessage: message => facts.acknowledgments.push(message.type) };
  const nonce = 'a'.repeat(43);
  listeners.get('message')({ source: opener, origin: portalOrigin, ports: [port], data: { version: 1, type: 'HANDOFF_BIND', nonce } });
  assert.deepEqual(facts.acknowledgments, ['HANDOFF_BOUND']);
  port.onmessage({ currentTarget: port, data: { version: 1, type: 'HANDOFF_SUBMIT', nonce, launch_id: '0199a200-0001-7000-8000-000000000201' } });
  return facts;
}

test('actual handoff source submits once only to each renamed paired credential origin', async () => {
  for (const pair of [...Object.values(credentialOriginPairs), { portal: 'http://127.0.0.1:8080', credential: 'http://127.0.0.1:8099' }, { portal: 'http://localhost:8080', credential: 'http://localhost:8099' }]) {
    const facts = await executeHandoff(pair.portal, pair.credential);
    assert.deepEqual(facts.acknowledgments, ['HANDOFF_BOUND', 'HANDOFF_ACCEPTED']);
    assert.deepEqual(facts.posts, [{ action: `${pair.credential}/launch`, method: 'post', target: '_self', enctype: 'application/x-www-form-urlencoded', name: 'launch_id', value: '0199a200-0001-7000-8000-000000000201' }]);
    assert.equal(facts.attached, 1); assert.equal(facts.closed, 0);
  }
});

test('actual handoff rejects retired origins, cross pairs, aliases and production loopback downgrade with zero POST', async () => {
  for (const pair of Object.values(credentialOriginPairs)) {
    const other = Object.values(credentialOriginPairs).find(item => item.portal !== pair.portal);
    const host = new URL(pair.credential).hostname;
    for (const rejected of ['https://credentials.account.pinvou.com', 'https://credentials-uat.account.pinvou.com', other.credential, `${pair.credential}:443`, `${pair.credential}:8443`, `${pair.credential}.`, `${pair.credential}.evil.example`, `${pair.credential}/`, `${pair.credential}?x=1`, `${pair.credential}#x`, `https://user@${host}`, `https://%63${host.slice(1)}`, `http://${host}`, 'http://127.0.0.1:8099', 'http://localhost:8099']) {
      const facts = await executeHandoff(pair.portal, rejected);
      assert.deepEqual(facts.posts, [], rejected);
      assert.deepEqual(facts.acknowledgments, ['HANDOFF_BOUND'], rejected);
      assert.equal(facts.attached, 0, rejected); assert.equal(facts.closed, 1, rejected);
    }
  }
});

test('edge contract accepts only the new exact environment pairs and rejects old public domains', () => {
  for (const [environment, pair] of Object.entries(credentialOriginPairs)) {
    const valid = { ...structuredClone(baseline), environment, portal_origin: pair.portal, credential_origin: pair.credential };
    assert.equal(validateCredentialEdge(valid).decision, 'EDGE_CONFIG_VALID');
    for (const rejected of ['https://credentials.account.pinvou.com', 'https://credentials-uat.account.pinvou.com', ...Object.values(credentialOriginPairs).filter(item => item.portal !== pair.portal).map(item => item.credential)]) {
      assert.throws(() => validateCredentialEdge({ ...valid, credential_origin: rejected }), /CREDENTIAL_EDGE_/);
    }
  }
});
