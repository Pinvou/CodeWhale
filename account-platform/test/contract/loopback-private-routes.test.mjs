import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { nginxConfig, nginxBlock } from './helpers/nginx-config.mjs';

const template = readFileSync(new URL('../../deploy/nginx/account-platform.conf.template', import.meta.url), 'utf8');
const server = nginxBlock(nginxConfig(template), ['server']);

for (const route of ['/account/internal', '/account/admin', '/token/internal', '/token/admin']) {
  for (const [modifier, path] of [['=', route], ['^~', `${route}/`]]) {
    test(`legacy public TLS edge rejects private route ${modifier} ${path}`, () => {
      const block = nginxBlock(server, ['location', modifier, path]);
      assert.deepEqual(block, [{ args: ['return', '404'], children: null }]);
    });
  }
}

for (const [location, upstream] of [
  [['location', '/account/'], 'http://127.0.0.1:18101'],
  [['location', '/token/'], 'http://127.0.0.1:18103'],
  [['location', '^~', '/api/admin/token/'], 'http://127.0.0.1:18105'],
]) {
  test(`legacy public route remains forwarded at ${location.join(' ')}`, () => {
    const block = nginxBlock(server, location);
    assert.ok(block.some(node => node.children === null && JSON.stringify(node.args) === JSON.stringify(['proxy_pass', upstream])));
  });
}
