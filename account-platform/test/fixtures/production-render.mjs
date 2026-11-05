// Public, disposable config-only fixtures. These are never deployment credentials.
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { createRequire } from 'node:module';
import { createPrivateKey, createPublicKey } from 'node:crypto';
import { bytesDigest, composeCompositions, renderCurrentProduction, repositoryRoot, workloads } from '../../../scripts/account-platform-production-render.mjs';
const require = createRequire(new URL('../../package.json', import.meta.url));
const YAML = require('yaml');
const names = ['account-service', 'identity-service', 'billing-service', 'token-gateway',
  'token-command-executor', 'token-credential-portal', 'keycloak-mobile-authenticator', 'account-approval-recovery'];
export function currentRenderFixture(kind, profile = 'personal', settings = {}) {
  const directory = mkdtempSync(join(tmpdir(), 'pinvou-public-render-'));
  try {
  const file = (name, content) => {
    const path = join(directory, name); mkdirSync(resolve(path, '..'), { recursive: true });
    writeFileSync(path, content, { mode: 0o600 }); return path.replaceAll('\\', '/');
  };
  const subjects = Object.fromEntries(names.map((name, index) => [name,
    `10000000-0000-4000-8000-${String(index + 1).padStart(12, '0')}`]));
  const publicKey = createPublicKey(createPrivateKey({ key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.alloc(32, 'f')]),
    format: 'der', type: 'pkcs8' })).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64url');
  const backup = { id: 'public-fixture-backup-0001', completedAt: new Date(Math.floor(Date.now() / 1000) * 1000 - 1000).toISOString().replace('.000Z', 'Z') };
  const manifest = '{"fixture":"configuration-only; no backup was executed"}\n';
  backup.sha256 = bytesDigest(manifest); backup.manifestFile = file('backup-manifest.json', manifest);
  const databases = Object.fromEntries((kind === 'compose' && profile === 'personal' ? ['account', 'billing'] : ['account', 'billing', 'token'])
    .map(domain => [domain, { host: 'postgres.fixture.invalid', port: 5432, name: `account_platform_${domain}` }]));
  const dbURL = (domain, login, password = '') => `postgresql://${login}${password ? `:${password}` : ''}@postgres.fixture.invalid:5432/account_platform_${domain}?sslmode=verify-full&sslrootcert=${kind === 'helm' ? 'system' : '/run/pinvou/trust/ca.crt'}`;
  const rule = domain => JSON.parse(readFileSync(new URL(`../../deploy/production/workload-${domain}-rules.json.template`, import.meta.url), 'utf8'))
    .map(r => ({ ...r, subject: subjects[r.service] }));
  const fixture = { directory, subjects, backup, databases, file, dispose: () => rmSync(directory, { recursive: true, force: true }) };
  const binary = kind === 'compose' ? process.env.PINVOU_TEST_KMS_COMPOSE_BINARY : process.env.PINVOU_TEST_KMS_HELM_BINARY;
  const rendererSHA256 = bytesDigest(readFileSync(binary));
  const options = { binary, binarySHA256: rendererSHA256 };
  if (kind === 'compose') {
    const texts = composeCompositions[profile].map(name => readFileSync(resolve(repositoryRoot, `account-platform/deploy/production/compose.${name}.yaml`), 'utf8'));
    const variables = [...new Set(texts.flatMap(text => [...text.matchAll(/\$\{([A-Z0-9_]+)/g)].map(m => m[1])))];
    const env = {};
    let ip = 10;
    for (const key of variables) {
      let value = 'public-fixture';
      if (key.endsWith('_IMAGE')) value = `sha256:${'a'.repeat(64)}`;
      else if (key.endsWith('_DIRECTORY')) { value = join(directory, key.toLowerCase()).replaceAll('\\', '/'); mkdirSync(value, { recursive: true }); }
      else if (key.endsWith('_FILE')) value = file(key.toLowerCase(), '# public fixture only\n');
      else if (key.endsWith('_SHA256') || key.endsWith('_DIGEST')) value = 'a'.repeat(64);
      else if (key.endsWith('_NETWORK')) value = `public-fixture-${key.toLowerCase().replaceAll('_', '-')}`;
      else if (key.endsWith('_IP') || key.endsWith('_ADDRESS')) value = `10.88.0.${ip++}`;
      else if (key.endsWith('_PORT')) value = '8443';
      else if (key.endsWith('_URL') || key.endsWith('_ENDPOINT') || key.endsWith('_ISSUER') || key.endsWith('_ORIGIN')) value = 'https://fixture.invalid';
      env[key] = value;
    }
    Object.assign(env, { TRUSTED_TIME_SOURCES: 'time1.fixture.invalid:123,time2.fixture.invalid:123,time3.fixture.invalid:123',
      NATS_URL: 'tls://nats.fixture.invalid:4222', USER_OIDC_ISSUER: 'https://fixture.invalid/realms/pinvou',
      USER_OIDC_JWKS_URL: 'https://fixture.invalid/realms/pinvou/protocol/openid-connect/certs',
      USER_OIDC_AUTHORIZATION_URL: 'https://fixture.invalid/realms/pinvou/protocol/openid-connect/auth',
      USER_OIDC_TOKEN_URL: 'https://fixture.invalid/realms/pinvou/protocol/openid-connect/token',
      USER_OIDC_AUDIENCE: 'pinvou-desktop', PERSONAL_PUBLIC_HOST: 'fixture.invalid', ACCOUNT_PLATFORM_PROFILE: profile,
      TOKEN_PROVIDER_ALLOWED_HOSTS: 'provider.fixture.invalid', CREDENTIAL_OIDC_CLIENT_ID: 'pinvou-credentials',
      WORKLOAD_IDENTITY_ISSUER: 'https://fixture.invalid/realms/pinvou-workload',
      WORKLOAD_IDENTITY_JWKS_URL: 'https://fixture.invalid/realms/pinvou-workload/protocol/openid-connect/certs' });
    const raw = {};
    for (const domain of Object.keys(databases)) {
      raw[`${domain.toUpperCase()}_MIGRATION_PRIVATE_ENV_FILE`] = {
        DATABASE_URL: dbURL(domain, `${domain}_migrator`),
        MIGRATION_ROLE: domain === 'token' ? 'token_gateway' : `${domain}_service`,
        MIGRATION_BACKUP_EVIDENCE_ID: backup.id, MIGRATION_BACKUP_COMPLETED_AT: backup.completedAt, MIGRATION_BACKUP_SHA256: backup.sha256,
      };
    }
    const key = Buffer.alloc(32, 'f');
    raw.ACCOUNT_SERVICE_PRIVATE_ENV_FILE = { DATABASE_URL: dbURL('account', 'account_service'), WORKLOAD_IDENTITY_RULES_JSON: JSON.stringify(rule('account')),
      OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64: key.toString('base64'), HUMAN_DELEGATION_SIGNING_KEY_ID: 'public-fixture-v1',
      HUMAN_DELEGATION_SIGNING_KEY_BASE64URL: key.toString('base64url'), REGISTRATION_QUERY_HMAC_KEY_BASE64: key.toString('base64').replace(/=+$/, ''),
      SMS_VERIFICATION_HMAC_KEY_BASE64: key.toString('base64').replace(/=+$/, ''), REGISTRATION_BONUS_FINGERPRINT_HMAC_KEY_BASE64: key.toString('base64'),
      SMS_PROVIDER: 'http-generated', SMS_HTTP_ENDPOINT: 'https://sms.fixture.invalid', SMS_HTTP_BEARER_TOKEN: 'public-fixture-only',
      MANUAL_REVIEW_ON_CALL_USER_IDS: `${subjects['account-service']},${subjects['billing-service']}` };
    raw.IDENTITY_SERVICE_PRIVATE_ENV_FILE = { WORKLOAD_IDENTITY_RULES_JSON: JSON.stringify(rule('identity')),
      IDENTITY_PASSWORD_HMAC_KEY_BASE64: key.toString('base64').replace(/=+$/, ''), AUTH_RISK_HMAC_KEY_BASE64: key.toString('base64').replace(/=+$/, ''),
      AUTH_RISK_REDIS_URL: 'rediss://identity:fixture@redis.fixture.invalid:6379/1' };
    raw.BILLING_SERVICE_PRIVATE_ENV_FILE = { DATABASE_URL: dbURL('billing', 'billing_service'), WORKLOAD_IDENTITY_RULES_JSON: JSON.stringify(rule('billing')),
      MANUAL_REVIEW_ON_CALL_USER_IDS: `${subjects['account-service']},${subjects['billing-service']}`,
      HUMAN_DELEGATION_VERIFICATION_KEYS_JSON: JSON.stringify({ 'public-fixture-v1': publicKey }),
      BILLING_PAYMENT_PROVIDER: 'development_simulator', PAYMENT_PROVIDER_BASE_URL: 'https://payment.fixture.invalid' };
    raw.TOKEN_GATEWAY_PRIVATE_ENV_FILE = profile === 'personal' ? {} : { DATABASE_URL: dbURL('token', 'token_gateway'), WORKLOAD_IDENTITY_RULES_JSON: JSON.stringify(rule('token')),
      TOKEN_DEFAULT_PROVIDER: 'NEW_API', TOKEN_DEFAULT_PROVIDER_BASE_URL: 'https://provider.fixture.invalid', TOKEN_DEFAULT_PROVIDER_API_KEY: 'public-fixture-only' };
    raw.TOKEN_PORTAL_PRIVATE_ENV_FILE = { REDIS_URL: 'rediss://portal:fixture@redis.fixture.invalid:6379/0' };
    raw.EVENT_RELAY_PRIVATE_ENV_FILE = { DATABASE_URL: dbURL('account', 'account_service') };
    if (profile === 'models') {
      raw.BILLING_RELAY_PRIVATE_ENV_FILE = { DATABASE_URL: dbURL('billing', 'billing_service') };
      raw.APPROVAL_INITIALIZER_PRIVATE_ENV_FILE = { DATABASE_URL: dbURL('account', 'account_migrator') };
      raw.TOKEN_CREDENTIAL_PRIVATE_ENV_FILE = { DATABASE_URL: dbURL('token', 'token_gateway') };
    }
    for (const [key, values] of Object.entries(raw)) if (env[key]) writeFileSync(env[key], Object.entries(values).map(([k, v]) => `${k}=${v}`).join('\n') + '\n');
    const configs = { ACCOUNT: 'account-service', IDENTITY: 'identity-service', BILLING: 'billing-service', TOKEN: 'token-gateway',
      KEYCLOAK: 'keycloak-mobile-authenticator', CREDENTIAL: 'token-credential-portal', TOKEN_EXECUTOR: 'token-command-executor' };
    for (const [prefix, consumer] of Object.entries(configs)) {
      const config = env[`${prefix}_WORKLOAD_CONFIG_DIRECTORY`];
      if (config) {
        writeFileSync(join(config, 'workload.json'), JSON.stringify({ issuer: env.WORKLOAD_IDENTITY_ISSUER,
          audience: 'pinvou-account-platform-internal', client_id: consumer, subject: subjects[consumer],
          client_secret_file: '/run/bootstrap/client-secret', token_file: '/run/projected/token' }), { mode: 0o600 });
      }
    }
    writeFileSync(join(env.INFRA_CA_DIRECTORY, 'ca.crt'), 'PUBLIC FIXTURE CA; no TLS handshake performed\n');
    const logins = { ACCOUNT_MIGRATION: ['account', 'account_migrator'], BILLING_MIGRATION: ['billing', 'billing_migrator'],
      TOKEN_MIGRATION: ['token', 'token_migrator'], ACCOUNT_SERVICE: ['account', 'account_service'], BILLING_SERVICE: ['billing', 'billing_service'],
      TOKEN: ['token', 'token_gateway'], EVENT_RELAY: ['account', 'account_service'], BILLING_RELAY: ['billing', 'billing_service'],
      APPROVAL_INITIALIZER: ['account', 'account_migrator'] };
    for (const [prefix, [domain, login]] of Object.entries(logins)) {
      const path = env[`${prefix}_DATABASE_DIRECTORY`];
      if (path) writeFileSync(join(path, 'pgpass'), `postgres.fixture.invalid:5432:account_platform_${domain}:${login}:public-fixture-password\n`, { mode: 0o600 });
    }
    // These public bytes and independently supplied fixture digests exercise
    // offline binding only; real bootstrap semantics are covered in Go, and
    // this configuration fixture claims no broker cutover or resource readiness.
    for (const [variable, filename, hash] of [
      ['APPROVAL_PUBLIC_IDENTITY_DIRECTORY', 'current.json', 'APPROVAL_PUBLIC_KEYS_SHA256'],
      ['APPROVAL_PUBLIC_MANIFEST_DIRECTORY', 'manifest.json', 'APPROVAL_MANIFEST_SHA256'],
      ['APPROVAL_PUBLIC_RECEIPT_DIRECTORY', 'receipt.json', 'APPROVAL_RECEIPT_SHA256'],
    ]) {
      if (!env[variable]) continue;
      const bytes = JSON.stringify({ fixture: 'configuration-only', artifact: filename });
      writeFileSync(join(env[variable], filename), bytes, { mode: 0o444 });
      env[hash] = bytesDigest(bytes);
    }
    const envFile = file('compose.env', Object.entries(env).map(([k, v]) => `${k}=${v}`).join('\n') + '\n');
    fixture.recipe = { kind, profile, envFile, overlays: [] };
    fixture.env = env; fixture.raw = raw;
    fixture.updateEnv = (key, value) => {
      env[key] = value;
      writeFileSync(envFile, Object.entries(env).map(([k, v]) => `${k}=${v}`).join('\n') + '\n');
    };
    fixture.updatePrivate = (key, name, value) => {
      if (value === undefined) delete raw[key][name]; else raw[key][name] = value;
      writeFileSync(env[key], Object.entries(raw[key]).map(([k, v]) => `${k}=${v}`).join('\n') + '\n');
    };
  } else {
    const defaults = YAML.parse(readFileSync(resolve(repositoryRoot, 'account-platform/deploy/helm/account-platform/values.yaml'), 'utf8'));
    const images = Object.fromEntries(Object.keys(defaults.images).map(key => [key,
      key === 'newApiTLS' ? { repository: 'registry.fixture.invalid/newapi-tls', digest: `sha256:${'a'.repeat(64)}` }
        : key === 'newApi' ? { repository: 'registry.fixture.invalid/newapi', tag: 'fixture', digest: `sha256:${'a'.repeat(64)}` }
          : { repository: `registry.fixture.invalid/${key.toLowerCase()}`, tag: 'fixture', digest: `sha256:${'a'.repeat(64)}` }]));
    fixture.recipe = { kind, namespace: 'public-fixture', release: 'public-fixture',
      valuesFile: file('helm-values.json', JSON.stringify({ images,
        global: { manualReviewOnCallUserIds: [subjects['account-service']] },
        ...(settings.credentialPortal ? { public: { portalUrl: 'https://account.pinvou.com' },
          tokenGateway: { runtimeSecret: 'public-fixture-gateway' },
          tokenCredentialPortal: { enabled: true, runtimeSecret: 'public-fixture-credential',
            publicUrl: 'https://credentials.pinvou.com', oidcAuthorizationUrl: 'https://accounts.example.com/realms/pinvou/protocol/openid-connect/auth',
            oidcTokenUrl: 'https://accounts.example.com/realms/pinvou/protocol/openid-connect/token',
            oidcJwksUrl: 'https://accounts.example.com/realms/pinvou/protocol/openid-connect/certs',
            oidcClientId: 'public-fixture-credential', oidcAudience: 'public-fixture-credential', egressCidrs: ['192.0.2.10/32'] },
          tokenCommandExecutor: { natsCidrs: ['192.0.2.30/32'] } } : {}), newApi: { enabled: true,
        tlsSecretName: 'public-fixture-newapi-tls', caConfigMap: 'public-fixture-newapi-ca' }, keycloak: { enabled: false,
          adminApiUrl: 'https://private-keycloak.fixture.invalid',
          provisionerTokenUrl: 'https://private-keycloak.fixture.invalid/realms/pinvou/protocol/openid-connect/token',
          caConfigMap: 'public-fixture-keycloak-ca' },
        oidc: { jwksUrl: 'https://private-keycloak.fixture.invalid/realms/pinvou/protocol/openid-connect/certs',
          tokenUrl: 'https://private-keycloak.fixture.invalid/realms/pinvou/protocol/openid-connect/token' },
        account: { commerceAdapterBaseUrl: 'https://commerce.fixture.invalid' },
        redis: { enabled: false }, nats: { enabled: false } })), secretSnapshotFile: file('private-secrets.json', '{}') };
  }
  fixture.render = renderCurrentProduction(fixture.recipe, options);
  if (kind === 'helm') {
    const secrets = {};
    for (const resource of fixture.render.config) {
      const pod = resource.spec?.template?.spec || resource.spec?.jobTemplate?.spec?.template?.spec;
      for (const container of [...(pod?.containers || []), ...(pod?.initContainers || [])]) for (const env of container.env || []) {
        const ref = env.valueFrom?.secretKeyRef; if (!ref) continue;
        let value = 'public-fixture-only';
        const domain = container.name === 'token-credential-portal' ? 'token'
          : ['account', 'billing', 'token'].find(d => ref.key.startsWith(`${d}-`));
        if (ref.key.endsWith('migration-database-url')) value = dbURL(domain, `${domain}_migrator`, 'public-fixture-password');
        else if (ref.key.endsWith('database-url')) value = dbURL(domain || 'account', domain === 'token' ? 'token_gateway' : `${domain || 'account'}_service`, 'public-fixture-password');
        else if (ref.key === 'migration-backup-evidence-id') value = backup.id;
        else if (ref.key === 'migration-backup-completed-at') value = backup.completedAt;
        else if (ref.key === 'migration-backup-sha256') value = backup.sha256;
        // Independent config-only digest values; no public artifact or resource readiness is asserted.
        else if (/^BILLING_APPROVAL_CONSUMER_.*_SHA256$/.test(env.name)) value = 'a'.repeat(64);
        else if (/REDIS_URL/.test(env.name)) value = 'rediss://fixture:fixture@redis.fixture.invalid:6379/0';
        else if (env.name === 'NATS_URL') value = 'tls://nats.fixture.invalid:4222';
        else if (env.name.endsWith('_BASE64URL')) value = Buffer.alloc(32, 'f').toString('base64url');
        else if (env.name.endsWith('_BASE64')) value = Buffer.alloc(32, 'f').toString('base64')
          .replace(['IDENTITY_PASSWORD_HMAC_KEY_BASE64', 'AUTH_RISK_HMAC_KEY_BASE64', 'REGISTRATION_QUERY_HMAC_KEY_BASE64', 'SMS_VERIFICATION_HMAC_KEY_BASE64'].includes(env.name) ? /=+$/ : /$^/, '');
        else if (env.name === 'HUMAN_DELEGATION_SIGNING_KEY_ID') value = 'public-fixture-v1';
        else if (env.name === 'HUMAN_DELEGATION_VERIFICATION_KEYS_JSON') value = JSON.stringify({ 'public-fixture-v1': publicKey });
        else if (env.name === 'PAYMENT_CALLBACK_CLIENT_SECRET') value = 'public-fixture-only-'.repeat(3);
        else if (/(?:URL|ENDPOINT|ISSUER)$/.test(env.name)) value = 'https://private.fixture.invalid';
        secrets[`${fixture.recipe.namespace}/${ref.name}/${ref.key}`] = value;
      }
    }
    writeFileSync(fixture.recipe.secretSnapshotFile, JSON.stringify(secrets));
    fixture.render = renderCurrentProduction(fixture.recipe, options);
    for (const workload of workloads(fixture.render, secrets)) {
      if (workload.environment.WORKLOAD_IDENTITY_RULES_JSON) for (const rule of JSON.parse(workload.environment.WORKLOAD_IDENTITY_RULES_JSON)) subjects[rule.service] = rule.subject;
    }
    // Helm uses actual namespace-qualified ServiceAccount subjects, not Keycloak UUIDs.
    for (const name of Object.keys(subjects)) if (!subjects[name].startsWith('system:serviceaccount:'))
      subjects[name] = `system:serviceaccount:${fixture.recipe.namespace}:public-fixture-unused-${name}`;
  }
  fixture.inventory = { version: 1, kind, images: Object.fromEntries(workloads(fixture.render, fixture.render.secrets).map(w => [w.id, w.image])),
    subjects, databases, backup, namespace: fixture.recipe.namespace, rendererSHA256 };
  fixture.rerender = () => { fixture.render = renderCurrentProduction(fixture.recipe, options); return fixture.render; };
  return fixture;
  } catch (error) {
    rmSync(directory, { recursive: true, force: true });
    throw error;
  }
}
