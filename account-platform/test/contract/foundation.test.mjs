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
  const files = ['account.yaml', 'billing.yaml', 'token-gateway.yaml', 'admin.yaml'];
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

test('recent recharge lookup is wallet-scoped internally and context-scoped publicly', async () => {
  const billing = await readYaml('contracts/openapi/billing.yaml');
  const gateway = await readYaml('contracts/openapi/token-gateway.yaml');
  const internalParameters = billing.paths['/recharge/orders/recent'].get.parameters.map(({ name }) => name).sort();
  assert.deepEqual(internalParameters, ['requester_id', 'wallet_id']);
  assert.equal(gateway.paths['/recharge/orders/recent'].get.parameters, undefined);
  assert.equal(
    gateway.paths['/recharge/orders/recent'].get.responses['200'].content['application/json'].schema.$ref,
    'billing.yaml#/components/schemas/RecentRechargeOrder',
  );
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
  const composePath = path.join(root, 'deploy/compose/compose.yaml');
  const composeRaw = await readFile(composePath, 'utf8');
  const compose = YAML.parse(composeRaw);
  assert.equal(compose.networks.backend.internal, true);
  for (const service of ['postgres', 'redis', 'nats']) {
    assert.equal(compose.services[service].ports, undefined, `${service} must not publish ports`);
    assert.deepEqual(compose.services[service].networks, ['backend'], `${service} must stay off the edge network`);
  }
  for (const service of ['keycloak', 'account-service', 'billing-service', 'token-gateway', 'payment-simulator', 'token-portal']) {
    assert.deepEqual(compose.services[service].networks, ['backend', 'edge'], `${service} must publish only through loopback edge bindings`);
  }
  assert.equal(compose.services['payment-simulator'].environment.APP_ENV, 'development');
  assert.deepEqual(compose.services.newapi.profiles, ['real-newapi']);
  assert.deepEqual(compose.services.newapi.networks, ['backend']);
  assert.match(compose.services.newapi.image, /^calciumion\/new-api:v1\.0\.0-rc\.11@sha256:[a-f0-9]{64}$/);
  assert.deepEqual(compose.services.newapi.ports, ['127.0.0.1:18106:3000']);
  assert.match(compose.services['token-gateway'].environment.TOKEN_PROVIDER_ALLOWED_HOSTS, /(?:^|,)newapi(?:,|$)/);
  assert.match(compose.services['billing-service'].environment.DATABASE_URL, /account_platform_billing/);
  assert.equal(compose.services['billing-service'].environment.OIDC_AUDIENCE, compose.services['account-service'].environment.OIDC_AUDIENCE);
  assert.equal(compose.services['token-portal'].environment.BILLING_HTTP_URL, 'http://billing-service:8080');
  assert.match(compose.services['account-service'].environment.DATABASE_URL, /account_platform_account/);
  assert.match(compose.services['token-gateway'].environment.DATABASE_URL, /account_platform_token/);
  assert.notEqual(
    compose.services['billing-service'].environment.DATABASE_URL,
    compose.services['account-service'].environment.DATABASE_URL,
  );
  for (const source of ['account', 'billing', 'token']) {
    const relay = compose.services[`${source}-event-relay`];
    assert.equal(relay.environment.EVENT_SOURCE, source);
    assert.equal(relay.environment.NATS_URL, 'nats://nats:4222');
    assert.equal(relay.environment.DATABASE_URL, compose.services[`${source === 'token' ? 'token-gateway' : `${source}-service`}`].environment.DATABASE_URL);
  }
  const example = await readFile(path.join(root, 'deploy/compose/.env.example'), 'utf8');
  const names = new Set(example.split(/\r?\n/).filter(Boolean).map((line) => line.split('=', 1)[0]));
  for (const match of composeRaw.matchAll(/\$\{([A-Z0-9_]+):\?/g)) {
    assert.ok(names.has(match[1]), `${match[1]} is required by Compose but missing from .env.example`);
  }
});

test('Keycloak development client enables password login without enabling implicit flow', async () => {
  const realm = JSON.parse(await readFile(path.join(root, 'deploy/keycloak/pinvou-dev-realm.json'), 'utf8'));
  const desktop = realm.clients.find((client) => client.clientId === 'pinvou-desktop-dev');
  assert.ok(desktop);
  assert.equal(desktop.publicClient, true);
  assert.equal(desktop.standardFlowEnabled, true);
  assert.equal(desktop.directAccessGrantsEnabled, true);
  assert.equal(desktop.implicitFlowEnabled, false);
  assert.deepEqual(desktop.redirectUris, ['http://127.0.0.1/*']);
  assert.equal(realm.registrationEmailAsUsername, false);
  assert.equal(realm.loginWithEmailAllowed, true);
  assert.ok(realm.roles.realm.some((role) => role.name === 'platform-admin'));
  const provisioning = await readFile(path.join(root, 'deploy/keycloak/provision-platform-admin.sh'), 'utf8');
  assert.match(provisioning, /PINVOU_ADMIN_PASSWORD:\?set PINVOU_ADMIN_PASSWORD/);
  assert.match(provisioning, /--rolename "\$ROLE"/);
  assert.doesNotMatch(provisioning, /PINVOU_ADMIN_PASSWORD=['"][^$]/);
});

test('operations API is role protected and limits mutations to account lifecycle management', async () => {
  const admin = await readYaml('contracts/openapi/admin.yaml');
  assert.deepEqual(admin.security, [{ oidc: [] }]);
  for (const [pathName, pathItem] of Object.entries(admin.paths)) {
    assert.match(pathName, /^\/(account|billing|token)\/admin\/v1\//);
    for (const operation of Object.values(pathItem)) {
      assert.ok(operation.responses['403'], `${pathName} must document role denial`);
    }
  }
  for (const [pathName, pathItem] of Object.entries(admin.paths).filter(([name]) => name.startsWith('/billing/') || name.startsWith('/token/'))) {
    assert.deepEqual(Object.keys(pathItem), ['get'], `${pathName} must remain read-only`);
  }
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/users/{user_id}/status']), ['patch']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/users/{user_id}']), ['delete']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations']), ['get', 'post']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/units']), ['get', 'post']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/units/{unit_id}']), ['patch', 'delete']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/status']), ['patch']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}']), ['patch', 'delete']);
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

test('payment simulator preserves the exact signed callback payload', async () => {
  const migration = await readFile(
    path.join(root, 'migrations/payment-simulator/000004_exact_notification_payload.up.sql'),
    'utf8',
  );
  assert.match(migration, /ALTER COLUMN payload TYPE BYTEA/);
  assert.doesNotMatch(migration, /ALTER COLUMN payload TYPE JSONB/);
});
