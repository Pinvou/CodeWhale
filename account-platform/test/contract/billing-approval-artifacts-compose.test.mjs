import assert from 'node:assert/strict';
import test from 'node:test';
import { chmodSync, writeFileSync, symlinkSync } from 'node:fs';
import { currentRenderFixture } from '../fixtures/production-render.mjs';
import { checkComposeBillingApprovalArtifacts } from '../../../scripts/account-platform-billing-approval-artifacts-check.mjs';
import { checkProductionBusiness } from '../../../scripts/account-platform-business-compose-check.mjs';

const enabled = { skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1' };
test('supported personal composition supplies unconditional Billing bootstrap without finance workloads', enabled, () => {
  const fixture = currentRenderFixture('compose', 'personal');
  try {
    const services = fixture.render.config.services, environment = services['billing-service'].environment;
    for (const suffix of ['PUBLIC_KEYS_FILE', 'PUBLIC_KEYS_SHA256', 'MANIFEST_FILE', 'MANIFEST_SHA256', 'RECEIPT_FILE', 'RECEIPT_SHA256']) {
      assert.ok(environment[`BILLING_APPROVAL_CONSUMER_${suffix}`], `personal required ${suffix}`);
    }
    assert.equal(services['approval-resources'], undefined);
    assert.equal(services['billing-event-relay'], undefined);
    assert.equal(services['token-command-executor'], undefined);
    assert.doesNotThrow(() => checkComposeBillingApprovalArtifacts(fixture.render.config));
    assert.equal(checkProductionBusiness(fixture.render, fixture.inventory).deployment_status, 'NOT_DEPLOYED');
  } finally { fixture.dispose(); }
});

test('actual personal and models compositions reject missing or untrusted public inputs and private aliases', enabled, () => {
  for (const profile of ['personal', 'models']) {
    const f = currentRenderFixture('compose', profile);
    try {
      assert.doesNotThrow(() => checkComposeBillingApprovalArtifacts(f.render.config));
      for (const suffix of ['PUBLIC_KEYS_FILE', 'PUBLIC_KEYS_SHA256', 'MANIFEST_FILE', 'MANIFEST_SHA256', 'RECEIPT_FILE', 'RECEIPT_SHA256']) {
        const config = structuredClone(f.render.config);
        delete config.services['billing-service'].environment[`BILLING_APPROVAL_CONSUMER_${suffix}`];
        assert.throws(() => checkComposeBillingApprovalArtifacts(config), /billing_approval_artifacts_/, `${profile} ${suffix}`);
      }
      for (const [name, mutate] of [
        ['writable artifact bind', s => { s.volumes.find(v => v.target === '/run/pinvou/approval-identities').read_only = false; }],
        ['implicit host directory creation', s => { s.volumes.find(v => v.target === '/run/pinvou/approval-identities').bind.create_host_path = true; }],
        ['whole directory loses fixed target', s => { s.volumes.find(v => v.target === '/run/pinvou/approval-identities').target = '/run/pinvou'; }],
        ['public directories alias each other', s => { s.volumes.find(v => v.target === '/run/pinvou/approval-manifest').source = s.volumes.find(v => v.target === '/run/pinvou/approval-identities').source; }],
        ['NKey directory aliases public identity', s => { s.volumes.find(v => v.target === '/run/pinvou/nats').source = s.volumes.find(v => v.target === '/run/pinvou/approval-identities').source; }],
        ['wrong independent digest', s => { s.environment.BILLING_APPROVAL_CONSUMER_RECEIPT_SHA256 = '0'.repeat(64); }],
        ['private path replaces public input', s => { s.environment.BILLING_APPROVAL_CONSUMER_MANIFEST_FILE = '/run/pinvou/nats/manifest.json'; }],
        ['plaintext broker', s => { s.environment.NATS_URL = 'nats://fixture.invalid:4222'; }],
      ]) {
        const config = structuredClone(f.render.config);
        mutate(config.services['billing-service']);
        assert.throws(() => checkComposeBillingApprovalArtifacts(config), /billing_approval_artifacts_|production_render_/, name);
      }
      const alias = `${f.directory}/public-artifact-alias`;
      symlinkSync(f.env.APPROVAL_PUBLIC_IDENTITY_DIRECTORY, alias, process.platform === 'win32' ? 'junction' : 'dir');
      const linked = structuredClone(f.render.config);
      linked.services['billing-service'].volumes.find(v => v.target === '/run/pinvou/approval-identities').source = alias.replaceAll('\\', '/');
      assert.throws(() => checkComposeBillingApprovalArtifacts(linked), /production_render_source_symlink/);
      const identity = f.env.APPROVAL_PUBLIC_IDENTITY_DIRECTORY;
      writeFileSync(`${identity}/unapproved.json`, '{}');
      assert.throws(() => checkComposeBillingApprovalArtifacts(f.render.config), /billing_approval_artifacts_identity_directory/);
    } finally { f.dispose(); }
  }
});

test('actual personal preflight rejects independently bound file drift', enabled, () => {
  const f = currentRenderFixture('compose', 'personal');
  try {
    const file = `${f.env.APPROVAL_PUBLIC_RECEIPT_DIRECTORY}/receipt.json`;
    chmodSync(file, 0o600); writeFileSync(file, '{"changed":true}');
    assert.throws(() => checkProductionBusiness(f.render, f.inventory), /billing_approval_artifacts_digest_mismatch/);
  } finally { f.dispose(); }
});
