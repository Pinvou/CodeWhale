import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { nginxConfig, nginxBlock } from './helpers/nginx-config.mjs';

const file = new URL('../../deploy/nginx/personal.conf.template', import.meta.url);
const token = ['location', '=', '/realms/pinvou/protocol/openid-connect/token'];
const values = (nodes, name) => nodes.filter(node => !node.children && node.args[0] === name).map(node => node.args.slice(1));
function budget(text) {
  const parsed = nginxConfig(text);
  const server = nginxBlock(parsed, ['server']);
  const route = nginxBlock(parsed, token);
  assert.deepEqual(values(server, 'proxy_read_timeout'), [['30s']], 'ordinary routes retain their prior budget');
  assert.deepEqual(values(server, 'proxy_connect_timeout'), [['3s']]);
  assert.deepEqual(values(server, 'proxy_send_timeout'), [['30s']]);
  assert.deepEqual(values(route, 'proxy_read_timeout'), [['60s']], 'the real progressive wait must survive the edge');
  assert.deepEqual(values(route, 'proxy_pass'), [['http://$identity_origin']]);
  assert.deepEqual(values(nginxBlock(parsed, ['location', '/account/']), 'proxy_read_timeout'), []);
}

test('only the exact public token edge gains room for the bounded login wait', () => {
  budget(readFileSync(file, 'utf8'));
});

test('missing, commented or old token budgets cannot pass the configuration contract', () => {
  const text = readFileSync(file, 'utf8');
  const at = text.indexOf('proxy_read_timeout 60s;');
  assert.notEqual(at, -1);
  for (const replacement of ['', '# proxy_read_timeout 60s;', 'proxy_read_timeout 30s;']) {
    assert.throws(() => budget(text.slice(0, at) + replacement + text.slice(at + 'proxy_read_timeout 60s;'.length)));
  }
});
