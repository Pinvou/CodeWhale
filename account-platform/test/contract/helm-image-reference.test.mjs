import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const root = new URL('../../deploy/helm/account-platform/', import.meta.url);
const helper = readFileSync(new URL('templates/_helpers.tpl', root), 'utf8');
const templates = ['app-deployments.yaml', 'event-relays.yaml', 'infrastructure.yaml', 'keycloak-realm-upgrade.yaml', 'migrations.yaml', 'token-governed-workloads.yaml', '_nats-credentials.tpl']
  .map(name => readFileSync(new URL(`templates/${name}`, root), 'utf8')).join('\n');

test('Helm image references support immutable digests and preserve non-production tags', () => {
  assert.match(helper, /define "pinvou\.imageRef"/);
  assert.match(helper, /\.image\.repository }}@{{ \.image\.digest/);
  assert.match(helper, /\.image\.repository }}:{{ \.image\.tag/);
  assert.match(helper, /production-new image .*must be pinned by digest/);
  assert.doesNotMatch(templates, /repository }}:{{ .*\.tag/);
  assert.doesNotMatch(templates, /include \\"pinvou\.imageRef\\"/);
});
