import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { nginxConfig, nginxBlock, nginxSourceHeaders } from './helpers/nginx-config.mjs';

const directory = new URL('../../deploy/nginx/', import.meta.url);
const account = ['location', '/account/'];
const identity = ['location', '=', '/identity/v1/registration/password-prechecks'];
function retained(text) {
  const config = nginxConfig(text);
  for (const args of [account, identity]) {
    assert.deepEqual(nginxSourceHeaders(nginxBlock(config, args)), ['$remote_addr']);
  }
}
test('supported legacy loopback edge overwrites the same canonical source for Account admission and Identity preparation', () => {
  const text = readFileSync(new URL('account-platform.conf.template', directory), 'utf8');
  retained(text);
  const marker = 'location = /identity/v1/registration/password-prechecks {';
  const start = text.indexOf(marker); assert.notEqual(start, -1);
  const directive = 'proxy_set_header X-Pinvou-Client-Source $remote_addr;';
  const at = text.indexOf(directive, start); assert.notEqual(at, -1);
  for (const replacement of ['', `# ${directive}`, 'proxy_set_header X-Pinvou-Client-Source "";', 'proxy_set_header X-Pinvou-Client-Source $http_x_pinvou_client_source;']) {
    assert.throws(() => retained(text.slice(0, at) + replacement + text.slice(at + directive.length)));
  }
  retained(text.slice(0, at) + '# proxy_set_header X-Pinvou-Client-Source fake;\n' + text.slice(at));
});
test('personal edge retains one inherited server canonical header with no location-level header reset', () => {
  const config = nginxConfig(readFileSync(new URL('personal.conf.template', directory), 'utf8'));
  assert.deepEqual(nginxSourceHeaders(nginxBlock(config, ['server'])), ['$remote_addr']);
  for (const args of [account, identity]) {
    assert.ok(!nginxBlock(config, args).some(node => node.args[0] === 'proxy_set_header'), 'a location header list must not silently replace the parent canonical header list');
  }
});
const oracle = JSON.parse(readFileSync(new URL('../fixtures/nginx-source-syntax.json', import.meta.url), 'utf8'));
assert.equal(oracle.length, 9);
for (const fixture of oracle) {
  test(`nginx source syntax: ${fixture.name}`, () => {
    const read = () => nginxSourceHeaders(nginxBlock(nginxConfig(fixture.text), ['location', '/fixture']));
    if (fixture.error) assert.throws(read);
    else assert.deepEqual(read(), fixture.sources);
  });
}
