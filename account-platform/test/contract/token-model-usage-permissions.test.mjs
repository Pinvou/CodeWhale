import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('Token model and usage surfaces use exact personal or organization permissions', async () => {
  const [authorization, service, catalog, proxy, contract] = await Promise.all([
    read('internal/token/model_usage_authorization.go'),
    read('internal/token/service.go'),
    read('internal/token/catalog_http.go'),
    read('internal/token/proxy.go'),
    read('contracts/openapi/token-gateway.yaml'),
  ]);
  assert.match(authorization, /case "PERSONAL"[\s\S]*AuthorizedUserID[\s\S]*case "ORGANIZATION"[\s\S]*AuthorizedOrganizationUserID/);
  assert.match(service, /modelProtocols[\s\S]*personal\.models\.read[\s\S]*org\.models\.read/);
  assert.match(catalog, /listCatalogModels[\s\S]*personal\.models\.read[\s\S]*org\.models\.read/);
  assert.match(proxy, /chatCompletions[\s\S]*personal\.model\.invoke[\s\S]*org\.model\.invoke/);
  assert.match(service, /getUsageRequest[\s\S]*personal\.usage\.read[\s\S]*org\.usage\.read\.self/);
  assert.match(service, /listMyUsageRequests[\s\S]*personal\.usage\.read[\s\S]*org\.usage\.read\.self/);
  assert.match(service, /cancelUsageRequest[\s\S]*authorizePersonalUsageUser[\s\S]*personal\.usage\.cancel/);
  for (const permission of ['personal.models.read', 'org.models.read', 'personal.model.invoke', 'org.model.invoke', 'personal.usage.read', 'org.usage.read.self', 'personal.usage.cancel']) {
    assert.match(contract, new RegExp(permission.replaceAll('.', '\\.')));
  }
});
