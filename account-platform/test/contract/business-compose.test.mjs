import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { checkProductionBusiness } from '../../../scripts/account-platform-business-compose-check.mjs';
import { businessComposeFixture } from '../helpers/business-compose-fixture.mjs';

function rejects(change) {
  const fixture = businessComposeFixture(); change(fixture.config, fixture.inventory);
  assert.throws(() => checkProductionBusiness(fixture.config, fixture.inventory));
}
test('production business template: all twelve services and existing security checks pass', () => {
  const { config, inventory } = businessComposeFixture();
  assert.deepEqual(checkProductionBusiness(config, inventory), { decision: 'CONFIG_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' });
});
test('production template: required inputs have no development fallback; raw private env files', () => {
  const source = readFileSync(new URL('../../deploy/production/compose.business.yaml', import.meta.url), 'utf8');
  assert.equal(source.includes(':-'), false);
  const { input } = businessComposeFixture();
  for (const service of Object.values(input.services)) {
    assert.equal(service.mem_limit, '256m');
    assert.deepEqual(service.ulimits, { core: { soft: 0, hard: 0 } });
  }
  assert.equal(Object.values(input.services).filter(s => s.env_file).length, 5);
  for (const service of Object.values(input.services)) if (service.env_file) {
    assert.equal(service.env_file.length, 1); assert.equal(service.env_file[0].required, true); assert.equal(service.env_file[0].format, 'raw');
  }
});
test('all client compatibility consumers receive the same explicit release manifest', () => {
  const { config } = businessComposeFixture();
  const consumers = ['account-service', 'billing-service', 'token-gateway', 'token-portal', 'token-credential-portal'];
  for (const name of consumers) {
    const main = readFileSync(new URL(`../../cmd/${name}/main.go`, import.meta.url), 'utf8');
    assert.match(main, /clientcompat\.FromEnv\(/);
    for (const key of ['PINVOU_API_CONTRACT_VERSION', 'PINVOU_CLIENT_UPGRADE_URL', 'PINVOU_CLIENT_RELEASE_MANIFEST']) {
      assert.ok(config.services[name].environment[key]);
      assert.equal(config.services[name].environment[key], config.services['account-service'].environment[key]);
    }
    rejects(c => { delete c.services[name].environment.PINVOU_CLIENT_RELEASE_MANIFEST; });
  }
});
test('production business rejects mutable or receipt-mismatched images and extra services', () => {
  rejects(c => { c.services['account-service'].image = 'latest'; });
  rejects(c => { c.services['account-service'].image = 'sha256:' + 'f'.repeat(64); });
  rejects(c => { c.services['mock-newapi'] = structuredClone(c.services['account-service']); });
  rejects(c => { c.services['account-service'].build = '.'; });
});
test('real Compose zero core limits may omit JSON zero fields but never the core declaration', () => {
  const { config, inventory } = businessComposeFixture();
  for (const service of Object.values(config.services)) service.ulimits = { core: {} };
  assert.equal(checkProductionBusiness(config, inventory).decision, 'CONFIG_VALID');
  rejects(c => { delete c.services['account-service'].ulimits; });
  rejects(c => { c.services['account-service'].ulimits = {}; });
  rejects(c => { c.services['account-service'].ulimits = { core: { hard: -1 } }; });
  rejects(c => { c.services['account-service'].ulimits = { core: { soft: 1 } }; });
  rejects(c => { c.services['account-service'].ulimits = { core: null }; });
  rejects(c => { c.services['account-service'].mem_limit = '256m'; });
  rejects(c => { c.services['account-service'].mem_limit = 536870912; });
  rejects(c => { c.services['account-service'].cpus = 1; });
});
test('production business rejects wrong subjects, expanded permissions and static identity', () => {
  rejects((c, i) => { i.subjects['billing-service'] = i.subjects['account-service']; });
  rejects(c => { const e = c.services['account-service'].environment; const r = JSON.parse(e.WORKLOAD_IDENTITY_RULES_JSON); r[0].subject = '00000000-0000-4000-8000-000000000999'; e.WORKLOAD_IDENTITY_RULES_JSON = JSON.stringify(r); });
  rejects(c => { const e = c.services['identity-service'].environment; const r = JSON.parse(e.WORKLOAD_IDENTITY_RULES_JSON); r[0].permissions.push('*'); e.WORKLOAD_IDENTITY_RULES_JSON = JSON.stringify(r); });
  rejects(c => { c.services['token-portal'].environment.INTERNAL_SERVICE_TOKEN = 'PUBLIC_FIXTURE_ONLY'; });
});
test('production business rejects wrong DB role, password DSN, missing pgpass and cross-domain control', () => {
  rejects(c => { c.services['account-service'].environment.DATABASE_URL = c.services['billing-service'].environment.DATABASE_URL; });
  rejects(c => { c.services['account-service'].environment.DATABASE_URL = c.services['account-service'].environment.DATABASE_URL.replace('@', ':PUBLIC_FIXTURE_ONLY@'); });
  rejects(c => { delete c.services['billing-service'].environment.PGPASSFILE; });
  rejects(c => { c.services['token-gateway'].environment.DATABASE_URL = c.services['token-gateway'].environment.DATABASE_URL.replace('token_gateway_fixture', 'token_gateway'); });
  rejects(c => { c.services['token-governance-check'].environment.ACCOUNT_POLICY_DATABASE_URL = c.services['token-governance-check'].environment.TOKEN_MIGRATOR_DATABASE_URL; });
});
test('governance uses a required dedicated Account policy identity, separate from runtime', () => {
  const { config, inventory, variables } = businessComposeFixture();
  const tool = config.services['token-governance-check'];
  assert.equal(new URL(tool.environment.ACCOUNT_POLICY_DATABASE_URL).username, variables.ACCOUNT_POLICY_RELEASE_ROLE);
  assert.equal(new URL(config.services['account-service'].environment.DATABASE_URL).username, 'account_service');
  assert.equal(new URL(tool.environment.ACCOUNT_POLICY_DATABASE_URL).pathname, '/account_platform_account');
  assert.equal(new URL(tool.environment.ACCOUNT_POLICY_DATABASE_URL).searchParams.get('sslmode'), 'verify-full');
  assert.deepEqual(tool.profiles, ['governance-operations']);
  assert.equal(checkProductionBusiness(config, inventory).decision, 'CONFIG_VALID');
  const source = readFileSync(new URL('../../deploy/production/compose.business.yaml', import.meta.url), 'utf8');
  assert.ok(source.includes('${ACCOUNT_POLICY_RELEASE_ROLE:?set verified ACCOUNT_POLICY_RELEASE_ROLE}'));
  for (const role of ['account_service', 'token_migrator', 'account_platform_admin', 'pg_read_all_stats',
    'account_policy_release_', 'account_policy_release_bad-name', 'account_policy_release_bad%40name',
    'account_policy_release_bad:password', 'account_policy_release_' + 'x'.repeat(64)]) {
    const fixture = businessComposeFixture();
    const env = fixture.config.services['token-governance-check'].environment;
    env.ACCOUNT_POLICY_DATABASE_URL = env.ACCOUNT_POLICY_DATABASE_URL.replace('account_policy_release_fixture@', `${role}@`);
    assert.throws(() => checkProductionBusiness(fixture.config, fixture.inventory), /account_policy_role_invalid/);
  }
  rejects(c => { delete c.services['token-governance-check'].environment.ACCOUNT_POLICY_DATABASE_URL; });
});
test('governance Account identity cannot be reused by either Token runtime identity', () => {
  for (const name of ['token_gateway_fixture', 'token_credential_fixture']) {
    const { config, inventory } = businessComposeFixture();
    for (const service of Object.values(config.services)) {
      if (service.environment.DATABASE_URL) {
        service.environment.DATABASE_URL = service.environment.DATABASE_URL.replace(`${name}@`, 'account_policy_release_fixture@');
      }
    }
    assert.throws(() => checkProductionBusiness(config, inventory), /account_policy_role_shared/);
  }
});
test('production business rejects public binds, port conflicts and secret network unions', () => {
  rejects(c => { c.services['account-service'].ports[0].host_ip = '0.0.0.0'; });
  rejects(c => { c.services['identity-service'].ports[0].published = c.services['account-service'].ports[0].published; });
  rejects(c => { c.services['token-credential-portal'].networks.backend = {}; });
  rejects(c => { c.services['account-event-relay'].networks['business-egress'] = {}; });
  rejects(c => { c.networks['credential-egress'].name = c.networks.backend.name; });
  rejects(c => { c.networks.backend.ipam = { config: [{ subnet: '10.0.0.0/8' }] }; });
  rejects(c => { c.networks.backend.ipam = null; });
});
test('production business rejects secret source overlap, sibling access, writable mounts and target hiding', () => {
  rejects(c => { c.services['identity-service'].volumes.find(v => v.target.endsWith('/workload')).source = c.services['account-service'].volumes.find(v => v.target.endsWith('/workload')).source; });
  rejects(c => { c.services['token-portal'].volumes[0].source = '/fixture/business/mounts'; });
  rejects(c => { c.services['account-service'].volumes[0].read_only = false; });
  rejects(c => { c.services['account-service'].volumes[0].bind.create_host_path = true; });
  rejects(c => { c.services['account-service'].tmpfs.push('/var/run/secrets:rw'); });
  rejects(c => { c.services['token-portal'].volumes.push({ type: 'bind', source: '/fixture/business/env', target: '/copied', read_only: true, bind: { create_host_path: false } }); });
});
test('production business rejects private environment leakage and insecure Redis', () => {
  rejects(c => { c.services['token-credential-portal'].environment.REDIS_URL = c.services['token-portal'].environment.REDIS_URL; });
  rejects(c => { delete c.services['account-service'].environment.H3C_MALL_APP_SECRET; });
  rejects(c => { c.services['identity-service'].environment.AUTH_RISK_REDIS_URL = 'redis://identity:PUBLIC_FIXTURE_ONLY@redis:6379/0'; });
  rejects(c => { c.services['identity-service'].environment.AUTH_RISK_REDIS_URL = c.services['token-portal'].environment.REDIS_URL; });
});
test('production business rejects tool auto-start or modified command', () => {
  rejects(c => { delete c.services['event-resources'].profiles; });
  rejects(c => { c.services['token-governance-check'].command = ['--apply']; });
  rejects(c => { c.services['account-service'].depends_on = { 'event-resources': { condition: 'service_started' } }; });
  rejects(c => { c.services['event-resources'].healthcheck = { test: ['CMD', '/healthcheck'] }; });
});
test('production business checks each actual health port and retains isolation flags', () => {
  for (const domain of ['account', 'billing', 'token']) rejects(c => { c.services[`${domain}-event-relay`].environment.HEALTHCHECK_URL = 'http://127.0.0.1:8080/health/ready'; });
  rejects(c => { c.services['identity-service'].environment.MOBILE_AUTH_ENABLED = 'false'; });
  rejects(c => { c.services['token-gateway'].environment.TOKEN_CREDENTIAL_ISOLATION_ENABLED = 'false'; });
  rejects(c => { c.services['token-portal'].environment.PORTAL_ADMIN_MODEL_DETAILS_ENABLED = 'false'; });
});
test('production business rejects placeholder on-call, insufficient time sources and unbounded settings', () => {
  rejects(c => { for (const s of Object.values(c.services)) if ('MANUAL_REVIEW_ON_CALL_USER_IDS' in s.environment) s.environment.MANUAL_REVIEW_ON_CALL_USER_IDS = 'admin'; });
  rejects(c => { for (const s of Object.values(c.services)) if ('TRUSTED_TIME_SOURCES' in s.environment) s.environment.TRUSTED_TIME_SOURCES = 'one.example.invalid,two.example.invalid'; });
  rejects(c => { c.services['billing-service'].environment.BILLING_ALLOW_TENTATIVE_PRICES = 'true'; });
  rejects(c => { c.services['token-portal'].privileged = true; });
});
