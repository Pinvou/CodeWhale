import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const template = readFileSync(new URL('../../deploy/nginx/account-platform.conf.template', import.meta.url), 'utf8');
const guide = readFileSync(new URL('../../deploy/nginx/README.md', import.meta.url), 'utf8');

for (const route of ['location /account/', 'location ^~ /api/admin/token/', 'location /']) {
  test(`legacy edge replaces the client source at ${route}`, () => {
    const start = template.indexOf(`    ${route} {`);
    assert.notEqual(start, -1);
    const block = template.slice(start, template.indexOf('\n    }', start));
    assert.match(block, /proxy_set_header X-Pinvou-Client-Source \$remote_addr;/);
    assert.doesNotMatch(block, /proxy_set_header X-Pinvou-Client-Source (?:""|\$http_)/);
  });
}

test('source propagation is paired with exact receiving peer configuration', () => {
  assert.match(guide, /ACCOUNT_TRUSTED_SOURCE_PEER_IPS/);
  assert.match(guide, /PORTAL_TRUSTED_PROXY_IPS/);
  assert.match(guide, /\$remote_addr/);
});
