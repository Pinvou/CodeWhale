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
  assert.deepEqual(productIDs, ['PLAN_TRIAL', 'PLAN_STANDARD', 'PLAN_ADVANCED', 'PLAN_FLAGSHIP']);

  const product = billing.components.schemas.RechargeProduct;
  for (const field of ['plan_name', 'sku_code', 'base_points', 'bonus_points', 'validity_days', 'price_status', 'invoice_product_name']) {
    assert.ok(product.required.includes(field), field);
  }

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

test('personal usage history exposes only user-facing fields', async () => {
  const gateway = await readYaml('contracts/openapi/token-gateway.yaml');
  const operation = gateway.paths['/me/usage-requests'].get;
  const record = gateway.components.schemas.UserUsageRecord;
  assert.equal(operation.responses['200'].headers['Cache-Control'].schema.const, 'no-store');
  assert.deepEqual(Object.keys(record.properties).sort(), [
    'actual_microcredits',
    'cached_input_tokens',
    'completed_at',
    'display_status',
    'input_tokens',
    'model',
    'output_tokens',
    'started_at',
    'updated_at',
    'usage_request_id',
  ]);
  for (const internalField of ['token_account_id', 'credential_id', 'wallet_id', 'provider']) {
    assert.equal(record.properties[internalField], undefined);
  }
});

test('personal plans cannot be purchased into organization wallets', async () => {
  const source = await readFile(path.join(root, 'internal/token/service.go'), 'utf8');
  assert.match(source, /resolved\.ContextType != "PERSONAL"/);
  assert.match(source, /PERSONAL_RECHARGE_ONLY/);
});

test('paid plan orders create independent thirty-day entitlement packages', async () => {
  const migration = await readFile(path.join(root, 'migrations/billing/000011_entitlement_packages.up.sql'), 'utf8');
  const payments = await readFile(path.join(root, 'internal/billing/payments.go'), 'utf8');
  assert.match(migration, /payment_order_id UUID NOT NULL UNIQUE/);
  assert.match(migration, /points_reserved \+ points_used \+ points_remaining = points_total/);
  assert.match(payments, /validUntil := validFrom\.AddDate\(0, 0, product\.ValidityDays\)/);
  assert.match(payments, /INSERT INTO entitlement_packages/);
  assert.match(payments, /refundable,expires_at/);
});

test('usage holds reserve and settle entitlement packages in earliest-expiry order', async () => {
  const migration = await readFile(path.join(root, 'migrations/billing/000012_package_hold_allocations.up.sql'), 'utf8');
  const postgres = await readFile(path.join(root, 'internal/billing/postgres.go'), 'utf8');
  assert.match(migration, /CREATE TABLE usage_hold_allocations/);
  assert.match(postgres, /ORDER BY valid_until,credited_at,id FOR UPDATE/);
  assert.match(postgres, /points_remaining=points_remaining-\$2,points_reserved=points_reserved\+\$2/);
  assert.match(postgres, /released_microcredits=reserved_microcredits/);
});

test('entitlement packages expire locally and remain queryable by the personal user', async () => {
  const gateway = await readYaml('contracts/openapi/token-gateway.yaml');
  const packages = await readFile(path.join(root, 'internal/billing/packages.go'), 'utf8');
  assert.ok(gateway.paths['/me/entitlement-packages']);
  assert.match(packages, /status='ACTIVE' AND valid_until<=\$1 AND points_reserved=0/);
  assert.match(packages, /"EXPIRE", "expire:"\+packageID\.String\(\)/);
});

test('repeated purchase clicks reuse one pending order for the same personal plan', async () => {
  const payments = await readFile(path.join(root, 'internal/billing/payments.go'), 'utf8');
  assert.match(payments, /lockTransactionKey\(ctx, tx, "payment\.pending"/);
  assert.match(payments, /getPendingPlanOrderForUpdate/);
  assert.match(payments, /status IN \('CREATED','PENDING'\)/);
	assert.match(payments, /policy\.ReusePendingOrderAfterExpiry && !existing\.ExpiresAt\.After\(now\)/);
	assert.match(payments, /UPDATE payment_orders SET expires_at=\$2/);
});

test('personal recharge orders expose a normal history list', async () => {
  const billing = await readYaml('contracts/openapi/billing.yaml');
  const gateway = await readYaml('contracts/openapi/token-gateway.yaml');
  const service = await readFile(path.join(root, 'internal/token/service.go'), 'utf8');
  assert.ok(billing.paths['/recharge/orders'].get);
  assert.ok(gateway.paths['/recharge/orders'].get);
  assert.match(service, /resolved\.ContextType != "PERSONAL"/);
  assert.match(service, /RechargeOrders\(r\.Context\(\), resolved\.UserID, walletID\)/);
});

test('late Mall payments require audited operator repair before fulfillment', async () => {
  const admin = await readYaml('contracts/openapi/admin.yaml');
  const migration = await readFile(path.join(root, 'migrations/billing/000014_payment_repairs.up.sql'), 'utf8');
  const reconciliation = await readFile(path.join(root, 'internal/billing/reconciliation.go'), 'utf8');
  assert.ok(admin.paths['/billing/admin/v1/payment-orders/{order_id}/repair']);
  assert.match(migration, /CREATE TABLE payment_order_repairs/);
  assert.match(migration, /payment_order_id UUID NOT NULL UNIQUE/);
  assert.match(reconciliation, /p\.Policy\(\)\.ReviewLatePayment/);
  assert.match(reconciliation, /provider\.Status != "SUCCESS"/);
  assert.match(reconciliation, /INSERT INTO payment_order_repairs/);
  assert.match(reconciliation, /issuePackageWithValidityStart/);
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
  assert.equal(compose.services['token-portal'].environment.SYSTEM_NATS_STATUS_URL, 'http://nats:8222/healthz');
  assert.equal(compose.services['token-portal'].environment.SYSTEM_ACCOUNT_RELAY_URL, 'http://account-event-relay:9090');
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
    assert.equal(relay.ports, undefined, `${source} relay metrics must remain internal`);
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

test('mobile authentication provisioning works in the minimal Keycloak image', async () => {
  const provisioning = await readFile(path.join(root, 'deploy/keycloak/provision-mobile-auth.sh'), 'utf8');
  const authenticator = await readFile(path.join(root, 'deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/MobileDirectGrantAuthenticator.java'), 'utf8');
  assert.doesNotMatch(provisioning, /\bawk\b/);
  assert.match(provisioning, /authentication\/flows\/\$FLOW\/executions/);
  assert.match(provisioning, /-s requirement=REQUIRED -n/);
  assert.match(provisioning, /update users\/profile -r "\$REALM" -f -/);
  const profileSource = provisioning.match(/<<'PINVOU_MOBILE_USER_PROFILE'\r?\n([\s\S]*?)\r?\nPINVOU_MOBILE_USER_PROFILE/)?.[1];
  assert.ok(profileSource, 'mobile user profile JSON must be embedded in the idempotent provisioning script');
  const profile = JSON.parse(profileSource);
  for (const name of ['email', 'firstName', 'lastName']) {
    const attribute = profile.attributes.find((candidate) => candidate.name === name);
    assert.ok(attribute, `${name} must remain available as an optional profile field`);
    assert.equal(attribute.required, undefined, `${name} must not block mobile-only login`);
  }
  for (const name of ['phone_number', 'phone_number_verified']) {
    const attribute = profile.attributes.find((candidate) => candidate.name === name);
    assert.deepEqual(attribute.permissions.edit, ['admin'], `${name} must be managed by the account service`);
  }
  assert.match(authenticator, /firstValue\("X-Pinvou-Identity-Subject"\)/);
  assert.match(authenticator, /getUserById\(context\.getRealm\(\), value\)/);
  assert.match(authenticator, /if \(smsLogin\) \{[\s\S]*verifySMS\(mobile, challenge, code\)[\s\S]*getUserById/);
});

test('remote Keycloak administration is available only through a controlled local channel', async () => {
  const proxy = await readFile(path.join(root, 'deploy/nginx/account-platform.conf.template'), 'utf8');
  const remoteCompose = await readFile(path.join(root, 'deploy/compose/compose.remote.yaml'), 'utf8');
  const runbook = await readFile(path.join(root, 'deploy/runbooks/keycloak-admin-access.md'), 'utf8');
  assert.match(proxy, /location \/realms\//);
  assert.match(proxy, /location \/resources\//);
  assert.doesNotMatch(proxy, /location \/admin\/?/);
  assert.match(remoteCompose, /KC_HOSTNAME_ADMIN: \$\{KEYCLOAK_ADMIN_BASE_URL:-http:\/\/127\.0\.0\.1:18081\}/);
  assert.match(runbook, /127\.0\.0\.1:18081\/admin\//);
  assert.match(runbook, /ssh -N -T/);
  assert.match(runbook, /kubectl[\s\S]*port-forward/);
});

test('organization creation is reserved for platform administrators', async () => {
  const account = await readYaml('contracts/openapi/account.yaml');
  const admin = await readYaml('contracts/openapi/admin.yaml');
  assert.equal(account.paths['/organizations'], undefined);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations']), ['get', 'post']);
});

test('operations API is role protected and limits mutations to documented management workflows', async () => {
  const admin = await readYaml('contracts/openapi/admin.yaml');
  assert.deepEqual(admin.security, [{ oidc: [] }]);
  for (const [pathName, pathItem] of Object.entries(admin.paths)) {
    assert.match(pathName, /^(\/system\/status|\/(account|billing|token)\/admin\/v1\/)/);
    for (const operation of Object.values(pathItem)) {
      assert.ok(operation.responses['403'], `${pathName} must document role denial`);
    }
  }
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/users/{user_id}/status']), ['patch']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/users/{user_id}']), ['delete']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations']), ['get', 'post']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/units']), ['get', 'post']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/units/{unit_id}']), ['patch', 'delete']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/members']), ['get', 'post']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/members/{user_id}']), ['delete']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/members/{user_id}/roles']), ['put']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/users/{user_id}/memberships']), ['get']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/audit-logs/{audit_id}']), ['get']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/ownership']), ['post']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}/status']), ['patch']);
  assert.deepEqual(Object.keys(admin.paths['/account/admin/v1/organizations/{organization_id}']), ['patch', 'delete']);
  assert.deepEqual(Object.keys(admin.paths['/billing/admin/v1/pricing-versions']), ['get', 'post']);
  assert.deepEqual(Object.keys(admin.paths['/billing/admin/v1/payment-orders/{order_id}/refunds']), ['post']);
  assert.deepEqual(Object.keys(admin.paths['/billing/admin/v1/usage-holds/{hold_id}/release']), ['post']);
  assert.deepEqual(Object.keys(admin.paths['/billing/admin/v1/reconciliation-runs']), ['get', 'post']);
  assert.deepEqual(Object.keys(admin.paths['/token/admin/v1/token-accounts/{token_account_id}']), ['get']);
  assert.deepEqual(Object.keys(admin.paths['/token/admin/v1/token-accounts/{token_account_id}/status']), ['patch']);
  assert.deepEqual(Object.keys(admin.paths['/token/admin/v1/token-accounts/{token_account_id}/routes']), ['put', 'delete']);
  assert.deepEqual(Object.keys(admin.paths['/token/admin/v1/token-accounts/{token_account_id}/credentials/{credential_id}/validate']), ['post']);
  assert.deepEqual(Object.keys(admin.paths['/token/admin/v1/usage-requests/{usage_request_id}']), ['get']);
  assert.deepEqual(Object.keys(admin.paths['/token/admin/v1/usage-requests/{usage_request_id}/retry-reconciliation']), ['post']);
});

test('Token operations expose credential metadata without provider secrets', async () => {
  const source = await readFile(path.join(root, 'internal/token/admin.go'), 'utf8');
  const metadata = source.match(/type AdminCredentialMetadata struct \{([\s\S]*?)\n\}/)?.[1] || '';
  assert.match(metadata, /Fingerprint/);
  assert.match(metadata, /ValidationStatus/);
  assert.match(metadata, /UpdatedAt/);
  assert.doesNotMatch(metadata, /BaseURL|Secret|Ciphertext|KeyID/);
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

test('default model routes are platform managed and deployment-secret backed', async () => {
  const migration = await readFile(
    path.join(root, 'migrations/token/000005_platform_managed_routes.up.sql'),
    'utf8',
  );
  const provisioner = await readFile(path.join(root, 'internal/token/default_route.go'), 'utf8');
  const gateway = await readFile(path.join(root, 'internal/token/proxy.go'), 'utf8');
  const chart = await readFile(
    path.join(root, 'deploy/helm/account-platform/templates/app-deployments.yaml'),
    'utf8',
  );
  assert.match(migration, /management_mode IN \('USER', 'PLATFORM'\)/);
  assert.match(provisioner, /pg_advisory_xact_lock/);
  assert.match(provisioner, /p\.repository\.secrets\.Encrypt/);
  assert.match(gateway, /explicit == nil && s\.defaultRoutes != nil/);
  assert.match(chart, /key: token-default-provider-api-key/);
  assert.doesNotMatch(chart, /TOKEN_DEFAULT_PROVIDER_API_KEY, value:/);
});

test('usage review reconciliation is leased, evidence based, and secret backed', async () => {
  const migration = await readFile(
    path.join(root, 'migrations/token/000006_usage_reconciliation.up.sql'),
    'utf8',
  );
  const reconciler = await readFile(path.join(root, 'internal/token/reconciliation.go'), 'utf8');
  const gateway = await readFile(path.join(root, 'internal/token/proxy.go'), 'utf8');
  const chart = await readFile(
    path.join(root, 'deploy/helm/account-platform/templates/app-deployments.yaml'),
    'utf8',
  );
  assert.match(migration, /reconciliation_lease_until/);
  assert.match(reconciler, /FOR UPDATE(?: OF request)? SKIP LOCKED/);
  assert.match(reconciler, /request_id/);
  assert.match(reconciler, /PriceByID/);
  assert.match(reconciler, /automatic reconciliation exhausted/);
  assert.match(gateway, /X-Oneapi-Request-Id/);
  assert.match(gateway, /RecordUpstreamRequest/);
  assert.match(chart, /key: token-reconciliation-newapi-admin-token/);
  assert.doesNotMatch(chart, /TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN, value:/);
});

test('payment simulator preserves the exact signed callback payload', async () => {
  const migration = await readFile(
    path.join(root, 'migrations/payment-simulator/000004_exact_notification_payload.up.sql'),
    'utf8',
  );
  assert.match(migration, /ALTER COLUMN payload TYPE BYTEA/);
  assert.doesNotMatch(migration, /ALTER COLUMN payload TYPE JSONB/);
});
