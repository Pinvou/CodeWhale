import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

test('public signup contract exposes no pre-captcha availability endpoint', async () => {
  const spec = JSON.parse(await readFile(new URL('../../contracts/openapi/registration-signup.json', import.meta.url), 'utf8'));
  assert.ok(!Object.hasOwn(spec.paths, '/v1/auth/registration/precheck'));
  for (const path of ['/v1/auth/captcha/challenges', '/v1/auth/sms/challenges', '/v1/auth/register']) assert.ok(spec.paths[path]?.post, path);
  const sms = spec.paths['/v1/auth/sms/challenges'].post;
  const body = sms.requestBody.content['application/json'].schema;
  for (const field of ['captcha_id', 'purpose', 'mobile']) assert.ok(body.required.includes(field), field);
  assert.equal(body.properties.captcha_offset.type, 'integer');
  assert.ok(sms.responses['410'], 'expired password references retain their existing public status');
});

test('authorization inventory removes only the retired public precheck authority', async () => {
  const inventory = JSON.parse(await readFile(new URL('../../deploy/authorization-entrypoints.json', import.meta.url), 'utf8'));
  assert.ok(!inventory.http_policies.some(entry => entry.path === '/v1/auth/registration/precheck'));
  for (const path of ['/v1/auth/captcha/challenges', '/v1/auth/sms/challenges', '/v1/auth/register']) {
    const entry = inventory.http_policies.find(entry => entry.source === 'account-platform/internal/account/service.go' && entry.path === path);
    assert.equal(entry?.scope, 'public');
    assert.match(entry.strategy, /^protocol\.registration\./);
  }
});
