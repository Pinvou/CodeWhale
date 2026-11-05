import assert from 'node:assert/strict';
import { once } from 'node:events';
import { createConnection } from 'node:net';
import test from 'node:test';
import net from './helpers/net-fixture.mjs';
import https from './helpers/https-fixture.mjs';

async function listen(server) {
  const ready = once(server, 'listening');
  server.listen(0, '127.0.0.1');
  await ready;
  return server.address().port;
}

const close = server => new Promise((resolve, reject) => {
  server.close(error => error ? reject(error) : resolve());
});

test('TCP fixture close waits for acknowledgement and leaves an independent listener alive', async () => {
  const first = net.createServer();
  const second = net.createServer(socket => socket.end('independent fixture'));
  let client;
  try {
    const ports = await Promise.all([listen(first), listen(second)]);
    assert.notEqual(ports[0], ports[1]);
    await close(first);
    assert.equal(first.listening, false);
    assert.equal(second.listening, true);
    client = createConnection({host: '127.0.0.1', port: ports[1]});
    const [data] = await once(client, 'data');
    assert.equal(data.toString(), 'independent fixture');
    await once(client, 'close');
  } finally {
    client?.destroy();
    await Promise.all([close(first), close(second)]);
  }
});

test('HTTPS fixture preserves listen and close callback contracts', async () => {
  const server = https.createServer();
  try {
    assert.ok(await listen(server) > 0);
    await close(server);
    assert.equal(server.listening, false);
    assert.equal(server.address(), null);
  } finally {
    await close(server);
  }
});
