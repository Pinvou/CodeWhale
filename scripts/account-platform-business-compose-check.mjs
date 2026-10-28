import { checkHelmBillingApprovalArtifacts, checkComposeBillingApprovalArtifacts } from './account-platform-billing-approval-artifacts-check.mjs';
import { checkHelmNATSCredentials } from './account-platform-nats-helm-check.mjs';
import { isMainModule } from './account-platform-cli-entry.mjs';
import { readFileSync } from 'node:fs';
import { approvedRender, gateCLI, mountedSource, physicalSourcePath, readSourceFile, requireGate as check, stableJSON } from './account-platform-production-render.mjs';
import { checkBusinessMigrations, checkRuntimeDatabases } from './account-platform-business-migrations-check.mjs';
import { parseReleaseJSON } from './account-platform-model-token-release-plan.mjs';
import { verifyRenderedKeycloak } from './account-platform-keycloak-helm-check.mjs';

const overlap = (a, b) => a === b || a.startsWith(`${b}/`) || b.startsWith(`${a}/`);
const same = (a, b) => stableJSON(a) === stableJSON(b);
const privateTarget = /\/(?:bootstrap|database|kms|nats|provisioner|handoff|workload)(?:\/|$)/;
function absolutePath(value) {
  check(typeof value === 'string' && /^\/(?!\/)/.test(value) && value !== '/'
    && !value.includes('\\') && !value.includes('//') && !value.split('/').some(v => ['.', '..'].includes(v)), 'mount_path');
  return value.replace(/\/$/, '');
}
function https(value) {
  let url; try { url = new URL(value); } catch { check(false, 'endpoint_tls'); }
  check(url.protocol === 'https:' && url.hostname && !url.username && !url.password && !url.hash, 'endpoint_tls');
}
const receiverTemplates = Object.fromEntries(['account', 'identity', 'billing', 'token'].map(domain => [
  `${domain === 'token' ? 'token-gateway' : `${domain}-service`}`,
  JSON.parse(readFileSync(new URL(`../account-platform/deploy/production/workload-${domain}-rules.json.template`, import.meta.url), 'utf8')),
]));
const privateKeys = {
  'account-service': ['DATABASE_URL', 'APPROVAL_WRITER_DATABASE_URL', 'WORKLOAD_IDENTITY_RULES_JSON',
    'OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64', 'HUMAN_DELEGATION_SIGNING_KEY_ID', 'HUMAN_DELEGATION_SIGNING_KEY_BASE64URL', 'AUDIT_EXPORT_MASTER_KEY_BASE64',
    'REGISTRATION_QUERY_HMAC_KEY_BASE64', 'REGISTRATION_BONUS_FINGERPRINT_HMAC_KEY_BASE64', 'SMS_VERIFICATION_HMAC_KEY_BASE64',
    'ALIYUN_SMS_ACCESS_KEY_ID', 'ALIYUN_SMS_ACCESS_KEY_SECRET', 'ALIYUN_SMS_SIGN_NAME', 'ALIYUN_SMS_REGISTER_TEMPLATE',
    'ALIYUN_SMS_LOGIN_TEMPLATE', 'ALIYUN_SMS_PROFILE_MOBILE_TEMPLATE', 'ALIYUN_SMS_PASSWORD_RESET_TEMPLATE', 'ALIYUN_SMS_ENDPOINT',
    'SMS_PROVIDER', 'SMS_HTTP_ENDPOINT', 'SMS_HTTP_BEARER_TOKEN', 'REGISTRATION_BONUS_FINGERPRINT_KEY_ID',
    'H3C_MALL_BASE_URL', 'H3C_MALL_APP_KEY', 'H3C_MALL_APP_SECRET',
    'WALLET_PROVISIONING_POLICY_VERSION', 'WALLET_PROVISIONING_SLA_MINUTES', 'MANUAL_REVIEW_ON_CALL_USER_IDS'],
  'identity-service': ['WORKLOAD_IDENTITY_RULES_JSON', 'IDENTITY_PASSWORD_HMAC_KEY_BASE64', 'AUTH_RISK_HMAC_KEY_BASE64', 'AUTH_RISK_REDIS_URL'],
  'billing-service': ['DATABASE_URL', 'WORKLOAD_IDENTITY_RULES_JSON', 'MANUAL_REVIEW_ON_CALL_USER_IDS',
    'HUMAN_DELEGATION_VERIFICATION_KEYS_JSON', 'BILLING_PAYMENT_PROVIDER', 'PAYMENT_PROVIDER_BASE_URL',
    'H3C_MALL_BASE_URL', 'H3C_MALL_APP_KEY', 'H3C_MALL_APP_SECRET', 'PAYMENT_MERCHANT_ID',
    'PAYMENT_CALLBACK_CLIENT_ID', 'PAYMENT_CALLBACK_CLIENT_SECRET', 'PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64',
    'PAYMENT_CALLBACK_TOKEN_KEY_ID', 'PAYMENT_CALLBACK_TOKEN_TTL_SECONDS'],
  'token-gateway': ['DATABASE_URL', 'WORKLOAD_IDENTITY_RULES_JSON', 'TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64',
    'TOKEN_ADMIN_PROXY_HMAC_KEY_BASE64', 'TOKEN_DEFAULT_PROVIDER_API_KEY', 'TOKEN_DEFAULT_PROVIDER_BASE_URL',
    'TOKEN_GATEWAY_ADAPTER', 'TOKEN_DEFAULT_PROVIDER_MODELS', 'TOKEN_DEFAULT_PROVIDER',
    'TOKEN_RECONCILIATION_NEWAPI_ADMIN_TOKEN', 'CLIENT_READINESS_ATTESTATION_PRIVATE_KEY_BASE64', 'MANUAL_REVIEW_ON_CALL_USER_IDS'],
  'token-credential-portal': ['DATABASE_URL', 'WORKLOAD_IDENTITY_RULES_JSON'],
  'token-command-executor': ['WORKLOAD_IDENTITY_RULES_JSON'],
  'token-portal': ['REDIS_URL', 'TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64', 'TOKEN_ADMIN_PROXY_HMAC_KEY_BASE64'],
};
const migrationKeys = ['DATABASE_URL', 'MIGRATION_ROLE', 'MIGRATION_BACKUP_EVIDENCE_ID', 'MIGRATION_BACKUP_COMPLETED_AT', 'MIGRATION_BACKUP_SHA256'];
const requiredPrivate = {
  'account-service': ['DATABASE_URL', 'WORKLOAD_IDENTITY_RULES_JSON', 'OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64',
    'HUMAN_DELEGATION_SIGNING_KEY_ID', 'HUMAN_DELEGATION_SIGNING_KEY_BASE64URL', 'REGISTRATION_QUERY_HMAC_KEY_BASE64',
    'REGISTRATION_BONUS_FINGERPRINT_HMAC_KEY_BASE64', 'SMS_VERIFICATION_HMAC_KEY_BASE64', 'SMS_PROVIDER', 'MANUAL_REVIEW_ON_CALL_USER_IDS'],
  'identity-service': ['WORKLOAD_IDENTITY_RULES_JSON', 'IDENTITY_PASSWORD_HMAC_KEY_BASE64', 'AUTH_RISK_HMAC_KEY_BASE64', 'AUTH_RISK_REDIS_URL'],
  'billing-service': ['DATABASE_URL', 'WORKLOAD_IDENTITY_RULES_JSON', 'MANUAL_REVIEW_ON_CALL_USER_IDS',
    'HUMAN_DELEGATION_VERIFICATION_KEYS_JSON', 'BILLING_PAYMENT_PROVIDER'], 'token-portal': ['REDIS_URL'],
  'account-event-relay': ['DATABASE_URL'], 'billing-event-relay': ['DATABASE_URL'],
};
function checkRequiredPrivate(workload, render) {
  const name = workload.kind === 'compose' ? workload.id : workload.service.name;
  const env = workload.environment;
  for (const key of requiredPrivate[name] || []) check(typeof env[key] === 'string' && env[key].trim().length > 0, 'private_value_missing');
  if (name === 'billing-service') {
    for (const key of ['OIDC_ISSUER', 'OIDC_JWKS_URL', 'OIDC_AUDIENCE'])
      check(typeof env[key] === 'string' && env[key].trim().length > 0, 'billing_oidc_missing');
    check(['h3c_mall', 'development_simulator'].includes(env.BILLING_PAYMENT_PROVIDER), 'payment_provider');
    if (env.BILLING_PAYMENT_PROVIDER === 'development_simulator') https(env.PAYMENT_PROVIDER_BASE_URL);
    else {
      https(env.H3C_MALL_BASE_URL);
      for (const key of ['H3C_MALL_APP_KEY', 'H3C_MALL_APP_SECRET', 'PAYMENT_MERCHANT_ID', 'PAYMENT_CALLBACK_CLIENT_ID',
        'PAYMENT_CALLBACK_CLIENT_SECRET', 'PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64', 'PAYMENT_CALLBACK_TOKEN_KEY_ID'])
        check(env[key]?.length > 0, 'payment_callback');
      check(env.PAYMENT_CALLBACK_CLIENT_SECRET.length >= 32, 'payment_callback');
      if (env.PAYMENT_CALLBACK_TOKEN_TTL_SECONDS) check(/^\+?\d+$/.test(env.PAYMENT_CALLBACK_TOKEN_TTL_SECONDS.trim())
        && Number(env.PAYMENT_CALLBACK_TOKEN_TTL_SECONDS) >= 60 && Number(env.PAYMENT_CALLBACK_TOKEN_TTL_SECONDS) <= 900, 'payment_callback');
    }
    const keys = parseReleaseJSON(env.HUMAN_DELEGATION_VERIFICATION_KEYS_JSON);
    check(keys && !Array.isArray(keys) && Object.keys(keys).length > 0 && Object.values(keys).every(key =>
      typeof key === 'string' && Buffer.from(key, 'base64url').length === 32
      && Buffer.from(key, 'base64url').toString('base64url') === key), 'delegation_keys');
  }
  if (name === 'account-service') {
    if (env.SMS_PROVIDER === 'http-generated') { https(env.SMS_HTTP_ENDPOINT); check(env.SMS_HTTP_BEARER_TOKEN?.length >= 8, 'sms_provider'); }
    else { check(env.SMS_PROVIDER === 'aliyun-managed', 'sms_provider');
      for (const key of ['ALIYUN_SMS_ACCESS_KEY_ID', 'ALIYUN_SMS_ACCESS_KEY_SECRET', 'ALIYUN_SMS_SIGN_NAME',
        'ALIYUN_SMS_REGISTER_TEMPLATE', 'ALIYUN_SMS_LOGIN_TEMPLATE']) check(env[key]?.length > 0, 'sms_provider'); }
  }
  if (['account-service', 'billing-service'].includes(name)) {
    const ids = env.MANUAL_REVIEW_ON_CALL_USER_IDS.split(',');
    // Preserve the original deployment inventory's canonical UUID format; the
    // runtime parser also accepts encodings outside this preflight contract.
    check(ids.length >= 1 && new Set(ids).size === ids.length
      && ids.every(v => /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(v)), 'oncall');
  }
  for (const key of ['OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64', 'REGISTRATION_BONUS_FINGERPRINT_HMAC_KEY_BASE64',
    'TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64', 'AUDIT_EXPORT_MASTER_KEY_BASE64', 'PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64']) {
    if (!env[key]) continue;
    const bytes = Buffer.from(env[key], 'base64');
    check((['TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64', 'PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64'].includes(key) ? bytes.length >= 32 : bytes.length === 32)
      && bytes.toString('base64') === env[key], 'private_key');
  }
  for (const key of ['IDENTITY_PASSWORD_HMAC_KEY_BASE64', 'AUTH_RISK_HMAC_KEY_BASE64',
    'REGISTRATION_QUERY_HMAC_KEY_BASE64', 'SMS_VERIFICATION_HMAC_KEY_BASE64']) if (env[key]) {
    const bytes = Buffer.from(env[key], 'base64');
    check(bytes.length >= 32 && bytes.toString('base64').replace(/=+$/, '') === env[key], 'private_key');
  }
  if (env.HUMAN_DELEGATION_SIGNING_KEY_BASE64URL) {
    const bytes = Buffer.from(env.HUMAN_DELEGATION_SIGNING_KEY_BASE64URL, 'base64url');
    check([32, 64].includes(bytes.length) && bytes.toString('base64url') === env.HUMAN_DELEGATION_SIGNING_KEY_BASE64URL, 'private_key');
  }
  if (name === 'token-gateway' && (render.kind === 'helm' || render.recipe.profile === 'models')) {
    for (const key of ['DATABASE_URL', 'TOKEN_DEFAULT_PROVIDER', 'TOKEN_DEFAULT_PROVIDER_BASE_URL', 'TOKEN_DEFAULT_PROVIDER_API_KEY'])
      check(env[key]?.length > 0, 'private_value_missing');
    https(env.TOKEN_DEFAULT_PROVIDER_BASE_URL);
    check(env.TOKEN_DEFAULT_PROVIDER_API_KEY.length >= 8 && ['NEW_API', 'OPENAI_COMPATIBLE'].includes(env.TOKEN_GATEWAY_ADAPTER || 'NEW_API'), 'provider');
  }
}
function checkEnvironment(workload, inventory, render) {
  const env = workload.environment;
  checkRequiredPrivate(workload, render);
  if (workload.kind === 'compose') {
    const source = render.template.services[workload.id], fixed = source.environment || {};
    check(!source.env_file?.length || privateKeys[workload.id] || /-migrate$/.test(workload.id)
      || /-event-relay$/.test(workload.id) || workload.id === 'approval-resources', 'private_process_scope');
    const extra = privateKeys[workload.id] || (/-migrate$/.test(workload.id) ? migrationKeys
      : /-event-relay$/.test(workload.id) ? ['DATABASE_URL', ...(workload.id === 'account-event-relay' ? ['APPROVAL_RELAY_DATABASE_URL'] : [])]
        : workload.id === 'approval-resources' ? ['DATABASE_URL'] : []);
    check(Object.keys(env).every(k => Object.hasOwn(fixed, k) || extra.includes(k)), 'environment_scope');
    for (const key of Object.keys(env).filter(k => !Object.hasOwn(fixed, k)))
      check(env[key].trim().length > 0 && !/[\u0000\r\n]/.test(env[key]), 'private_value_missing');
  }
  check(!Object.values(env).some(v => typeof v !== 'string' || /\$\{|__[A-Z][A-Z0-9_]+__/.test(v)), 'environment_unresolved');
  for (const [key, value] of Object.entries(env)) {
    if (value && /(?:OIDC_(?:ISSUER|JWKS_URL|AUTHORIZATION_URL|TOKEN_URL)|WORKLOAD_IDENTITY_(?:ISSUER|JWKS_URL)|OTEL_EXPORTER_OTLP_ENDPOINT|PORTAL_PUBLIC_URL|CREDENTIAL_PORTAL_PUBLIC_URL|TOKEN_SECRET_PROVIDER_URL)$/.test(key)) https(value);
    if (/(?:SKIP_TLS_VERIFY|INSECURE_SKIP_VERIFY|TLS_INSECURE)$/.test(key)) check(value === '' || value === 'false', 'tls_bypass');
    if (key === 'INTERNAL_SERVICE_TOKEN') check(value === '', 'static_internal_token');
    if (key === 'NATS_URL') check(value.startsWith('tls://') && !new URL(value).username && !new URL(value).password, 'nats_tls');
    if (['REDIS_URL', 'AUTH_RISK_REDIS_URL'].includes(key)) check(value.startsWith('rediss://'), 'redis_tls');
  }
  if (env.APP_ENV) check(['production', 'production-new'].includes(env.APP_ENV), 'environment_mode');
  if (env.TOKEN_SECRET_PROVIDER) {
    check(['remote', 'vault-transit'].includes(env.TOKEN_SECRET_PROVIDER) && !env.TOKEN_LOCAL_KEK_BASE64, 'kms_mode');
    if (env.TOKEN_SECRET_PROVIDER === 'vault-transit') {
      const endpoint = new URL(env.TOKEN_SECRET_PROVIDER_URL);
      check(['', '/'].includes(endpoint.pathname) && !endpoint.search && !env.TOKEN_SECRET_PROVIDER_TOKEN
        && env.TOKEN_SECRET_PROVIDER_TOKEN_FILE && env.TOKEN_SECRET_PROVIDER_CA_FILE, 'kms_mode');
    }
  }
  if (env.TOKEN_PROVIDER_ALLOWED_HOSTS) {
    const hosts = env.TOKEN_PROVIDER_ALLOWED_HOSTS.split(',');
    check(hosts.length > 0 && hosts.length === new Set(hosts).size && hosts.every(host =>
      /^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/.test(host) && host !== 'localhost' && !/^\d+(?:\.\d+){3}$/.test(host)), 'provider_hosts');
    if (env.TOKEN_DEFAULT_PROVIDER_BASE_URL) check(hosts.includes(new URL(env.TOKEN_DEFAULT_PROVIDER_BASE_URL).hostname), 'provider_hosts');
  }
  if (Object.hasOwn(env, 'TRUSTED_TIME_SOURCES')) {
    const sources = env.TRUSTED_TIME_SOURCES.split(',');
    check(sources.length >= 3 && new Set(sources).size === sources.length && sources.every(s => /^[a-zA-Z0-9.-]+(?::\d+)?$/.test(s)), 'time_sources');
  }
  if (env.WORKLOAD_IDENTITY_RULES_JSON) {
    let rules; try { rules = parseReleaseJSON(env.WORKLOAD_IDENTITY_RULES_JSON); } catch { check(false, 'workload_rules'); }
    check(Array.isArray(rules) && rules.length > 0, 'workload_rules');
    if (workload.kind === 'compose') {
      const template = receiverTemplates[workload.id];
      check(template, 'workload_receiver');
      const expected = template.map(rule => ({ ...rule, subject: inventory.subjects?.[rule.service] }));
      const sorted = values => values.map(rule => ({ ...rule, permissions: [...rule.permissions].sort() }))
        .sort((a, b) => a.service.localeCompare(b.service));
      check(same(sorted(rules), sorted(expected)), 'workload_rules_unapproved');
    }
    const services = new Set();
    for (const rule of rules) {
      const subject = inventory.subjects?.[rule.service];
      check(subject && subject === rule.subject && !services.has(rule.service), 'workload_subject'); services.add(rule.service);
      check(Array.isArray(rule.permissions) && rule.permissions.length > 0
        && new Set(rule.permissions).size === rule.permissions.length && rule.permissions.every(p => /^internal\.[a-z0-9.-]+$/.test(p)), 'workload_permission');
      if (workload.kind === 'compose') check(/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(subject), 'workload_subject');
      else check(subject.startsWith(`system:serviceaccount:${inventory.namespace}:`), 'workload_subject');
    }
  }
}
function checkCompose(all, render, inventory) {
  check(render.config.name === 'pinvou-personal' && !render.config.volumes && !render.config.secrets && !render.config.configs, 'composition');
  const networks = Object.values(render.config.networks || {});
  check(networks.length >= 2 && networks.every(n => n.external === true && /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(n.name || ''))
    && new Set(networks.map(n => n.name)).size === networks.length, 'network');
  const mounts = [];
  for (const workload of all) {
    const service = workload.service, edge = ['personal-edge', 'credential-edge'].includes(workload.id);
    const uid = workload.id === 'credential-edge' ? '101:101'
      : workload.id === 'keycloak-mobile-authenticator-workload-agent' ? '1000:0' : '65532:65532';
    check(service.user === uid
      && service.read_only === true && same(service.cap_drop, ['ALL'])
      && same(service.security_opt, ['no-new-privileges:true']) && !service.privileged && !service.cap_add
      && !service.network_mode && !service.pid && !service.ipc && !service.devices && !service.volumes_from
      && !service.sysctls && !service.build && service.pull_policy === 'never', 'runtime_boundary');
    if (!edge) {
      check(Number(service.mem_limit) > 0 && Number(service.mem_limit) <= 1024 * 1024 * 1024
        && Number(service.cpus) > 0 && Number(service.cpus) <= 2 && service.pids_limit > 0 && service.pids_limit <= 256
        && service.ulimits?.core && Object.entries(service.ulimits.core).every(([k, v]) => ['soft', 'hard'].includes(k) && v === 0), 'resource_limits');
      check(service.healthcheck && (service.healthcheck.disable === true || Array.isArray(service.healthcheck.test)), 'healthcheck');
    }
    check(service.tmpfs == null || (Array.isArray(service.tmpfs) && service.tmpfs.length === 1
      && /^\/tmp:rw,noexec,nosuid,nodev,size=16m,uid=(?:65532|101),gid=(?:65532|101),mode=0700$/.test(service.tmpfs[0])), 'tmpfs');
    if (service.healthcheck?.disable === true && !/-migrate$/.test(workload.id)) {
      check(Array.isArray(service.profiles) && service.profiles.length > 0 && service.restart === 'no'
        && (same(service.command, []) || (workload.id === 'approval-resources'
          && same(service.command, ['-action', 'status', '-expected-version', '1']))), 'operator_scope');
    }
    check(!service.ports || (edge && service.ports.length === 1 && service.ports[0].target === 8443
      && service.ports[0].protocol === 'tcp' && /^\d+$/.test(service.ports[0].published)
      && Number(service.ports[0].published) >= (workload.id === 'credential-edge' ? 443 : 1024)
      && Number(service.ports[0].published) <= 65535), 'published_port');
    check(Object.keys(service.networks || {}).every(name => Object.hasOwn(render.config.networks, name)), 'network');
    const targets = [];
    for (const volume of workload.mounts) {
      check(volume.type === 'bind' && volume.bind?.create_host_path === false && !volume.consistency, 'mount_type');
      const source = physicalSourcePath(absolutePath(volume.source)), target = absolutePath(volume.target);
      check(!targets.some(other => overlap(other, target)), 'mount_shadow'); targets.push(target);
      if (volume.read_only !== true) {
        const writer = workload.id.endsWith('-workload-agent') && target === '/run/projected';
        const receipt = workload.id === 'approval-resources'
          && ['/run/pinvou/approval-receipt', '/run/pinvou/token-consumer-receipt'].includes(target);
        check(writer || receipt, 'writable_mount');
      }
      mounts.push({ source, target, workload: workload.id, readOnly: volume.read_only === true });
    }
    checkEnvironment(workload, inventory, render);
    if (workload.id.endsWith('-workload-agent')) {
      const configMount = workload.mounts.find(v => v.target === '/run/config');
      check(configMount, 'agent_config');
      const config = parseReleaseJSON(readSourceFile(mountedSource(workload, '/run/config/workload.json')));
      const consumer = workload.id.replace(/-workload-agent$/, '');
      check(config.subject === inventory.subjects?.[consumer] && config.audience === 'pinvou-account-platform-internal'
        && config.client_id === consumer && config.client_secret_file === '/run/bootstrap/client-secret'
        && config.token_file === '/run/projected/token', 'agent_identity');
      https(config.issuer);
      const receivers = all.filter(w => w.environment.WORKLOAD_IDENTITY_ISSUER);
      check(receivers.length > 0 && receivers.every(w => w.environment.WORKLOAD_IDENTITY_ISSUER === config.issuer), 'agent_issuer');
      check(!Object.hasOwn(config, 'client_secret'), 'agent_bootstrap');
    }
  }
  for (let i = 0; i < mounts.length; i++) for (const b of mounts.slice(i + 1)) {
    const a = mounts[i];
    if (!overlap(a.source, b.source)) continue;
    check(a.source === b.source, 'mount_source_overlap');
    const publicCA = a.readOnly && b.readOnly && a.target === '/run/pinvou/trust' && b.target === a.target;
    const projection = [a, b].some(writer => writer.target === '/run/projected' && !writer.readOnly
      && writer.workload.endsWith('-workload-agent') && [a, b].some(reader => reader !== writer
        && reader.target === '/run/pinvou/workload' && reader.readOnly
        && `${reader.workload}-workload-agent` === writer.workload));
    const approvedPublic = a.target === b.target && ['/run/pinvou/approval-identities', '/run/pinvou/approval-manifest',
      '/run/pinvou/approval-receipt', '/run/pinvou/token-consumer-receipt'].includes(a.target);
    // Same-principal database mounts are checked against actual pgpass contents by
    // the migration validator; migration credentials can never reach runtime.
    const runtimeDatabase = a.readOnly && b.readOnly && a.target === '/run/pinvou/database'
      && b.target === a.target && ![a.workload, b.workload].some(n => /-migrate$/.test(n));
    const operatorIdentity = a.readOnly && b.readOnly && a.target === '/run/pinvou/nats' && b.target === a.target
      && [a.workload, b.workload].every(n => ['event-resources', 'approval-resources'].includes(n));
    check(publicCA || projection || approvedPublic || runtimeDatabase || operatorIdentity, 'shared_secret');
  }
  checkComposeBillingApprovalArtifacts(render.config);
}
function checkHelm(all, render, inventory) {
  checkHelmBillingApprovalArtifacts(render.config, render.secrets, render.recipe.namespace);
  checkHelmNATSCredentials(render.config);
  check(inventory.namespace === render.recipe.namespace, 'namespace');
  for (const resource of render.config) {
    if (resource.kind === 'Service') check(!['NodePort', 'LoadBalancer', 'ExternalName'].includes(resource.spec?.type)
      && !resource.spec?.externalIPs && !resource.spec?.ports?.some(p => p.nodePort), 'published_service');
    if (resource.kind === 'Ingress') {
      const hosts = (resource.spec?.rules || []).map(rule => rule.host);
      check(hosts.length > 0 && hosts.every(host => typeof host === 'string' && !host.includes('*')
        && (resource.spec?.tls || []).some(tls => tls.secretName && tls.hosts?.includes(host))), 'ingress_tls');
    }
  }
  const policies = render.config.filter(r => r.kind === 'NetworkPolicy');
  check(policies.some(r => Object.keys(r.spec?.podSelector?.matchLabels || {}).length > 0
    && !r.spec.podSelector.matchExpressions && same(r.spec?.policyTypes, ['Ingress', 'Egress'])
    && !r.spec?.ingress && !r.spec?.egress && all.every(w => Object.entries(r.spec.podSelector.matchLabels)
      .every(([key, value]) => w.resource.spec?.template?.metadata?.labels?.[key] === value))), 'network_default_deny');
  for (const policy of policies) for (const direction of ['ingress', 'egress']) for (const rule of policy.spec?.[direction] || []) {
    const peers = rule[direction === 'ingress' ? 'from' : 'to'];
    check(Array.isArray(peers) && peers.length > 0 && peers.every(peer => Object.keys(peer).length > 0
      && !same(peer.namespaceSelector, {}) && !['0.0.0.0/0', '::/0'].includes(peer.ipBlock?.cidr)), 'network_peer');
  }
  for (const workload of all) {
    const pod = workload.pod, container = workload.service, context = container.securityContext || {};
    check(!pod.hostNetwork && !pod.hostPID && !pod.hostIPC && pod.automountServiceAccountToken === false
      && (context.runAsNonRoot === true || pod.securityContext?.runAsNonRoot === true)
      && context.runAsUser !== 0 && pod.securityContext?.runAsUser !== 0
      && context.allowPrivilegeEscalation === false
      // Keycloak is an existing infrastructure dependency with its own rendered
      // lifecycle contract; the removed business-only checker did not require a
      // read-only Keycloak distribution. Do not invent that runtime change here.
      && (container.name === 'keycloak' || context.readOnlyRootFilesystem === true)
      && !context.privileged && same(context.capabilities?.drop, ['ALL']) && !context.capabilities?.add, 'runtime_boundary');
    check(!pod.volumes?.some(v => v.hostPath || v.nfs || v.csi || v.persistentVolumeClaim), 'host_volume');
    for (const mount of workload.mounts) {
      const found = (pod.volumes || []).filter(v => v.name === mount.name);
      check(found.length === 1 && !mount.subPathExpr, 'mount_reference');
      const volume = found[0];
      if (volume.secret) check(mount.readOnly === true && Array.isArray(volume.secret.items)
        && volume.secret.items.length > 0, 'whole_secret_mount');
    }
    checkEnvironment(workload, inventory, render);
  }
  if (render.config.some(r => r.kind === 'Deployment' && r.metadata?.name?.endsWith('-keycloak'))) verifyRenderedKeycloak(render.config);
}
export function checkProductionBusiness(render, inventory) {
  const all = approvedRender(render, inventory);
  const subjects = Object.values(inventory.subjects || {});
  check(subjects.length > 0 && subjects.length === new Set(subjects).size, 'subject_inventory');
  if (render.kind === 'compose') checkCompose(all, render, inventory);
  else checkHelm(all, render, inventory);
  checkRuntimeDatabases(all, inventory, render);
  checkBusinessMigrations(render, inventory);
  return { decision: 'BUSINESS_CONFIG_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED',
    migrations_executed: false, workloads_checked: all.length };
}
if (isMainModule(import.meta.url)) gateCLI(checkProductionBusiness);
