#!/usr/bin/env node
// Checks rendered configuration in memory. Never prints configuration or secret values.
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { isDeepStrictEqual } from 'node:util';
import { checkGovernedComposeConfiguration } from './account-platform-model-token-deployment-check.mjs';
import { parseReleaseJSON } from './account-platform-model-token-release-plan.mjs';
const require = createRequire(new URL('../account-platform/package.json', import.meta.url));
const YAML = require('yaml');
const readYAML = relative => YAML.parse(readFileSync(new URL(relative, import.meta.url), 'utf8'), { merge: true, uniqueKeys: true });
const template = readYAML('../account-platform/deploy/production/compose.business.yaml');
const permissionTemplate = readYAML('../account-platform/deploy/compose/compose.model-token.yaml');
export const businessPrivateEnvironment = Object.freeze({
  'account-service': ['OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64', 'HUMAN_DELEGATION_SIGNING_KEY_BASE64URL', 'AUDIT_EXPORT_MASTER_KEY_BASE64', 'REGISTRATION_QUERY_HMAC_KEY_BASE64', 'SMS_VERIFICATION_HMAC_KEY_BASE64', 'ALIYUN_SMS_ACCESS_KEY_ID', 'ALIYUN_SMS_ACCESS_KEY_SECRET', 'H3C_MALL_APP_KEY', 'H3C_MALL_APP_SECRET'],
  'identity-service': ['IDENTITY_PASSWORD_HMAC_KEY_BASE64', 'AUTH_RISK_HMAC_KEY_BASE64', 'AUTH_RISK_REDIS_URL'],
  'billing-service': ['H3C_MALL_APP_KEY', 'H3C_MALL_APP_SECRET', 'PAYMENT_CALLBACK_CLIENT_ID', 'PAYMENT_CALLBACK_CLIENT_SECRET', 'PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64'],
  'token-gateway': ['TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64', 'TOKEN_DEFAULT_PROVIDER_API_KEY', 'TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN', 'CLIENT_READINESS_ATTESTATION_PRIVATE_KEY_BASE64'],
  'token-portal': ['REDIS_URL', 'TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64'],
});
const workloadNames = ['account-service', 'identity-service', 'billing-service', 'token-gateway', 'token-command-executor', 'token-credential-portal', 'keycloak-mobile-authenticator'];
const uuid = value => typeof value === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value);
const digest = value => typeof value === 'string' && /^sha256:[0-9a-f]{64}$/.test(value);
const sameKeys = (a, b) => isDeepStrictEqual(Object.keys(a || {}).sort(), Object.keys(b || {}).sort());
const overlap = (a, b) => a === b || a.startsWith(`${b}/`) || b.startsWith(`${a}/`);
function check(ok, code) { if (!ok) throw new Error(`production_business_${code}`); }
function canonical(value) {
  check(typeof value === 'string' && value.startsWith('/') && value !== '/' && path.posix.normalize(value) === value
    && !value.endsWith('/') && !/[\\\x00-\x20\x7f]/.test(value), 'path_invalid');
  return value;
}
function https(value) {
  let parsed; try { parsed = new URL(value); } catch { check(false, 'https_invalid'); }
  check(parsed.protocol === 'https:' && !parsed.username && !parsed.password && !parsed.search && !parsed.hash, 'https_invalid');
  return parsed;
}

// The tracked template fixes literal settings and the permitted variable slots.
// Repeated site variables must resolve identically across every consumer.
function bindValue(expected, actual, bindings) {
  check(typeof actual === 'string' && actual.length > 0, 'value_missing');
  const slots = [...expected.matchAll(/\$\{([A-Z0-9_]+):\?[^}]+\}/g)];
  if (!slots.length) { check(actual === expected, 'literal_changed'); return; }
  const escape = raw => raw.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  let pattern = '^', offset = 0;
  for (const slot of slots) {
    pattern += escape(expected.slice(offset, slot.index)) + '([\\s\\S]+?)';
    offset = slot.index + slot[0].length;
  }
  const match = new RegExp(pattern + escape(expected.slice(offset)) + '$').exec(actual);
  check(match, 'variable_shape');
  slots.forEach((slot, index) => {
    const value = match[index + 1];
    check(value.trim() === value && !value.includes('${') && !/[\x00\r\n]/.test(value), 'variable_invalid');
    check(!(slot[1] in bindings) || bindings[slot[1]] === value, 'variable_inconsistent');
    bindings[slot[1]] = value;
  });
}

export function checkProductionBusiness(config, inventory) {
  check(config?.name === 'pinvou-production-business' && sameKeys(config.services, template.services)
    && sameKeys(config.networks, template.networks), 'inventory_mismatch');
  check(sameKeys(inventory?.images, template.services) && sameKeys(inventory?.subjects, Object.fromEntries(workloadNames.map(n => [n, true])))
    && Object.values(inventory.images).every(digest) && Object.values(inventory.subjects).every(uuid)
    && new Set(Object.values(inventory.subjects)).size === workloadNames.length, 'evidence_invalid');
  check(!config.volumes && !config.secrets && !config.configs && !config.include, 'extra_sources');
  checkGovernedComposeConfiguration(config);
  const bindings = {}, mounts = [], publicTargets = new Set(['/var/run/secrets/pinvou/trust', '/var/run/secrets/pinvou/nats-ca', '/var/run/secrets/pinvou/kms-ca']);
  const allowedKeys = new Set(['image', 'pull_policy', 'user', 'restart', 'read_only', 'cap_drop', 'security_opt', 'mem_limit', 'cpus', 'pids_limit', 'ulimits', 'tmpfs', 'logging', 'healthcheck', 'environment', 'volumes', 'networks', 'ports', 'profiles', 'entrypoint', 'command']);
  for (const [name, expected] of Object.entries(template.services)) {
    const service = config.services[name];
    check(Object.keys(service).every(k => allowedKeys.has(k)), 'service_option_unreviewed');
    check(digest(service.image) && service.image === inventory.images[name], 'image_not_verified');
    bindValue(expected.image, service.image, bindings);
    for (const k of ['pull_policy', 'user', 'restart', 'read_only', 'cap_drop', 'security_opt', 'pids_limit', 'tmpfs', 'logging']) {
      check(isDeepStrictEqual(service[k], expected[k]), 'runtime_boundary_changed');
    }
    // compose-go UlimitsConfig's JSON omitempty omits zero soft/hard fields.
    // Require an explicit core object; missing core is NOT a zero limit.
    check(sameKeys(service.ulimits, { core: true }) && service.ulimits.core && typeof service.ulimits.core === 'object'
      && !Array.isArray(service.ulimits.core) && Object.entries(service.ulimits.core).every(([key, value]) => ['soft', 'hard'].includes(key) && value === 0), 'core_limit_invalid');
    check([0.5, '0.5'].includes(service.cpus) && [268435456, '268435456'].includes(service.mem_limit), 'resource_limits');
    check(isDeepStrictEqual(service.profiles, expected.profiles), 'profile_changed');
    if (expected.profiles) {
      check(isDeepStrictEqual(service.entrypoint, ['/service']) && isDeepStrictEqual(service.command, [])
        && isDeepStrictEqual(service.healthcheck, { disable: true }), 'tool_auto_execution');
    } else {
      check(service.entrypoint == null && service.command == null, 'runtime_entrypoint_changed');
      check(isDeepStrictEqual(service.healthcheck, expected.healthcheck), 'healthcheck_changed');
    }
    check(sameKeys(service.networks, Object.fromEntries(expected.networks.map(n => [n, true])))
      && Object.values(service.networks).every(n => n == null || sameKeys(n, {})), 'network_membership');
    const secrets = businessPrivateEnvironment[name] || [];
    check(sameKeys(service.environment, { ...expected.environment, ...Object.fromEntries(secrets.map(k => [k, true])) }), 'environment_scope');
    for (const [k, value] of Object.entries(expected.environment)) bindValue(String(value), service.environment[k], bindings);
    for (const key of secrets) check(typeof service.environment[key] === 'string' && service.environment[key].trim().length > 0
      && !/[\x00\r\n]/.test(service.environment[key]), 'private_value_missing');
    const expectedMounts = expected.volumes || [];
    check(Array.isArray(service.volumes) && service.volumes.length === expectedMounts.length, 'mount_count');
    for (const item of expectedMounts) {
      const found = service.volumes.filter(v => v.target === item.target);
      check(found.length === 1, 'mount_target');
      const value = found[0];
      check(sameKeys(value, item) && value.type === 'bind' && value.read_only === true
        && isDeepStrictEqual(value.bind, { create_host_path: false }), 'mount_unsafe');
      canonical(value.source); bindValue(item.source, value.source, bindings);
      mounts.push({ name, source: value.source, target: value.target });
    }
    if (expected.ports) {
      check(service.ports?.length === 1, 'port_missing');
      const port = service.ports[0];
      check(Object.keys(port).every(k => ['target', 'published', 'host_ip', 'protocol', 'mode'].includes(k))
        && port.target === 8080 && port.host_ip === '127.0.0.1' && port.protocol === 'tcp'
        && (!port.mode || port.mode === 'ingress') && /^[1-9][0-9]{0,4}$/.test(port.published)
        && Number(port.published) <= 65535, 'port_invalid');
      bindValue(expected.ports[0].published, port.published, bindings);
    } else check(!service.ports?.length, 'unexpected_port');
    if (service.environment.WORKLOAD_IDENTITY_RULES_JSON) {
      const rules = parseReleaseJSON(service.environment.WORKLOAD_IDENTITY_RULES_JSON);
      const original = parseReleaseJSON(permissionTemplate.services[name].environment.WORKLOAD_IDENTITY_RULES_JSON);
      const expectedRules = original.map(rule => ({ ...rule, subject: inventory.subjects[rule.service] }));
      const sorted = list => [...list].map(rule => ({ ...rule, permissions: [...rule.permissions].sort() })).sort((a, b) => a.service.localeCompare(b.service));
      check(Array.isArray(rules) && isDeepStrictEqual(sorted(rules), sorted(expectedRules)), 'workload_rules_mismatch');
    }
  }
  for (let i = 0; i < mounts.length; i++) for (const other of mounts.slice(i + 1)) {
    const current = mounts[i];
    if (!overlap(current.source, other.source)) continue;
    check(current.source === other.source && current.target === other.target && publicTargets.has(current.target), 'secret_source_shared');
  }
  const names = new Set();
  for (const [key, value] of Object.entries(config.networks)) {
    check(Object.keys(value).every(k => ['name', 'external', 'ipam'].includes(k))
      && (!Object.hasOwn(value, 'ipam') || isDeepStrictEqual(value.ipam, {})) && value.external === true
      && typeof value.name === 'string' && /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(value.name)
      && !names.has(value.name), 'network_alias');
    names.add(value.name); bindValue(template.networks[key].name, value.name, bindings);
  }
  for (const key of ['TOKEN_GATEWAY_RELEASE_ROLE', 'TOKEN_CREDENTIAL_RELEASE_ROLE']) {
    check(/^[a-z][a-z0-9_]{0,62}$/.test(bindings[key])
      && !['token_gateway', 'token_migrator', 'token_policy_operator', 'token_credential_runtime', 'account_platform_admin'].includes(bindings[key]), 'release_role_invalid');
  }
  check(bindings.TOKEN_GATEWAY_RELEASE_ROLE !== bindings.TOKEN_CREDENTIAL_RELEASE_ROLE, 'release_role_shared');
  const onCall = bindings.MANUAL_REVIEW_ON_CALL_USER_IDS.split(',');
  check(onCall.length > 0 && onCall.every(uuid) && new Set(onCall).size === onCall.length, 'on_call_invalid');
  const sources = bindings.TRUSTED_TIME_SOURCES.split(',');
  check(sources.length >= 3 && new Set(sources).size === sources.length
    && sources.every(s => /^[a-z0-9][a-z0-9.-]+(?::123)?$/.test(s)), 'time_sources_invalid');
  https(bindings.WORKLOAD_IDENTITY_ISSUER); https(bindings.WORKLOAD_IDENTITY_JWKS_URL);
  https(bindings.PINVOU_CLIENT_UPGRADE_URL);
  const upstream = https(bindings.NEWAPI_HTTPS_URL);
  check(bindings.TOKEN_PROVIDER_ALLOWED_HOSTS.split(',').includes(upstream.hostname), 'provider_not_allowlisted');
  check(/^[0-9]+\.[0-9]+\.[0-9]+$/.test(bindings.BUSINESS_VERSION)
    && /^(true|false)$/.test(bindings.BILLING_NORMAL_REFUND_CREATION_ENABLED), 'business_setting_invalid');
  const releases = parseReleaseJSON(bindings.PINVOU_CLIENT_RELEASE_MANIFEST);
  check(Array.isArray(releases) && releases.length >= 3, 'client_manifest_invalid');
  for (const [name, key, user] of [['identity-service', 'AUTH_RISK_REDIS_URL', 'identity'], ['token-portal', 'REDIS_URL', 'portal']]) {
    let redis; try { redis = new URL(config.services[name].environment[key]); } catch { check(false, 'redis_invalid'); }
    check(redis.protocol === 'rediss:' && redis.hostname === 'redis' && redis.port === '6379'
      && redis.username === user && redis.password && redis.pathname === '/0' && !redis.search && !redis.hash, 'redis_invalid');
  }
  return { decision: 'CONFIG_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const args = process.argv.slice(2);
    check(args.length === 4 && args[0] === '--compose-render' && args[2] === '--inventory', 'usage');
    const config = parseReleaseJSON(readFileSync(args[1], 'utf8'));
    const inventory = parseReleaseJSON(readFileSync(args[3], 'utf8'));
    process.stdout.write(JSON.stringify(checkProductionBusiness(config, inventory)) + '\n');
  } catch (error) {
    // Even parser/IO errors can contain source snippets: allow fixed diagnostic codes only.
    const code = /^(production_business|model_token_deployment)_[a-z_]+$/.test(error?.message) ? error.message : 'production_business_input_rejected';
    process.stderr.write(code + '\n'); process.exitCode = 1;
  }
}
