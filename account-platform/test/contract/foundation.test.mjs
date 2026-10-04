import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');

async function readYaml(relative) {
  return YAML.parse(await readFile(path.join(root, relative), 'utf8'));
}

test('OpenAPI contracts expose stable versions and no inline credentials', async () => {
  const files = ['account.yaml', 'billing.yaml', 'token-gateway.yaml'];
  for (const file of files) {
    const raw = await readFile(path.join(root, 'contracts/openapi', file), 'utf8');
    const document = YAML.parse(raw);
    assert.equal(document.openapi, '3.1.0', file);
    assert.match(document.info.version, /^\d+\.\d+\.\d+$/, file);
    assert.ok(Object.keys(document.paths).length > 0, file);
    assert.doesNotMatch(raw, /(client_secret|access_token|api[_-]?key)\s*:\s*[^<{\s]/i, file);
  }
});

test('billing products are server-authoritative and fixed to the approved catalog', async () => {
  const billing = await readYaml('contracts/openapi/billing.yaml');
  const productIDs = billing.components.schemas.RechargeProduct.properties.product_id.enum;
  assert.deepEqual(productIDs, ['CNY_1', 'CNY_10', 'CNY_100', 'CNY_200']);

  const createProperties = billing.components.schemas.CreateRechargeOrderRequest.properties;
  assert.equal(createProperties.amount_fen, undefined);
  assert.equal(createProperties.credit_microcredits, undefined);
  assert.deepEqual(createProperties.channel.$ref, '#/components/schemas/PaymentChannel');
});

test('event schemas compile as JSON Schema 2020-12 and reject secrets', async () => {
  const directory = path.join(root, 'contracts/events');
  const files = (await readdir(directory)).filter((name) => name.endsWith('.schema.json'));
  assert.ok(files.length >= 2);
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  addFormats(ajv);
  for (const file of files) {
    const raw = await readFile(path.join(directory, file), 'utf8');
    const schema = JSON.parse(raw);
    assert.doesNotMatch(raw, /token_credential|access_token|client_secret|api_key/i, file);
    assert.doesNotThrow(() => ajv.compile(schema), file);
  }
});

test('Compose keeps data stores private and simulator development-only', async () => {
  const compose = await readYaml('deploy/compose/compose.yaml');
  assert.equal(compose.networks.backend.internal, true);
  for (const service of ['postgres', 'redis', 'nats']) {
    assert.equal(compose.services[service].ports, undefined, `${service} must not publish ports`);
    assert.deepEqual(compose.services[service].networks, ['backend'], `${service} must remain on the private network`);
  }
  assert.notEqual(compose.networks.frontend.internal, true, 'published services need a reachable bridge network');
  for (const service of ['keycloak', 'account-service', 'billing-service', 'token-gateway', 'payment-simulator']) {
    assert.deepEqual(compose.services[service].networks, ['backend', 'frontend'], service);
    assert.ok(compose.services[service].ports.length > 0, `${service} must have a local entry point`);
    for (const port of compose.services[service].ports) {
      assert.match(port, /^127\.0\.0\.1:\d+:8080$/, `${service} must bind only to loopback`);
    }
  }
  assert.equal(compose.services['payment-simulator'].environment.APP_ENV, 'development');
  assert.match(compose.services['billing-service'].environment.DATABASE_URL, /account_platform_billing/);
  assert.match(compose.services['account-service'].environment.DATABASE_URL, /account_platform_account/);
  assert.match(compose.services['token-gateway'].environment.DATABASE_URL, /account_platform_token/);
  assert.notEqual(
    compose.services['billing-service'].environment.DATABASE_URL,
    compose.services['account-service'].environment.DATABASE_URL,
  );
});

test('Compose environment template covers every required variable with placeholders', async () => {
  const compose = await readFile(path.join(root, 'deploy/compose/compose.yaml'), 'utf8');
  const template = await readFile(path.join(root, 'deploy/compose/.env.example'), 'utf8');
  const entries = new Map(template.split(/\r?\n/).filter((line) => line && !line.startsWith('#'))
    .map((line) => {
      const separator = line.indexOf('=');
      assert.ok(separator > 0, 'template entry must have a variable name');
      return [line.slice(0, separator), line.slice(separator + 1)];
    }));
  const required = new Set([...compose.matchAll(/\$\{([A-Z_][A-Z_0-9]*):\?/g)].map((match) => match[1]));
  assert.ok(required.size > 0);
  assert.deepEqual([...entries.keys()].sort(), [...required].sort());
  for (const [name, value] of entries) assert.match(value, /^replace-with-/, name);
});

test('Keycloak development client disables unsafe grants', async () => {
  const realm = JSON.parse(await readFile(path.join(root, 'deploy/keycloak/pinvou-dev-realm.json'), 'utf8'));
  const desktop = realm.clients.find((client) => client.clientId === 'pinvou-desktop-dev');
  assert.ok(desktop);
  assert.equal(desktop.publicClient, true);
  assert.equal(desktop.standardFlowEnabled, true);
  assert.equal(desktop.directAccessGrantsEnabled, false);
  assert.equal(desktop.implicitFlowEnabled, false);
  assert.deepEqual(desktop.redirectUris, ['http://127.0.0.1/*']);
});

test('Compose browser issuer and server key retrieval preserve the identity boundary', async () => {
  const compose = await readYaml('deploy/compose/compose.yaml');
  const browserIssuer = `${compose.services.keycloak.environment.KC_HOSTNAME}/realms/pinvou`;
  assert.equal(browserIssuer, 'http://127.0.0.1:18080/realms/pinvou');
  for (const name of ['account-service', 'token-gateway']) {
    const environment = compose.services[name].environment;
    assert.equal(environment.OIDC_ISSUER, browserIssuer);
    assert.equal(environment.OIDC_JWKS_URL, 'http://keycloak:8080/realms/pinvou/protocol/openid-connect/certs');
  }
});

test('every migration up script has a matching down script', async () => {
  const migrationRoot = path.join(root, 'migrations');
  for (const domain of ['account', 'billing', 'token', 'payment-simulator']) {
    const files = await readdir(path.join(migrationRoot, domain));
    const up = files.filter((name) => name.endsWith('.up.sql')).map((name) => name.replace('.up.sql', ''));
    const down = new Set(files.filter((name) => name.endsWith('.down.sql')).map((name) => name.replace('.down.sql', '')));
    assert.ok(up.length > 0, `${domain} must contain migrations`);
    for (const migration of up) assert.ok(down.has(migration), `${domain}/${migration} has no down migration`);
  }
});
