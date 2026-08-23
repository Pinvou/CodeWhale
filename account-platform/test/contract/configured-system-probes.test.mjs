import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const read = relative => fs.readFileSync(path.join(root, relative), 'utf8');

test('system status components are configured and provider neutral', () => {
  const source = read('internal/portal/system_status.go');
  const main = read('cmd/token-portal/main.go');
  const compose = read('deploy/compose/compose.yaml');
  const helm = read('deploy/helm/account-platform/templates/app-deployments.yaml');

  assert.match(main, /SYSTEM_PROBES_JSON/);
  assert.match(source, /for _, configured := range s\.config\.SystemProbes/);
  assert.doesNotMatch(source, /key == "newapi"|NewAPI status check/);
  assert.match(source, /JSONBooleanField/);
  assert.match(source, /case "relay"/);
  assert.match(compose, /SYSTEM_PROBES_JSON/);
  assert.match(helm, /SYSTEM_PROBES_JSON/);
  for (const legacy of ['SYSTEM_KEYCLOAK_STATUS_URL', 'SYSTEM_NEWAPI_STATUS_URL', 'SYSTEM_NATS_STATUS_URL']) {
    assert.doesNotMatch(main, new RegExp(legacy));
  }
});
