import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('N, N-1, and N-2 share the published account, billing, and gateway write contracts', async () => {
  const [policy, account, billing, gateway] = await Promise.all([
    read('internal/platform/clientcompat/policy.go'),
    read('contracts/openapi/account.yaml').then(YAML.parse),
    read('contracts/openapi/billing.yaml').then(YAML.parse),
    read('contracts/openapi/token-gateway.yaml').then(YAML.parse),
  ]);
  const raw = policy.match(/defaultReleaseManifest\s*=\s*`([^`]+)`/)?.[1];
  assert.ok(raw, 'runtime release manifest must be inspectable by the release contract gate');
  const releases = JSON.parse(raw);
  assert.equal(releases.length, 3);
  for (const release of releases) {
    assert.match(release.version, /^\d+\.\d+\.\d+$/);
    assert.ok(account.paths['/account/bootstrap'].post.responses['426']);
    assert.ok(billing.paths['/recharge/orders'].post.responses['426']);
    assert.ok(gateway.paths['/chat/completions'].post.responses['426']);
    assert.ok(gateway.paths['/recharge/orders'].post.responses['426']);
  }
});

test('compatibility manifest and stable 426 publish notification and trusted upgrade metadata', async () => {
  const [gateway, common, policy] = await Promise.all([
    read('contracts/openapi/token-gateway.yaml').then(YAML.parse),
    read('contracts/openapi/common.yaml').then(YAML.parse),
    read('internal/platform/clientcompat/policy.go'),
  ]);
  const schema = gateway.components.schemas.ClientCompatibility;
  for (const field of ['api_contract_version', 'current_client_version', 'minimum_supported_client_version', 'upgrade_url', 'releases']) {
    assert.ok(schema.required.includes(field), `missing ${field}`);
  }
  assert.ok(common.components.schemas.Error.properties.code.enum.includes('CLIENT_UPGRADE_REQUIRED'));
  assert.match(policy, /60 \* 24 \* time\.Hour/);
  assert.match(policy, /90\*24\*time\.Hour/);
  assert.match(policy, /http\.StatusUpgradeRequired/);
  assert.match(policy, /rel=\\"latest-version\\"/);
});

test('desktop direct API and platform-model runtime both identify the exact client version', async () => {
  const [account, bridge, identity] = await Promise.all([
    read('../pinvou3-app/src-tauri/src/features/account_platform/mod.rs'),
    read('../pinvou3-app/src-tauri/src/features/assistant/platform/bridge.rs'),
	read('../pinvou3-app/src-tauri/src/platform/client_version.rs'),
  ]);
  assert.match(identity, /X-Pinvou-Client-Version/);
  assert.match(identity, /env!\("CARGO_PKG_VERSION"\)/);
  assert.match(account, /platform::client_version::HEADER/);
  assert.match(bridge, /is_builtin_account_platform/);
  assert.match(bridge, /client_version::HEADER\.to_string\(\)/);
});
