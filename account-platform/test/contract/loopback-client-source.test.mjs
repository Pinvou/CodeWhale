import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { nginxConfig, nginxBlock, nginxSourceHeaders } from './helpers/nginx-config.mjs';

const template = readFileSync(new URL('../../deploy/nginx/account-platform.conf.template', import.meta.url), 'utf8');
const guide = readFileSync(new URL('../../deploy/nginx/README.md', import.meta.url), 'utf8');

for (const route of ['location /account/', 'location = /identity/v1/registration/password-prechecks', 'location ^~ /api/admin/token/', 'location /']) {
  test(`legacy edge replaces the client source at ${route}`, () => {
    const args = route.split(' ');
    assert.deepEqual(nginxSourceHeaders(nginxBlock(nginxConfig(template), args)), ['$remote_addr']);
  });
}

test('source propagation is paired with exact receiving peer configuration', () => {
  assert.match(guide, /ACCOUNT_TRUSTED_SOURCE_PEER_IPS/);
  assert.match(guide, /PORTAL_TRUSTED_PROXY_IPS/);
  assert.match(guide, /IDENTITY_PASSWORD_PREPARATION_TRUSTED_PROXY_IPS/);
  assert.match(guide, /\$remote_addr/);
});
