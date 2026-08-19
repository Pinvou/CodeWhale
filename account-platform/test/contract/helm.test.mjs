import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';
import Ajv from 'ajv';
import addFormats from 'ajv-formats';

const root = path.resolve(import.meta.dirname, '../..');
const chart = path.join(root, 'deploy/helm/account-platform');

test('Helm production-new values contain references, not secret material or simulators', async () => {
  const raw = await readFile(path.join(chart, 'values-production-new.yaml'), 'utf8');
  const values = YAML.parse(raw);
  assert.equal(values.global.environment, 'production-new');
  assert.match(values.global.runtimeSecret, /production-new/);
  assert.equal(values.tokenGateway.secretProvider, 'remote');
  assert.equal(values.tokenGateway.providerAllowedHosts.includes('newapi'), true);
  assert.match(values.images.newApi.digest, /^sha256:[a-f0-9]{64}$/);
  assert.ok(values.tokenGateway.providerAllowedHosts.every(host => !/mock|localhost|127\.0\.0\.1/i.test(host)));
  for (const cidrs of Object.values(values.external)) {
    assert.ok(cidrs.length > 0);
    assert.ok(cidrs.every(cidr => cidr !== '0.0.0.0/0' && cidr !== '::/0'));
  }
  assert.doesNotMatch(raw, /(password|clientSecret|accessToken|refreshToken|apiKey|api_key):\s*[^\s]+/i);
  assert.doesNotMatch(raw, /payment-simulator|mock-newapi/i);
});

test('Helm values schema rejects unsafe production-new overrides', async () => {
  const schema = JSON.parse(await readFile(path.join(chart, 'values.schema.json'), 'utf8'));
  const base = YAML.parse(await readFile(path.join(chart, 'values.yaml'), 'utf8'));
  const production = YAML.parse(await readFile(path.join(chart, 'values-production-new.yaml'), 'utf8'));
  const merge = (left, right) => Object.fromEntries(new Set([...Object.keys(left), ...Object.keys(right)]).values().map(key => [key, left[key] && right[key] && typeof left[key] === 'object' && typeof right[key] === 'object' && !Array.isArray(left[key]) ? merge(left[key], right[key]) : right[key] ?? left[key]]));
  const values = merge(base, production);
  const ajv = new Ajv({ allErrors: true, strict: true });
  addFormats(ajv);
  const validate = ajv.compile(schema);
  assert.equal(validate(values), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...values, networkPolicy: { ...values.networkPolicy, enabled: false } }), false);
  assert.equal(validate({ ...values, tokenGateway: { ...values.tokenGateway, secretProvider: 'local' } }), false);
  assert.equal(validate({ ...values, newApi: { ...values.newApi, enabled: false } }), false);
  assert.equal(validate({ ...values, images: { ...values.images, newApi: { ...values.images.newApi, digest: '' } } }), false);
  assert.equal(validate({ ...values, external: { ...values.external, postgresCidrs: ['0.0.0.0/0'] } }), false);
});

test('Helm chart includes required workloads, migration hooks, and deny-by-default policy', async () => {
  const templateDirectory = path.join(chart, 'templates');
  const files = await readdir(templateDirectory);
  const combined = (await Promise.all(files.map(name => readFile(path.join(templateDirectory, name), 'utf8')))).join('\n');
  for (const component of ['account-service', 'billing-service', 'token-gateway', 'token-portal', 'new-api', 'keycloak', 'redis', 'nats']) {
    assert.match(combined, new RegExp(component));
  }
  assert.match(combined, /pre-install,pre-upgrade/);
  assert.match(combined, /default-deny/);
  assert.match(combined, /pinvou\.io\/newapi-data-plane/);
  assert.match(combined, /app\.kubernetes\.io\/component: token-gateway/);
  assert.match(combined, /newapi-database-url/);
  assert.match(combined, /newapi-session-secret/);
  assert.match(combined, /SYSTEM_NATS_STATUS_URL/);
  assert.match(combined, /range \$source := list "account" "billing" "token"/);
  assert.match(combined, /kind: Service[\s\S]*name: \{\{ \$fullname \}\}-\{\{ \$source \}\}-event-relay/);
  assert.match(combined, /name: monitor, port: 8222/);
  assert.doesNotMatch(combined, /kind:\s*Secret/);
});
