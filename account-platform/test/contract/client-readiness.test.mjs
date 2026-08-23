import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('client switch readiness is service-authenticated, read-only, and attestation bound', async () => {
  const [service, readiness, migration, gatewayMain, accountService] = await Promise.all([
    read('internal/token/service.go'),
    read('internal/token/client_readiness.go'),
    read('migrations/token/000016_client_readiness.up.sql'),
    read('cmd/token-gateway/main.go'),
    read('internal/account/service.go'),
  ]);
  assert.match(service, /internal\.GET\("\/client-readiness"/);
  assert.match(service, /internal\.POST\("\/client-readiness\/attestations"/);
  assert.match(service, /router\.GET\("\/v1\/client-compatibility"/);
  assert.match(accountService, /internal\.GET\("\/client-compatibility"/);
  assert.match(readiness, /ValidateModelReady/);
  assert.match(readiness, /ValidatePriceFormula/);
  assert.match(readiness, /OIDC_METADATA/);
  assert.match(readiness, /ArtifactSHA256/);
  assert.match(readiness, /ed25519\.Sign/);
  assert.doesNotMatch(readiness, /ReservePriced|EnsureHold|chatCompletions|BeginUsage/);
  assert.match(migration, /claims_sha256/);
  assert.match(migration, /immutable/);
  assert.match(gatewayMain, /required in production/);
});

test('public compatibility contract exposes only updater-safe verification metadata', async () => {
  const contract = YAML.parse(await read('contracts/openapi/token-gateway.yaml'));
  const operation = contract.paths['/client-compatibility'].get;
  assert.deepEqual(operation.security, []);
  const schema = contract.components.schemas.ClientCompatibility;
  assert.ok(schema.required.includes('minimum_client_version'));
  assert.ok(schema.required.includes('maximum_client_version'));
  assert.ok(schema.required.includes('attestation_available'));
  assert.equal(schema.properties.token_account_id, undefined);
  assert.equal(schema.properties.credential_id, undefined);
  assert.equal(schema.properties.route, undefined);
});
