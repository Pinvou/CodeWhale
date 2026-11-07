import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import test from 'node:test';
import { parse } from 'yaml';
import { currentRenderFixture } from '../fixtures/production-render.mjs';
import { composeCompositions, repositoryRoot } from '../../../scripts/account-platform-production-render.mjs';

test('isolated product admission Compose keeps the production baseline disabled and binds the exact Billing and validator consumers', {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, t => {
  const fixture = currentRenderFixture('compose', 'models');
  t.after(() => fixture.dispose());
  const env = Object.fromEntries(Object.entries(process.env).filter(([name]) =>
    ['PATH', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(name.toUpperCase())));
  const overlay = resolve(repositoryRoot, 'account-platform/deploy/compose/compose.product-admission-acceptance.yaml');
  // Compose serializes zero-valued soft/hard limits as absent JSON fields.
  const limits = parse(readFileSync(overlay, 'utf8')).services['product-admission-validator'].ulimits;
  assert.deepEqual(limits, { core: { soft: 0, hard: 0 } });
  const render = (base, enabled) => spawnSync(process.env.PINVOU_TEST_KMS_COMPOSE_BINARY, [
    '--env-file', fixture.recipe.envFile,
    ...base.flatMap(name => ['-f', resolve(repositoryRoot, `account-platform/deploy/production/compose.${name}.yaml`)]),
    ...(enabled ? ['-f', overlay] : []), '--profile', '*', 'config', '--format', 'json',
  ], { cwd: repositoryRoot, env: { ...env, COMPOSE_CONVERT_WINDOWS_PATHS: '1' }, encoding: 'utf8', timeout: 30_000, windowsHide: true });
  const config = (base, enabled) => {
    const result = render(base, enabled);
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stderr);
    return JSON.parse(result.stdout);
  };

  const inputs = {
    SOCKET_DIRECTORY: '/fixture/admission/socket',
    TRANSIT_TOKEN_DIRECTORY: '/fixture/admission/transit',
    TRANSIT_KEY: 'admission-fixture',
    TRANSIT_URL: 'https://transit.fixture.invalid',
    VALIDATOR_IMAGE: `sha256:${'a'.repeat(64)}`,
    VALIDATOR_MEMORY: '2048m',
    VALIDATOR_CPUS: '0.5',
    VALIDATOR_PIDS: '64',
  };
  for (const [profile, base] of [['models', composeCompositions.models], ['personal', [...composeCompositions.personal, 'finance']]]) {
    fixture.updateEnv('ACCOUNT_PLATFORM_PROFILE', profile);
    for (const key of Object.keys(inputs)) fixture.updateEnv(`PRODUCT_ADMISSION_ACCEPTANCE_${key}`, '');
    const before = config(base, false);
    const billingBefore = before.services['billing-service'];
    assert.equal(before.services['product-admission-validator'], undefined);
    assert.equal(billingBefore.environment.BILLING_PRODUCT_ADMISSION_MATERIALS_ENABLED, undefined);
    for (const [key, value] of Object.entries(inputs)) fixture.updateEnv(`PRODUCT_ADMISSION_ACCEPTANCE_${key}`, value);

    const after = config(base, true);
    const billing = after.services['billing-service'];
    const worker = after.services['product-admission-validator'];
    assert.equal(after.name, 'pinvou-product-admission-acceptance');
    assert.equal(billing.user, billingBefore.user);
    assert.equal(billing.user, '65532:65532');
    assert.deepEqual(billing.group_add, ['65533']);
    assert.deepEqual(Object.keys(billingBefore.networks), ['backend']);
    assert.deepEqual(Object.keys(billing.networks).sort(), ['backend', 'kms']);
    assert.deepEqual(after.networks, {
      ...before.networks,
      kms: { external: true, name: fixture.env.OPENBAO_NETWORK, ipam: {} },
    });
    assert.notEqual(after.networks.kms.name, after.networks.backend.name);
    assert.deepEqual(billing.depends_on, billingBefore.depends_on);
    assert.deepEqual(billing.healthcheck, billingBefore.healthcheck);
    assert.deepEqual(worker.environment, {
      EVIDENCE_IMAGE_VALIDATOR_ALLOWED_UID: billing.user.split(':')[0],
      EVIDENCE_IMAGE_VALIDATOR_ENABLED: 'true',
      EVIDENCE_IMAGE_VALIDATOR_SOCKET: '/run/pinvou/admission-validator/validator.sock',
      EVIDENCE_PDF_CANDIDATE_ENABLED: 'true',
    });
    assert.equal(worker.user, '65533:65533');
    assert.equal(worker.image, inputs.VALIDATOR_IMAGE);
    assert.equal(worker.pull_policy, 'never');
    assert.equal(worker.restart, 'no');
    assert.equal(worker.network_mode, 'none');
    assert.equal(worker.networks, undefined);
    assert.equal(worker.ports, undefined);
    assert.equal(worker.env_file, undefined);
    assert.equal(worker.read_only, true);
    assert.deepEqual(worker.cap_drop, ['ALL']);
    assert.deepEqual(worker.security_opt, ['no-new-privileges:true']);
    assert.deepEqual(worker.ulimits, { core: {} });
    assert.equal(Number(worker.mem_limit), 2048 * 1024 * 1024);
    assert.equal(Number(worker.memswap_limit), Number(worker.mem_limit));
    assert.equal(Number(worker.cpus), 0.5);
    assert.equal(worker.pids_limit, 64);

    const socket = { type: 'bind', source: inputs.SOCKET_DIRECTORY, target: '/run/pinvou/admission-validator', bind: { create_host_path: false } };
    const sink = { type: 'bind', source: inputs.TRANSIT_TOKEN_DIRECTORY, target: '/run/pinvou/admission-transit', read_only: true, bind: { create_host_path: false } };
    assert.deepEqual(worker.volumes, [socket]);
    assert.deepEqual(billing.volumes.filter(mount => mount.target.startsWith('/run/pinvou/admission-')), [{ ...socket, read_only: true }, sink]);
    assert.ok(billing.volumes.some(mount => mount.target === '/run/pinvou/trust' && mount.read_only));
    assert.deepEqual(billing.environment, {
      ...billingBefore.environment,
      BILLING_PRODUCT_ADMISSION_MATERIALS_ENABLED: 'true',
      BILLING_PRODUCT_ADMISSION_VALIDATOR_SOCKET: worker.environment.EVIDENCE_IMAGE_VALIDATOR_SOCKET,
      BILLING_PRODUCT_ADMISSION_VALIDATOR_UID: worker.user.split(':')[0],
      BILLING_PRODUCT_ADMISSION_KEY_ID: inputs.TRANSIT_KEY,
      BILLING_PRODUCT_ADMISSION_TRANSIT_URL: inputs.TRANSIT_URL,
      BILLING_PRODUCT_ADMISSION_TRANSIT_TOKEN_FILE: `${sink.target}/token`,
      BILLING_PRODUCT_ADMISSION_TRANSIT_CA_FILE: '/run/pinvou/trust/ca.crt',
    });
    for (const [name, service] of Object.entries(before.services)) {
      if (name !== 'billing-service') assert.deepEqual(after.services[name], service, `${profile}: ${name}`);
    }
  }
  fixture.updateEnv('ACCOUNT_PLATFORM_PROFILE', 'models');
  for (const [key, value] of Object.entries(inputs)) {
    const name = `PRODUCT_ADMISSION_ACCEPTANCE_${key}`;
    fixture.updateEnv(name, '');
    const result = render(composeCompositions.models, true);
    assert.ifError(result.error);
    assert.notEqual(result.status, 0, name);
    assert.equal(result.stdout, '', name);
    assert.match(result.stderr, new RegExp(name));
    fixture.updateEnv(name, value);
  }
  fixture.updateEnv('ACCOUNT_PLATFORM_PROFILE', 'personal');
  fixture.updateEnv('OPENBAO_NETWORK', '');
  const missingTransitNetwork = render([...composeCompositions.personal, 'finance'], true);
  assert.ifError(missingTransitNetwork.error);
  assert.notEqual(missingTransitNetwork.status, 0);
  assert.equal(missingTransitNetwork.stdout, '');
  assert.match(missingTransitNetwork.stderr, /OPENBAO_NETWORK/);
});
