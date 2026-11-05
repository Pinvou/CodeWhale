import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { runInNewContext } from 'node:vm';

const source = readFileSync(new URL('../../internal/portal/credential-handoff.v1.js', import.meta.url), 'utf8');
const nonce = 'n'.repeat(43);
const launchID = '11111111-1111-4111-8111-111111111111';
const production = ['https://account.pinvou.com', 'https://credentials.pinvou.com'];

async function harness(pair = production, configure = () => {}) {
  const messages = [], dom = [], posts = [], listeners = new Map(), timers = new Map();
  const location = new URL('/credential-handoff', pair[0]);
  const opener = { postMessage: (data, origin) => messages.push({ data, origin }) };
  const window = {
    opener, closed: false,
    close() { this.closed = true; },
    addEventListener: (name, listener) => listeners.set(name, listener),
    removeEventListener: name => listeners.delete(name),
    setTimeout: callback => { const id = timers.size + 1; timers.set(id, callback); return id; },
    clearTimeout: id => timers.delete(id),
  };
  const navigator = { serviceWorker: { controller: null, getRegistrations: async () => [] } };
  const meta = { content: pair[1] };
  const document = {
    querySelector: selector => { assert.equal(selector, 'meta[name="pinvou-credential-origin"]'); return meta; },
    createElement: tag => {
      assert.ok(['form', 'input'].includes(tag));
      const node = { tag, children: [], append(child) { this.children.push(child); } };
      if (tag === 'form') node.submit = () => posts.push({ action: node.action, method: node.method,
        target: node.target, enctype: node.enctype, inputs: node.children.map(child => ({ name: child.name, value: child.value })) });
      return node;
    },
    body: { append: element => dom.push(element) },
  };
  const port = { messages: [], closed: false, start() {}, close() { this.closed = true; }, postMessage(data) { this.messages.push(data); } };
  const bind = { source: opener, origin: location.origin, ports: [port], data: { version: 1, type: 'HANDOFF_BIND', nonce } };
  const submit = { currentTarget: port, data: { version: 1, type: 'HANDOFF_SUBMIT', nonce, launch_id: launchID } };
  const fixture = { window, navigator, location, meta, bind, submit, port, messages, dom, posts, listeners, timers };
  configure(fixture);
  runInNewContext(source, { window, navigator, location, document, URL }, { timeout: 1000, filename: 'credential-handoff.v1.js' });
  await new Promise(setImmediate);
  fixture.deliverBind = () => listeners.get('message')?.(bind);
  fixture.deliverSubmit = () => port.onmessage?.(submit);
  return fixture;
}

test('actual handoff script completes READY BIND SUBMIT only to its paired credential origin', async t => {
  for (const pair of [production,
    ['https://account-uat.pinvou.com', 'https://credentials-uat.pinvou.com'],
    ['http://127.0.0.1:3000', 'http://127.0.0.1:3001'],
    ['http://localhost:3000', 'http://localhost:3001']]) {
    await t.test(pair[0], async () => {
      const f = await harness(pair);
      assert.deepEqual(f.messages.map(message => ({ ...message.data, origin: message.origin })),
        [{ version: 1, type: 'HANDOFF_READY', origin: pair[0] }]);
      f.deliverBind();
      assert.deepEqual({ ...f.port.messages[0] }, { version: 1, type: 'HANDOFF_BOUND', nonce });
      f.deliverSubmit();
      assert.deepEqual(f.posts, [{ action: `${pair[1]}/launch`, method: 'post', target: '_self',
        enctype: 'application/x-www-form-urlencoded', inputs: [{ name: 'launch_id', value: launchID }] }]);
      assert.equal(f.dom.length, 1);
      assert.equal(f.submit.data.launch_id, '', 'consumed launch credential must be cleared');
      assert.equal(f.port.closed, true);
      assert.equal(f.window.opener, null);
      assert.equal(f.timers.size, 0);
      assert.equal(f.listeners.size, 0);
      // Replay through a retained stale port callback cannot submit a second form.
      f.deliverSubmit();
      assert.equal(f.posts.length, 1);
      assert.equal(f.dom.length, 1);
    });
  }
});

test('actual handoff script rejects invalid boot bind and submit facts before POST or DOM effects', async t => {
  const cases = [
    ['wrong path', 'boot', f => f.location.pathname = '/other'],
    ['query credential', 'boot', f => f.location.search = '?launch_id=secret'],
    ['fragment credential', 'boot', f => f.location.hash = '#secret'],
    ['no opener', 'boot', f => f.window.opener = null],
    ['self opener', 'boot', f => f.window.opener = f.window],
    ['active service worker', 'boot', f => f.navigator.serviceWorker.controller = {}],
    ['covering service worker', 'boot', f => f.navigator.serviceWorker.getRegistrations = async () => [{ scope: `${f.location.origin}/` }]],
    ['failed worker query', 'boot', f => f.navigator.serviceWorker.getRegistrations = async () => { throw new Error('controlled failure'); }],
    ['foreign opener message', 'bind', f => f.bind.source = {}],
    ['foreign bind origin', 'bind', f => f.bind.origin = 'https://other.invalid'],
    ['bind version', 'bind', f => f.bind.data.version = 2],
    ['bind type', 'bind', f => f.bind.data.type = 'OTHER'],
    ['bind short nonce', 'bind', f => f.bind.data.nonce = 'n'],
    ['bind nonce alphabet', 'bind', f => f.bind.data.nonce = '!'.repeat(43)],
    ['bind extra field', 'bind', f => f.bind.data.extra = true],
    ['bind null data', 'bind', f => f.bind.data = null],
    ['bind no port', 'bind', f => f.bind.ports = []],
    ['bind extra port', 'bind', f => f.bind.ports.push(f.port)],
    ['submit foreign port', 'submit', f => f.submit.currentTarget = {}],
    ['submit version', 'submit', f => f.submit.data.version = 2],
    ['submit type', 'submit', f => f.submit.data.type = 'OTHER'],
    ['submit wrong nonce', 'submit', f => f.submit.data.nonce = 'x'.repeat(43)],
    ['submit invalid launch', 'submit', f => f.submit.data.launch_id = 'invalid'],
    ['submit extra field', 'submit', f => f.submit.data.extra = true],
    ['submit null data', 'submit', f => f.submit.data = null],
    ['retired credential origin', 'submit', f => f.meta.content = 'https://credentials.account.pinvou.com'],
    ['cross environment credential', 'submit', f => f.meta.content = 'https://credentials-uat.pinvou.com'],
    ['credential path suffix', 'submit', f => f.meta.content += '/'],
    ['worker acquired after bind', 'submit', f => f.navigator.serviceWorker.controller = {}],
    ['bound stage timeout', 'submit', f => { for (const callback of f.timers.values()) callback(); }],
  ];
  assert.equal(cases.length, 30);
  for (const [name, phase, mutate] of cases) {
    await t.test(name, async () => {
      const f = await harness(production, phase === 'boot' ? mutate : undefined);
      if (phase === 'bind') mutate(f);
      f.deliverBind();
      if (phase === 'submit') mutate(f);
      f.deliverSubmit();
      assert.deepEqual(f.posts, []);
      assert.deepEqual(f.dom, []);
    });
  }
});
