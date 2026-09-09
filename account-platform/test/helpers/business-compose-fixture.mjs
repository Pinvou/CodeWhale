// Public-only test data. Never use this fixture as production configuration.
import { readFileSync } from 'node:fs';
import YAML from 'yaml';
import { businessPrivateEnvironment } from '../../../scripts/account-platform-business-compose-check.mjs';

export function businessComposeFixture(root = '/fixture/business') {
  const source = readFileSync(new URL('../../deploy/production/compose.business.yaml', import.meta.url), 'utf8');
  const overlay = YAML.parse(readFileSync(new URL('../../deploy/compose/compose.model-token.yaml', import.meta.url), 'utf8'), { merge: true });
  const variables = {}, subjects = {};
  const subjectNames = ['account-service', 'identity-service', 'billing-service', 'token-gateway', 'token-command-executor', 'token-credential-portal', 'keycloak-mobile-authenticator'];
  subjectNames.forEach((name, i) => { subjects[name] = `00000000-0000-4000-8000-${String(i + 1).padStart(12, '0')}`; });
  let imageIndex = 1, port = 29100;
  for (const [, key] of source.matchAll(/\$\{([A-Z0-9_]+):\?[^}]+\}/g)) {
    if (key in variables) continue;
    variables[key] = key.endsWith('_IMAGE') ? `sha256:${String(imageIndex++).padStart(64, '0')}`
      : key.endsWith('_DIRECTORY') ? `${root}/mounts/${key.toLowerCase()}`
        : key.endsWith('_PRIVATE_ENV_FILE') ? `${root}/env/${key.toLowerCase()}.env`
          : key.endsWith('_BIND_PORT') ? String(port++)
            : key.endsWith('_NETWORK') ? `fixture-${key.toLowerCase()}` : 'fixture-public-value';
  }
  Object.assign(variables, {
    WORKLOAD_IDENTITY_ISSUER: 'https://keycloak:8443/realms/pinvou-workload',
    WORKLOAD_IDENTITY_JWKS_URL: 'https://keycloak:8443/realms/pinvou-workload/protocol/openid-connect/certs',
    TOKEN_SECRET_PROVIDER_URL: 'https://openbao:8200', TOKEN_SECRET_KEY_ID: 'fixture-transit',
    TOKEN_GATEWAY_RELEASE_ROLE: 'token_gateway_fixture', TOKEN_CREDENTIAL_RELEASE_ROLE: 'token_credential_fixture',
    ACCOUNT_POLICY_RELEASE_ROLE: 'account_policy_release_fixture',
    MANUAL_REVIEW_ON_CALL_USER_IDS: '00000000-0000-4000-8000-000000000100',
    TRUSTED_TIME_SOURCES: 'one.example.invalid,two.example.invalid,three.example.invalid',
    PINVOU_CLIENT_UPGRADE_URL: 'https://releases.example.invalid/current',
    PINVOU_CLIENT_RELEASE_MANIFEST: JSON.stringify([
      { version: '0.9.2', released_at: '2026-09-01T00:00:00Z' },
      { version: '0.9.1', released_at: '2026-08-01T00:00:00Z', deprecation_notice_at: '2026-09-01T00:00:00Z', sunset_at: '2026-12-01T00:00:00Z' },
      { version: '0.9.0', released_at: '2026-07-01T00:00:00Z', deprecation_notice_at: '2026-09-01T00:00:00Z', sunset_at: '2026-12-01T00:00:00Z' },
    ]),
    BUSINESS_VERSION: '0.9.2', BILLING_NORMAL_REFUND_CREATION_ENABLED: 'false',
    NEWAPI_HTTPS_URL: 'https://newapi.example.invalid', TOKEN_PROVIDER_ALLOWED_HOSTS: 'newapi.example.invalid',
    TOKEN_DEFAULT_PROVIDER_MODELS: 'fixture-model', TOKEN_RECONCILIATION_NEWAPI_ADMIN_USER_ID: '1',
    TOKEN_MINIMUM_CLIENT_VERSION: '0.9.0', TOKEN_MAXIMUM_CLIENT_VERSION: '0.9.2',
    TOKEN_MODEL_CATALOG_DEFAULT_CONTEXT_TOKENS: '128000', TOKEN_MODEL_CATALOG_DEFAULT_MAX_OUTPUT_TOKENS: '4096',
    HUMAN_DELEGATION_VERIFICATION_KEYS_JSON: '{"fixture-key":"fixture-public-key"}',
    SYSTEM_PROBES_JSON: '[{"key":"portal","kind":"local"}]',
  });
  for (const [service, key] of Object.entries({ 'account-service': 'ACCOUNT_WORKLOAD_RULES_JSON', 'identity-service': 'IDENTITY_WORKLOAD_RULES_JSON', 'billing-service': 'BILLING_WORKLOAD_RULES_JSON', 'token-gateway': 'TOKEN_WORKLOAD_RULES_JSON' })) {
    variables[key] = JSON.stringify(JSON.parse(overlay.services[service].environment.WORKLOAD_IDENTITY_RULES_JSON).map(rule => ({ ...rule, subject: subjects[rule.service] })));
  }
  // Substitute parsed scalar values, not raw YAML, so JSON strings retain their quoting.
  const replace = value => typeof value === 'string' ? value.replace(/\$\{([A-Z0-9_]+):\?[^}]+\}/g, (_, key) => variables[key])
    : Array.isArray(value) ? value.map(replace) : value && typeof value === 'object' ? Object.fromEntries(Object.entries(value).map(([k, v]) => [k, replace(v)])) : value;
  const input = replace(YAML.parse(source, { merge: true }));
  const config = structuredClone(input), envFiles = {}, images = {};
  for (const network of Object.values(config.networks)) network.ipam = {};
  for (const [name, service] of Object.entries(config.services)) {
    images[name] = service.image;
    // Actual Compose 5.4 JSON encodes bytes as a decimal string and omits zero ulimit fields.
    service.mem_limit = '268435456';
    service.ulimits = { core: {} };
    service.networks = Object.fromEntries(service.networks.map(n => [n, {}]));
    if (service.ports) service.ports = service.ports.map(p => ({ ...p, mode: 'ingress' }));
    const privateEnv = Object.fromEntries((businessPrivateEnvironment[name] || []).map(key => [key, `PUBLIC_FIXTURE_ONLY_${key}`]));
    if ('H3C_MALL_APP_SECRET' in privateEnv) privateEnv.H3C_MALL_APP_SECRET = 'PUBLIC_FIXTURE_ONLY_$DO_NOT_INTERPOLATE_"quotes"';
    if (name === 'identity-service') privateEnv.AUTH_RISK_REDIS_URL = 'rediss://identity:PUBLIC_FIXTURE_ONLY@redis:6379/0';
    if (name === 'token-portal') privateEnv.REDIS_URL = 'rediss://portal:PUBLIC_FIXTURE_ONLY@redis:6379/0';
    if (service.env_file) {
      envFiles[service.env_file[0].path] = Object.entries(privateEnv).map(([k, v]) => `${k}=${v}`).join('\n') + '\n';
      delete service.env_file;
    }
    Object.assign(service.environment, privateEnv);
  }
  for (const value of [input, config]) for (const key of Object.keys(value)) if (key.startsWith('x-')) delete value[key];
  return { input, config, inventory: { images, subjects }, envFiles, variables };
}
