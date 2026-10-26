import assert from 'node:assert/strict';
import { readFileSync, writeFileSync, symlinkSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { currentRenderFixture } from '../fixtures/production-render.mjs';
import { checkProductionBusiness } from '../../../scripts/account-platform-business-compose-check.mjs';
import { checkBusinessMigrations, pgpassRows } from '../../../scripts/account-platform-business-migrations-check.mjs';
import { bytesDigest, mountedSource, parseHelmRender, renderCurrentProduction, workloads } from '../../../scripts/account-platform-production-render.mjs';
import { verifyRenderedKeycloak } from '../../../scripts/account-platform-keycloak-helm-check.mjs';

const compose = { skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1' };
const helm = { skip: process.env.PINVOU_TEST_KMS_HELM !== '1' };
const rejected = (f, check = checkProductionBusiness, code = /production_render_/) => assert.throws(() => check(f.render, f.inventory), code);
function currentWaiverBindings(f) {
  const all = workloads(f.render, f.render.secrets);
  const service = name => all.find(w => (w.kind === 'compose' ? w.id : w.service.name) === name);
  const billing = JSON.parse(service('billing-service').environment.WORKLOAD_IDENTITY_RULES_JSON);
  const token = JSON.parse(service('token-gateway').environment.WORKLOAD_IDENTITY_RULES_JSON);
  assert.ok(billing.find(rule => rule.service === 'token-gateway').permissions.includes('internal.billing.hold.waive'));
  assert.ok(token.find(rule => rule.service === 'billing-service').permissions.includes('internal.token.usage.waive'));
}
async function fixtureCase(t, kind, profile, name, action, settings = {}) {
  await t.test(name, () => { const f = currentRenderFixture(kind, profile, settings); try { action(f); } finally { f.dispose(); } });
}

test('production render parser rejects ambiguous documents and keeps libpq password escapes', () => {
  assert.throws(() => parseHelmRender('apiVersion: v1\nkind: Secret\nkind: Pod\n'), /production_render_yaml_invalid/);
  assert.throws(() => parseHelmRender('apiVersion: v1\nkind: ConfigMap\nmetadata: {name: same}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: same}\n'), /production_render_resource_duplicate/);
  assert.deepEqual(pgpassRows('db:5432:account:account_migrator:escaped\\:password\\\\tail\n'),
    [['db', '5432', 'account', 'account_migrator', 'escaped:password\\tail']]);
  assert.throws(() => pgpassRows('*:5432:account:account_migrator:password\n'), /production_render_pgpass/);
});

test('actual current personal Compose preflight binds fixed templates and real private inputs', compose, async t => {
  await fixtureCase(t, 'compose', 'personal', 'current modular process set and migration-success startup', f => {
    const result = checkProductionBusiness(f.render, f.inventory);
    assert.equal(result.workloads_checked, 17); assert.equal(result.deployment_status, 'NOT_DEPLOYED');
    assert.equal(f.render.config.services['token-gateway'].environment.DATABASE_URL, undefined);
    assert.equal(checkBusinessMigrations(f.render, f.inventory).domains_checked, 2);
    assert.equal(f.render.config.services['billing-service'].environment.BILLING_PAYMENT_PROVIDER, 'development_simulator');
    assert.equal(f.render.config.services['account-service'].depends_on['account-migrate'].condition, 'service_completed_successfully');
  });
  await fixtureCase(t, 'compose', 'personal', 'unapproved pinned image rejected after actual rerender', f => {
    f.updateEnv('TOKEN_GATEWAY_IMAGE', `sha256:${'b'.repeat(64)}`); f.rerender(); rejected(f, undefined, /production_render_image/);
  });
  await fixtureCase(t, 'compose', 'personal', 'actual receiver permission cannot add wildcard', f => {
    const rules = JSON.parse(f.raw.ACCOUNT_SERVICE_PRIVATE_ENV_FILE.WORKLOAD_IDENTITY_RULES_JSON);
    rules[0].permissions.push('internal.*');
    f.updatePrivate('ACCOUNT_SERVICE_PRIVATE_ENV_FILE', 'WORKLOAD_IDENTITY_RULES_JSON', JSON.stringify(rules));
    f.rerender(); rejected(f, undefined, /production_render_workload_rules_unapproved/);
  });
  await fixtureCase(t, 'compose', 'personal', 'foreign actual agent subject rejected', f => {
    const path = `${f.env.ACCOUNT_WORKLOAD_CONFIG_DIRECTORY}/workload.json`, config = JSON.parse(readFileSync(path));
    config.subject = f.subjects['billing-service']; writeFileSync(path, JSON.stringify(config));
    rejected(f, undefined, /production_render_agent_identity/);
  });
  await fixtureCase(t, 'compose', 'personal', 'foreign actual agent issuer rejected', f => {
    const path = `${f.env.ACCOUNT_WORKLOAD_CONFIG_DIRECTORY}/workload.json`, config = JSON.parse(readFileSync(path));
    config.issuer = 'https://other-authority.fixture.invalid'; writeFileSync(path, JSON.stringify(config));
    rejected(f, undefined, /production_render_agent_issuer/);
  });
  await fixtureCase(t, 'compose', 'personal', 'logout-like extra private feature override rejected', f => {
    f.updatePrivate('ACCOUNT_SERVICE_PRIVATE_ENV_FILE', 'UNREVIEWED_FEATURE_ENABLED', 'true'); f.rerender();
    rejected(f, undefined, /production_render_environment_scope/);
  });
  await fixtureCase(t, 'compose', 'personal', 'missing actual startup key rejected', f => {
    f.updatePrivate('ACCOUNT_SERVICE_PRIVATE_ENV_FILE', 'REGISTRATION_QUERY_HMAC_KEY_BASE64', undefined); f.rerender();
    rejected(f, undefined, /production_render_private_value_missing/);
  });
  await fixtureCase(t, 'compose', 'personal', 'Billing on-call requirement follows the actual shared binary', f => {
    f.updatePrivate('BILLING_SERVICE_PRIVATE_ENV_FILE', 'MANUAL_REVIEW_ON_CALL_USER_IDS', undefined); f.rerender();
    rejected(f, undefined, /production_render_private_value_missing/);
  });
  await fixtureCase(t, 'compose', 'personal', 'one canonical on-call owner remains a valid production schedule', f => {
    for (const key of ['ACCOUNT_SERVICE_PRIVATE_ENV_FILE', 'BILLING_SERVICE_PRIVATE_ENV_FILE'])
      f.updatePrivate(key, 'MANUAL_REVIEW_ON_CALL_USER_IDS', f.subjects['account-service']);
    f.rerender(); assert.equal(checkProductionBusiness(f.render, f.inventory).environment_status, 'ENV_PENDING');
  });
  await fixtureCase(t, 'compose', 'personal', 'malformed nil version and variant on-call UUIDs are rejected', f => {
    const valid = f.subjects['account-service'];
    for (const invalid of ['-'.repeat(36), '00000000-0000-0000-0000-000000000000',
      valid.slice(0, 35) + 'z', valid.slice(0, 14) + '0' + valid.slice(15), valid.slice(0, 19) + '0' + valid.slice(20)]) {
      f.updatePrivate('BILLING_SERVICE_PRIVATE_ENV_FILE', 'MANUAL_REVIEW_ON_CALL_USER_IDS', invalid);
      f.rerender(); rejected(f, undefined, /production_render_oncall/);
    }
  });
  await fixtureCase(t, 'compose', 'personal', 'duplicate on-call owners remain rejected in either service', f => {
    for (const key of ['ACCOUNT_SERVICE_PRIVATE_ENV_FILE', 'BILLING_SERVICE_PRIVATE_ENV_FILE']) {
      f.updatePrivate(key, 'MANUAL_REVIEW_ON_CALL_USER_IDS', `${f.subjects['account-service']},${f.subjects['account-service']}`);
      f.rerender(); rejected(f, undefined, /production_render_oncall/);
      f.updatePrivate(key, 'MANUAL_REVIEW_ON_CALL_USER_IDS', f.subjects['account-service']);
    }
  });
  await fixtureCase(t, 'compose', 'personal', 'unknown payment adapter cannot become a silent personal default', f => {
    f.updatePrivate('BILLING_SERVICE_PRIVATE_ENV_FILE', 'BILLING_PAYMENT_PROVIDER', 'missing_adapter'); f.rerender();
    rejected(f, undefined, /production_render_payment_provider/);
  });
  await fixtureCase(t, 'compose', 'personal', 'existing H3C private branch is explicit and requires callback credentials', f => {
    f.updatePrivate('BILLING_SERVICE_PRIVATE_ENV_FILE', 'BILLING_PAYMENT_PROVIDER', 'h3c_mall');
    for (const [key, value] of Object.entries({ H3C_MALL_BASE_URL: 'https://commerce.fixture.invalid', H3C_MALL_APP_KEY: 'public-fixture',
      H3C_MALL_APP_SECRET: 'public-fixture', PAYMENT_MERCHANT_ID: 'public-fixture', PAYMENT_CALLBACK_CLIENT_ID: 'public-fixture',
      PAYMENT_CALLBACK_CLIENT_SECRET: 'public-fixture-only-'.repeat(3), PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64: Buffer.alloc(32, 'f').toString('base64'),
      PAYMENT_CALLBACK_TOKEN_KEY_ID: 'public-fixture-v1' })) f.updatePrivate('BILLING_SERVICE_PRIVATE_ENV_FILE', key, value);
    f.rerender(); assert.equal(checkProductionBusiness(f.render, f.inventory).environment_status, 'ENV_PENDING');
    f.updatePrivate('BILLING_SERVICE_PRIVATE_ENV_FILE', 'PAYMENT_CALLBACK_CLIENT_SECRET', undefined); f.rerender();
    rejected(f, undefined, /production_render_payment_callback/);
  });
  await fixtureCase(t, 'compose', 'personal', 'actual migration DSN cannot use runtime login', f => {
    f.updatePrivate('ACCOUNT_MIGRATION_PRIVATE_ENV_FILE', 'DATABASE_URL', f.raw.ACCOUNT_MIGRATION_PRIVATE_ENV_FILE.DATABASE_URL.replace('account_migrator@', 'account_service@'));
    f.rerender(); rejected(f, checkBusinessMigrations, /production_render_database_identity/);
  });
  await fixtureCase(t, 'compose', 'personal', 'actual runtime DSN cannot use migrator login', f => {
    f.updatePrivate('ACCOUNT_SERVICE_PRIVATE_ENV_FILE', 'DATABASE_URL', f.raw.ACCOUNT_SERVICE_PRIVATE_ENV_FILE.DATABASE_URL.replace('account_service@', 'account_migrator@'));
    f.rerender(); rejected(f, undefined, /production_render_database_identity/);
  });
  await fixtureCase(t, 'compose', 'personal', 'TLS cannot be relaxed in actual private DSN', f => {
    f.updatePrivate('ACCOUNT_MIGRATION_PRIVATE_ENV_FILE', 'DATABASE_URL', f.raw.ACCOUNT_MIGRATION_PRIVATE_ENV_FILE.DATABASE_URL.replace('verify-full', 'require'));
    f.rerender(); rejected(f, checkBusinessMigrations, /production_render_database_tls/);
  });
  await fixtureCase(t, 'compose', 'personal', 'actual backup manifest bytes must match inventory hash', f => {
    writeFileSync(f.backup.manifestFile, 'changed public fixture\n'); rejected(f, checkBusinessMigrations, /production_render_backup_manifest/);
  });
  await fixtureCase(t, 'compose', 'personal', 'stale evidence rejected against actual migrate semantics', f => {
    rejected(f, (r, i) => checkBusinessMigrations(r, i, Date.now() + 86400001), /production_render_backup_stale/);
  });
  await fixtureCase(t, 'compose', 'personal', 'mutation of final command cannot bypass current fixed source', f => {
    f.render.config.services['account-migrate'].command.push('--direction', 'down');
    rejected(f, checkBusinessMigrations, /production_render_template_changed/);
  });
});

test('actual current models Compose keeps migrator credentials out of runtime mounts', compose, async t => {
  await fixtureCase(t, 'compose', 'models', 'three domains and separate actual Token pgpass files', f => {
    currentWaiverBindings(f);
    const all = workloads(f.render), migration = all.find(w => w.id === 'token-migrate'), runtime = all.find(w => w.id === 'token-gateway');
    const a = mountedSource(migration, migration.environment.PGPASSFILE), b = mountedSource(runtime, runtime.environment.PGPASSFILE);
    assert.notEqual(a, b);
    assert.equal(checkProductionBusiness(f.render, f.inventory).workloads_checked, 23);
    assert.equal(checkBusinessMigrations(f.render, f.inventory).domains_checked, 3);
    assert.equal(runtime.service.depends_on['token-migrate'].condition, 'service_completed_successfully');
  });
  for (const [label, value] of [['missing', undefined], ['empty', '']])
    await fixtureCase(t, 'compose', 'models', `${label} credential portal SQL DSN rejected after actual rerender`, f => {
      assert.equal(checkProductionBusiness(f.render, f.inventory).decision, 'BUSINESS_CONFIG_VALID');
      f.updatePrivate('TOKEN_CREDENTIAL_PRIVATE_ENV_FILE', 'DATABASE_URL', value); f.rerender();
      rejected(f, undefined, /production_render_(runtime_database_missing|private_value_missing)/);
    });
  await fixtureCase(t, 'compose', 'models', 'real renderer refuses missing separate Token migrator directory', f => {
    f.updateEnv('TOKEN_MIGRATION_DATABASE_DIRECTORY', ''); assert.throws(() => f.rerender(), /production_render_renderer_failed/);
  });
  await fixtureCase(t, 'compose', 'models', 'runtime readable pgpass cannot contain migrator credentials', f => {
    writeFileSync(`${f.env.TOKEN_DATABASE_DIRECTORY}/pgpass`,
      'postgres.fixture.invalid:5432:account_platform_token:token_gateway:public-runtime\npostgres.fixture.invalid:5432:account_platform_token:token_migrator:public-migrator\n');
    rejected(f, undefined, /production_render_runtime_pgpass_identity/);
  });
  await fixtureCase(t, 'compose', 'models', 'overlapping secret directory cannot hide behind another spelling', f => {
    f.updateEnv('TOKEN_MIGRATION_DATABASE_DIRECTORY', f.env.TOKEN_DATABASE_DIRECTORY); f.rerender();
    rejected(f, undefined, /production_render_shared_secret/);
  });
  await fixtureCase(t, 'compose', 'models', 'KMS bootstrap directory cannot become gateway token source', f => {
    f.updateEnv('TOKEN_KMS_TOKEN_DIRECTORY', f.env.TOKEN_WORKLOAD_BOOTSTRAP_DIRECTORY); f.rerender();
    rejected(f, undefined, /production_render_shared_secret/);
  });
  await fixtureCase(t, 'compose', 'models', 'physical symlink cannot disguise a runtime credential directory', f => {
    const alias = `${f.directory}/credential-alias`;
    symlinkSync(f.env.TOKEN_DATABASE_DIRECTORY, alias, process.platform === 'win32' ? 'junction' : 'dir');
    f.updateEnv('TOKEN_MIGRATION_DATABASE_DIRECTORY', alias.replaceAll('\\', '/')); f.rerender();
    rejected(f, undefined, /production_render_source_symlink/);
  });
});

test('actual production Helm migration hooks bind resolved Secret keys and backup evidence', helm, async t => {
  await fixtureCase(t, 'helm', 'models', 'all current workloads and three original hooks validated', f => {
    currentWaiverBindings(f);
    // Ordinary Token events are retired. Keep the two remaining relays and
    // every runtime, trust preparation, TLS sidecar and migration container.
    const expectedContainers = ['account-service', 'billing-service', 'identity-service',
      'token-gateway', 'token-portal', 'new-api', 'newapi-tls-data', 'newapi-tls-status-only',
      'prepare-keycloak-secret', 'prepare-nats-billing-command-executor', 'prepare-nats-account-event-relay',
      'prepare-nats-account-approval-relay', 'prepare-nats-billing-event-relay',
      'event-relay', 'event-relay', 'migrate', 'migrate', 'migrate'];
    const rendered = workloads(f.render, f.render.secrets);
    assert.deepEqual(rendered.map(w => w.service.name).sort(), expectedContainers.slice().sort());
    assert.deepEqual(rendered.filter(w => w.service.name === 'event-relay').map(w =>
      w.id.match(/-([a-z]+)-event-relay\/container\/event-relay$/)?.[1]).sort(), ['account', 'billing']);
    assert.equal(checkProductionBusiness(f.render, f.inventory).workloads_checked, expectedContainers.length);
    assert.equal(checkBusinessMigrations(f.render, f.inventory).domains_checked, 3);
  });
  for (const [label, value] of [['missing', undefined], ['empty', '']])
    await fixtureCase(t, 'helm', 'models', `${label} enabled credential portal SQL Secret DSN rejected after actual rerender`, f => {
      const portal = workloads(f.render, f.render.secrets).find(w => w.service.name === 'token-credential-portal');
      assert.ok(portal); assert.equal(checkProductionBusiness(f.render, f.inventory).decision, 'BUSINESS_CONFIG_VALID');
      const ref = portal.refs.DATABASE_URL, key = `${f.recipe.namespace}/${ref.name}/${ref.key}`;
      const secrets = JSON.parse(readFileSync(f.recipe.secretSnapshotFile));
      if (value === undefined) delete secrets[key]; else secrets[key] = value;
      writeFileSync(f.recipe.secretSnapshotFile, JSON.stringify(secrets)); f.rerender();
      rejected(f, undefined, /production_render_(runtime_database_missing|private_value_missing|secret_missing)/);
    }, { credentialPortal: true });
  await fixtureCase(t, 'helm', 'models', 'specific migration Secret value cannot select runtime login', f => {
    const key = Object.keys(f.render.secrets).find(k => k.endsWith('/account-migration-database-url'));
    f.render.secrets[key] = f.render.secrets[key].replace('account_migrator:', 'account_service:');
    rejected(f, checkBusinessMigrations, /production_render_database_identity/);
  });
  await fixtureCase(t, 'helm', 'models', 'optional true backup Secret missing still rejects production migration', f => {
    delete f.render.secrets[Object.keys(f.render.secrets).find(k => k.endsWith('/migration-backup-sha256'))];
    rejected(f, checkBusinessMigrations, /production_render_backup_binding/);
  });
  await fixtureCase(t, 'helm', 'models', 'hook cannot silently become ordinary Job', f => {
    const job = f.render.config.find(r => r.kind === 'Job' && r.metadata.name.includes('-migrate-account-'));
    delete job.metadata.annotations['helm.sh/hook']; rejected(f, checkBusinessMigrations, /production_render_template_changed/);
  });
  await fixtureCase(t, 'helm', 'models', 'current inherited migration limits cannot be absent', f => {
    const job = f.render.config.find(r => r.kind === 'Job' && r.metadata.name.includes('-migrate-account-'));
    const original = f.render.template.find(r => r.metadata.name === job.metadata.name && r.kind === 'Job');
    delete job.spec.template.spec.containers[0].resources; delete original.spec.template.spec.containers[0].resources;
    rejected(f, checkBusinessMigrations, /production_render_migration_resources/);
  });
  await fixtureCase(t, 'helm', 'models', 'actual site values cannot disable release-wide default deny', f => {
    const values = JSON.parse(readFileSync(f.recipe.valuesFile)); values.networkPolicy = { enabled: false };
    writeFileSync(f.recipe.valuesFile, JSON.stringify(values));
    assert.throws(() => { f.rerender(); checkProductionBusiness(f.render, f.inventory); },
      /production_render_(renderer_failed|network_default_deny)/);
    // Also retain the checker boundary if a future template/schema accidentally
    // omits the policy instead of refusing the values before rendering.
    for (const key of ['config', 'template']) f.render[key] = f.render[key].filter(r =>
      !(r.kind === 'NetworkPolicy' && r.metadata.name.endsWith('-default-deny')));
    rejected(f, undefined, /production_render_network_default_deny/);
  });
});

test('actual current Helm preserves original Keycloak management health and isolated sticky/token routing', helm, async t => {
  const f = currentRenderFixture('helm', 'models');
  try {
    const values = JSON.parse(readFileSync(f.recipe.valuesFile));
    values.keycloak.enabled = true; values.keycloak.realmUpgradeEnabled = true;
    values.keycloak.tlsSecretName = 'public-fixture-keycloak-tls';
    values.public = { ...values.public, portalUrl: 'https://portal.fixture.invalid' };
    writeFileSync(f.recipe.valuesFile, JSON.stringify(values));
    const actual = f.rerender().config;
    await t.test('actual current Keycloak enabled render', () => assert.equal(verifyRenderedKeycloak(actual).deployment_status, 'NOT_DEPLOYED'));
    await t.test('realm upgrade Job supplies the public Portal URL required by its actual entrypoint', () => {
      const job = actual.find(r => r.kind === 'Job' && r.metadata.name.endsWith('-keycloak-realm-upgrade'));
      assert.ok(job, 'enabled realm upgrade Job must be rendered');
      const container = job.spec.template.spec.containers.find(c => c.name === 'realm-upgrade');
      assert.deepEqual(container.command, ['/opt/keycloak/bin/pinvou-provision-mobile-auth.sh']);
      const portal = container.env.filter(entry => entry.name === 'PORTAL_PUBLIC_URL');
      assert.deepEqual(portal, [{ name: 'PORTAL_PUBLIC_URL', value: values.public.portalUrl }]);
      const entrypoint = readFileSync(new URL('../../deploy/keycloak/provision-mobile-auth.sh', import.meta.url), 'utf8');
      assert.match(entrypoint, /\$\{PORTAL_PUBLIC_URL:\?/);
    });
    for (const [name, mutate, code] of [
      ['wrong management port', c => { c.ports.find(p => p.name === 'management').containerPort = 8080; }, 'keycloak_health'],
      ['wrong startup path', c => { c.startupProbe.httpGet.path = '/health/ready'; }, 'keycloak_health'],
      ['wrong readiness port', c => { c.readinessProbe.httpGet.port = 'http'; }, 'keycloak_health'],
    ]) await t.test(name, () => {
      const copy = structuredClone(actual), deployment = copy.find(r => r.kind === 'Deployment' && r.metadata.name.endsWith('-keycloak'));
      mutate(deployment.spec.template.spec.containers.find(c => c.name === 'keycloak'));
      assert.throws(() => verifyRenderedKeycloak(copy), new RegExp(`production_render_${code}`));
    });
    await t.test('sticky paths cannot swallow token proxy', () => {
      const copy = structuredClone(actual), ingress = copy.find(r => r.kind === 'Ingress' && r.metadata.name.endsWith('-keycloak-session'));
      ingress.spec.rules[0].http.paths.push({ ...ingress.spec.rules[0].http.paths[0], path: '/' });
      assert.throws(() => verifyRenderedKeycloak(copy), /production_render_keycloak_session_scope/);
    });
    await t.test('token proxy cannot route directly to Keycloak', () => {
      const copy = structuredClone(actual), ingress = copy.find(r => r.kind === 'Ingress' && r.spec.rules?.some(rule =>
        rule.http.paths.some(path => path.path === '/realms/pinvou/protocol/openid-connect/token')));
      const rule = ingress.spec.rules.find(rule => rule.http.paths.some(path => path.path === '/realms/pinvou/protocol/openid-connect/token'));
      rule.http.paths[0].backend.service.name = rule.http.paths[0].backend.service.name.replace(/-identity-service$/, '-keycloak');
      assert.throws(() => verifyRenderedKeycloak(copy), /production_render_keycloak_token_proxy/);
    });
    await t.test('shared ingress cannot acquire cookie affinity', () => {
      const copy = structuredClone(actual), ingress = copy.find(r => r.kind === 'Ingress' && r.spec.rules?.some(rule =>
        rule.http.paths.some(path => path.path === '/realms/pinvou/protocol/openid-connect/token')));
      ingress.metadata.annotations = { 'nginx.ingress.kubernetes.io/affinity': 'cookie' };
      assert.throws(() => verifyRenderedKeycloak(copy), /production_render_keycloak_token_proxy/);
    });
    await t.test('standalone original manual checker CLI', () => {
      // Rendered Helm contains Secret references, not the private snapshot values.
      const path = f.file('public-keycloak-render.yaml', actual.map(r => JSON.stringify(r)).join('\n---\n'));
      const script = fileURLToPath(new URL('../../../scripts/account-platform-keycloak-helm-check.mjs', import.meta.url));
      const result = spawnSync(process.execPath, [script, path], { encoding: 'utf8', timeout: 30000, windowsHide: true });
      assert.equal(result.status, 0, result.stderr); assert.equal(JSON.parse(result.stdout).decision, 'KEYCLOAK_RENDER_VALID');
    });
  } finally { f.dispose(); }
});

test('manual production preflight CLIs use only pinned renderer config/template and redact input failures', {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1' || process.env.PINVOU_TEST_KMS_HELM !== '1',
}, async t => {
  for (const [kind, profile] of [['compose', 'personal'], ['helm', 'models']]) for (const script of ['business-compose', 'business-migrations']) {
    await fixtureCase(t, kind, profile, `${kind} ${script} CLI`, f => {
      const recipe = f.file('recipe.json', JSON.stringify(f.recipe)), inventory = f.file('inventory.json', JSON.stringify(f.inventory));
      const binary = kind === 'compose' ? process.env.PINVOU_TEST_KMS_COMPOSE_BINARY : process.env.PINVOU_TEST_KMS_HELM_BINARY;
      const path = fileURLToPath(new URL(`../../../scripts/account-platform-${script}-check.mjs`, import.meta.url));
      const result = spawnSync(process.execPath, [path, '--recipe', recipe, '--inventory', inventory, '--renderer', binary],
        { encoding: 'utf8', timeout: 30000, windowsHide: true });
      assert.equal(result.status, 0, result.stderr);
      const output = JSON.parse(result.stdout); assert.equal(output.environment_status, 'ENV_PENDING'); assert.equal(output.migrations_executed, false);
      assert.equal(result.stdout.includes('public-fixture-password'), false); assert.equal(result.stderr, '');
      f.inventory.images[Object.keys(f.inventory.images)[0]] = 'DO_NOT_PRINT_PRIVATE_INPUT';
      writeFileSync(inventory, JSON.stringify(f.inventory));
      const invalid = spawnSync(process.execPath, [path, '--recipe', recipe, '--inventory', inventory, '--renderer', binary],
        { encoding: 'utf8', timeout: 30000, windowsHide: true });
      assert.equal(invalid.status, 1); assert.equal(invalid.stdout, '');
      assert.equal(invalid.stderr, 'production_render_image\n');
    });
  }
  await fixtureCase(t, 'compose', 'personal', 'unrecognized recipe cannot supply arbitrary compose file or shell', f => {
    f.recipe.overlays = ['../../DO_NOT_PRINT_PRIVATE_INPUT'];
    assert.throws(() => renderCurrentProduction(f.recipe, { binary: process.env.PINVOU_TEST_KMS_COMPOSE_BINARY,
      binarySHA256: bytesDigest(readFileSync(process.env.PINVOU_TEST_KMS_COMPOSE_BINARY)) }), /production_render_composition/);
  });
  await fixtureCase(t, 'compose', 'personal', 'existing root CLI reaches the actual offline business checker', f => {
    const recipe = f.file('recipe-root.json', JSON.stringify(f.recipe)), inventory = f.file('inventory-root.json', JSON.stringify(f.inventory));
    const script = fileURLToPath(new URL('../../../scripts/account-platform.mjs', import.meta.url));
    const result = spawnSync(process.execPath, [script, 'production', 'check-business', '--recipe', recipe, '--inventory', inventory,
      '--renderer', process.env.PINVOU_TEST_KMS_COMPOSE_BINARY], { encoding: 'utf8', timeout: 30000, windowsHide: true });
    assert.equal(result.status, 0, result.stderr); assert.equal(JSON.parse(result.stdout).decision, 'BUSINESS_CONFIG_VALID');
  });
});

test('production Helm requires explicit operator on-call users instead of inherited examples', helm, () => {
  const f = currentRenderFixture('helm', 'models');
  try {
    const values = JSON.parse(readFileSync(f.recipe.valuesFile, 'utf8'));
    const explicit = values.global.manualReviewOnCallUserIds;
    assert.ok(Array.isArray(explicit) && explicit.length > 0, 'the existing positive fixture must explicitly configure on-call users');
    delete values.global.manualReviewOnCallUserIds;
    writeFileSync(f.recipe.valuesFile, JSON.stringify(values));
    assert.throws(() => f.rerender(), /production_render_renderer_failed/);
    values.global.manualReviewOnCallUserIds = explicit;
    writeFileSync(f.recipe.valuesFile, JSON.stringify(values));
    f.rerender();
    const result = checkProductionBusiness(f.render, f.inventory);
    assert.equal(result.decision, 'BUSINESS_CONFIG_VALID');
    const configured = workloads(f.render, f.render.secrets)
      .filter(w => w.environment.MANUAL_REVIEW_ON_CALL_USER_IDS !== undefined);
    assert.ok(configured.length > 0);
    for (const w of configured) assert.equal(w.environment.MANUAL_REVIEW_ON_CALL_USER_IDS, explicit.join(','));
  } finally { f.dispose(); }
});
