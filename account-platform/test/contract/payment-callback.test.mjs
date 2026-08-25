import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import YAML from 'yaml';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('public payment callback contract is provider-neutral and identity-bound', async () => {
  const contract = YAML.parse(await read('contracts/openapi/billing.yaml'));
  const token = contract.paths['/api/v1/payment-callback/token'].post;
  const callback = contract.paths['/api/v1/payment-callbacks'].post;
  assert.deepEqual(token.security, [{ paymentCallbackClient: [] }]);
  assert.deepEqual(callback.security, [{ paymentCallbackBearer: [] }]);
  assert.equal(contract.paths['/api/v1/h3c-mall/payment-callback/token'], undefined);
  assert.equal(contract.paths['/api/v1/h3c-mall/payment-callbacks'], undefined);
  assert.equal(contract.components.schemas.PaymentCallbackToken.properties.scope.const, 'payment.callback.write');
  assert.equal(contract.components.schemas.PaymentCallbackToken.properties.expires_in.maximum, 900);
  const body = contract.components.schemas.PaymentCallback;
  assert.ok(body.required.includes('merchantOrderNo'));
  assert.ok(body.required.includes('providerStatus'));
  assert.equal(body.properties.providerId, undefined, 'provider identity must come from the callback token');
});

test('provider-bound callback profiles normalize into the existing notification inbox', async () => {
  const service = await read('internal/billing/service.go');
  const auth = await read('internal/platform/paymentcallback/auth.go');
  const profiles = await read('internal/platform/paymentcallback/profile.go');
  const payments = await read('internal/billing/payments.go');
  const worker = await read('internal/billing/payment_notification_worker.go');
  assert.match(auth, /ProviderID[\s\S]*ProviderVersion[\s\S]*MerchantID/);
  assert.match(service, /VerifyBearer[\s\S]*callbackProfiles\.Parse[\s\S]*PersistProviderNotification[\s\S]*StatusAccepted/);
  assert.match(profiles, /profileKey\(principal\.ProviderID, principal\.ProviderVersion\)/);
  assert.match(payments, /PersistProviderNotification[\s\S]*persistNotification/);
  assert.match(payments, /mergeSuccessfulPaymentWithinTransaction[\s\S]*mergeProviderObservationWithinTransaction/);
  assert.match(worker, /payment_notifications WHERE result='RECEIVED'/);
});

test('H3C remains the first provider profile without expanding unsupported capabilities', async () => {
  const provider = await read('internal/platform/h3cpayment/provider.go');
  const profile = await read('internal/platform/h3ccallback/notification.go');
  assert.match(provider, /NotificationSupported:\s*true/);
  assert.match(provider, /AuthoritativeUnpaidTerminal:\s*false/);
  assert.match(profile, /ProviderID\(\) string \{ return "h3c_mall" \}/);
  assert.match(profile, /case "FAILED":[\s\S]*return "REVIEW_REQUIRED"/);
  assert.match(provider, /ErrUnsupported/);
});

test('deployment exposes only the two generic callback endpoints and sources secrets externally', async () => {
  const ingress = await read('deploy/helm/account-platform/templates/ingress.yaml');
  const nginx = await read('deploy/nginx/account-platform.conf.template');
  const websiteLocations = await read('deploy/nginx/payment-callback-locations.conf.template');
  const publicAccountNginx = await read('../deploy/nginx-pinvou-account-domain.conf');
  const deployment = await read('deploy/helm/account-platform/templates/app-deployments.yaml');
  for (const route of ['/api/v1/payment-callback/token', '/api/v1/payment-callbacks']) {
    assert.match(ingress, new RegExp(route.replaceAll('/', '\\/')));
    assert.match(nginx, new RegExp(`location = ${route.replaceAll('/', '\\/')} \\{[\\s\\S]*?proxy_pass http:\\/\\/127\\.0\\.0\\.1:18102;`));
    assert.match(websiteLocations, new RegExp(`location = ${route.replaceAll('/', '\\/')} \\{[\\s\\S]*?proxy_pass __BILLING_UPSTREAM__;`));
    assert.match(publicAccountNginx, new RegExp(`location = ${route.replaceAll('/', '\\/')} \\{[\\s\\S]*?proxy_pass http:\\/\\/127\\.0\\.0\\.1:18102;`));
  }
  assert.doesNotMatch(websiteLocations, /server\s*\{|location \/\s*\{/);
  assert.match(publicAccountNginx, /server_name account\.pinvou\.com/);
  assert.match(publicAccountNginx, /return 308 https:\/\/account\.pinvou\.com\$request_uri/);
  const requirements = await read('docs/requirements.md');
  assert.match(requirements, /https:\/\/account\.pinvou\.com\/api\/v1\/payment-callback\/token/);
  assert.match(requirements, /https:\/\/account\.pinvou\.com\/api\/v1\/payment-callbacks/);
  assert.match(deployment, /PAYMENT_CALLBACK_CLIENT_SECRET[\s\S]*secretKeyRef/);
  assert.match(deployment, /PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64[\s\S]*secretKeyRef/);
  assert.doesNotMatch(deployment, /H3C_CALLBACK_/);
  assert.doesNotMatch(deployment, /value:\s*[^\n]*PAYMENT_CALLBACK_CLIENT_SECRET/);
});
