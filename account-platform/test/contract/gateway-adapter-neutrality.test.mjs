import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('model gateway protocols and usage evidence remain provider neutral', async () => {
  const [migration, adapter, proxy, reconciliation, admin, service, openapi, adminUI, deployment] = await Promise.all([
    read('migrations/token/000011_gateway_adapter_neutrality.up.sql'),
    read('internal/token/gateway_adapter.go'),
    read('internal/token/proxy.go'),
    read('internal/token/reconciliation.go'),
    read('internal/token/admin.go'),
    read('internal/token/service.go'),
    read('contracts/openapi/token-gateway.yaml'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
  ]);
  for (const field of ['provider_id', 'adapter_version', 'upstream_request_id', 'upstream_reference_digest']) assert.match(migration, new RegExp(field));
  assert.doesNotMatch(migration, /ADD COLUMN newapi_request_id/);
  assert.match(adapter, /NewNewAPIGatewayAdapter[\s\S]*X-Oneapi-Request-Id/);
  assert.match(adapter, /ParseUsage[\s\S]*prompt_tokens/);
  assert.match(adapter, /RejectsBeforeGeneration/);
  assert.match(adapter, /CopyResponseHeaders[\s\S]*referenceHeaders/);
  assert.doesNotMatch(proxy, /X-Oneapi|NewAPI|newapi/i);
  assert.match(proxy, /s\.upstream\.ParseUsage/);
  assert.match(proxy, /s\.upstream\.CopyResponseHeaders/);
  assert.match(reconciliation, /UsageEvidenceProvider/);
  for (const source of [admin, service, adminUI]) {
    assert.doesNotMatch(source, /newapi_request_id|provider_request_id/i);
  }
  assert.match(service, /modelProtocols/);
  assert.match(openapi, /\/model-protocols:[\s\S]*GatewayProtocolCapabilities/);
  assert.match(openapi, /protocol_code:[\s\S]*enum: \[[^\]]*CHAT[^\]]*\]/);
  assert.match(deployment, /TOKEN_GATEWAY_ADAPTER/);
});
