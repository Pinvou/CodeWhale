#!/usr/bin/env node
// Structural verification of real renderer output. No cluster, process or activation mutation.
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import { isIP } from 'node:net';
import { parseReleaseJSON } from './account-platform-model-token-release-plan.mjs';
import { checkComposeKMS } from './account-platform-kms-compose-check.mjs';
import { checkHelmKMS } from './account-platform-kms-helm-check.mjs';
import { checkComposeNATS } from './account-platform-nats-compose-check.mjs';
const require = createRequire(new URL('../account-platform/package.json', import.meta.url));
const YAML = require('yaml');
const success = () => ({ decision: 'DEPLOYMENT_CONFIG_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' });
function check(condition, code) { if (!condition) throw new Error(`model_token_deployment_${code}`); }
const component = item => item?.spec?.template?.metadata?.labels?.['app.kubernetes.io/component'];
const env = container => Object.fromEntries((container?.env || []).map(item => [item.name, item.value ?? item.valueFrom]));
const secret = value => value?.secretKeyRef?.name;

function canonicalBindAddress(raw) {
  const address = raw.replace(/^\[|\]$/g, '');
  const version = isIP(address);
  check(version !== 0, 'port_invalid');
  if (version === 4) return address;
  // WHATWG serialization compresses/normalizes IPv6, including expanded zeros.
  const canonical = new URL(`http://[${address}]/`).hostname.slice(1, -1).toLowerCase();
  if (canonical.startsWith('::ffff:')) {
    const parts = canonical.slice(7).split(':');
    if (parts.length === 2) {
      const high = parseInt(parts[0], 16); const low = parseInt(parts[1], 16);
      return [high >>> 8, high & 255, low >>> 8, low & 255].join('.');
    }
  }
  return canonical;
}

export function matchesPodSelector(labels, selector = {}) {
  if (Object.entries(selector.matchLabels || {}).some(([key, value]) => labels[key] !== value)) return false;
  return (selector.matchExpressions || []).every(({ key, operator, values }) => {
    if (operator === 'In') return key in labels && values.includes(labels[key]);
    if (operator === 'NotIn') return !(key in labels) || !values.includes(labels[key]);
    if (operator === 'Exists') return key in labels;
    if (operator === 'DoesNotExist') return !(key in labels);
    return false;
  });
}

function boundedCIDR(cidr) {
  if (typeof cidr !== 'string') return false;
  const parts = cidr.split('/'); const version = isIP(parts[0]);
  return parts.length === 2 && version !== 0 && /^(?:[1-9]\d*)$/.test(parts[1])
    && Number(parts[1]) <= (version === 4 ? 32 : 128);
}

function selectedComponents(selector, labels) {
  if (!selector || typeof selector !== 'object') return [];
  // Same-namespace peers must remain bound to this chart instance, not all namespaces.
  for (const key of ['app.kubernetes.io/name', 'app.kubernetes.io/instance']) {
    if (selector.matchLabels?.[key] !== labels[key] || !labels[key]) return [];
  }
  if (selector.matchLabels?.['app.kubernetes.io/component']) return [selector.matchLabels['app.kubernetes.io/component']];
  if (selector.matchLabels?.['pinvou.io/newapi-data-plane'] === 'true') return ['new-api'];
  const expression = selector.matchExpressions?.find(item => item.key === 'app.kubernetes.io/component' && item.operator === 'In');
  return Array.isArray(expression?.values) ? expression.values : [];
}

// Validate the permits of EVERY selecting policy, not trusted-looking policy names.
// CIDR ownership/actual endpoint provenance remains an environment evidence check.
export function validateGovernedNetworkPolicy(policy, workload) {
  const name = component(workload); const labels = workload.spec.template.metadata.labels;
  const podTargets = name === 'token-credential-portal'
    ? { 'account-service': [8080], keycloak: [8080], 'new-api': [8443] }
    : name === 'token-command-executor' ? { 'account-service': [8080], 'token-gateway': [8080], nats: [4222] } : {};
  for (const direction of ['ingress', 'egress']) {
    const rules = policy.spec[direction] || [];
    check(Array.isArray(rules), 'network_rule_invalid');
    for (const rule of rules) {
      const peerKey = direction === 'egress' ? 'to' : 'from';
      check(Object.keys(rule).every(key => [peerKey, 'ports'].includes(key))
        && Array.isArray(rule[peerKey]) && rule[peerKey].length > 0
        && Array.isArray(rule.ports) && rule.ports.length > 0, 'network_rule_unbounded');
      for (const entry of rule.ports) {
        const port = entry.port; const protocol = entry.protocol || 'TCP';
        check(Object.keys(entry).every(key => ['port', 'protocol'].includes(key))
          && Number.isInteger(port) && port > 0 && port <= 65535 && ['TCP', 'UDP'].includes(protocol), 'network_port_invalid');
        for (const peer of rule[peerKey]) {
          check(Object.keys(peer).every(key => ['ipBlock', 'podSelector', 'namespaceSelector'].includes(key)) && Object.keys(peer).length > 0, 'network_peer_invalid');
          if (peer.ipBlock) {
            check(Object.keys(peer).length === 1 && Object.keys(peer.ipBlock).length === 1 && boundedCIDR(peer.ipBlock.cidr), 'network_cidr_unbounded');
            const permitted = direction === 'egress' && (protocol === 'UDP' && port === 123 && name !== 'token-governance-check'
              || protocol === 'TCP' && (name === 'token-governance-check' ? port === 5432
                : name === 'token-command-executor' ? [443, 4222].includes(port) : [443, 5432].includes(port)
                  || port === 8200 && env(workload.spec.template.spec.containers[0]).TOKEN_SECRET_PROVIDER === 'vault-transit'
                    && /^https:\/\/[^/?#]+:8200\/?$/.test(env(workload.spec.template.spec.containers[0]).TOKEN_SECRET_PROVIDER_URL)));
            check(permitted, 'network_destination_out_of_scope');
          } else if (peer.namespaceSelector) {
            const namespace = peer.namespaceSelector.matchLabels?.['kubernetes.io/metadata.name'];
            check(typeof namespace === 'string' && namespace.length > 0, 'network_namespace_unbounded');
            if (direction === 'egress') {
              check(namespace === 'kube-system' && port === 53, 'network_destination_out_of_scope');
            } else {
              check(protocol === 'TCP' && (port === 9090 || name === 'token-credential-portal' && port === 8080)
                && peer.podSelector && Object.keys(peer.podSelector.matchLabels || {}).length > 0, 'network_ingress_out_of_scope');
            }
          } else {
            const targets = selectedComponents(peer.podSelector, labels);
            check(direction === 'egress' && protocol === 'TCP' && targets.length > 0
              && targets.every(target => podTargets[target]?.includes(port)), 'network_destination_out_of_scope');
          }
        }
      }
    }
  }
}

function checkNetworkPolicies(items, workload) {
  const labels = workload.spec.template.metadata.labels;
  check(labels['pinvou.io/restricted-token-control'] === 'true', 'network_isolation_missing');
  const matching = items.filter(policy => policy?.kind === 'NetworkPolicy' && matchesPodSelector(labels, policy.spec.podSelector));
  check(matching.some(policy => policy.spec.policyTypes?.includes('Ingress') && policy.spec.policyTypes?.includes('Egress')
    && policy.metadata.name.endsWith(component(workload))), 'network_isolation_missing');
  for (const policy of matching) validateGovernedNetworkPolicy(policy, workload);
}

function verifyCredentialEndpoints(environment) {
  // WHATWG URL erases explicit :443; Go's credential validator rejects it.
  const explicitPort = raw => typeof raw !== 'string' || /^https:\/\/[^/?#]*:\d+(?:[/?#]|$)/.test(raw);
  const expected = String(environment.APP_ENV).startsWith('production')
    ? 'https://credentials.pinvou.com' : 'https://credentials-uat.pinvou.com';
  check(['staging', 'production', 'production-new'].includes(environment.APP_ENV), 'environment_invalid');
  check(environment.CREDENTIAL_PORTAL_PUBLIC_URL === expected, 'origin_invalid');
  check(environment.PORTAL_PUBLIC_URL === (String(environment.APP_ENV).startsWith('production') ? 'https://account.pinvou.com' : 'https://account-uat.pinvou.com'), 'portal_origin_invalid');
  let issuer;
  try { issuer = new URL(environment.OIDC_ISSUER); } catch { check(false, 'oidc_invalid'); }
  check(!explicitPort(environment.OIDC_ISSUER) && issuer.protocol === 'https:' && !issuer.port && !issuer.username && !issuer.password && !issuer.hash && !issuer.search && !issuer.pathname.toLowerCase().includes('userinfo'), 'oidc_invalid');
  for (const key of ['OIDC_JWKS_URL', 'CREDENTIAL_OIDC_AUTHORIZATION_URL', 'CREDENTIAL_OIDC_TOKEN_URL']) {
    let endpoint;
    try { endpoint = new URL(environment[key]); } catch { check(false, 'oidc_invalid'); }
    check(!explicitPort(environment[key]) && endpoint.protocol === 'https:' && endpoint.origin === issuer.origin && !endpoint.port && endpoint.pathname !== '/' && !endpoint.pathname.toLowerCase().includes('userinfo') && !endpoint.username && !endpoint.password && !endpoint.hash && !endpoint.search, 'oidc_invalid');
  }
  check(environment.CREDENTIAL_OIDC_CLIENT_ID && environment.CREDENTIAL_OIDC_AUDIENCE, 'oidc_invalid');
}

export function checkGovernedHelmResources(items) {
  checkHelmKMS(items);
  const deployments = items.filter(item => item?.kind === 'Deployment');
  const get = name => {
    const found = deployments.filter(item => component(item) === name);
    check(found.length === 1, 'workload_missing_or_duplicate');
    return found[0];
  };
  const credential = get('token-credential-portal'); const executor = get('token-command-executor');
  const gateway = get('token-gateway');
  const cenv = env(credential.spec.template.spec.containers[0]);
  const eenv = env(executor.spec.template.spec.containers[0]);
  const genv = env(gateway.spec.template.spec.containers[0]);
  verifyCredentialEndpoints(cenv);
  check(secret(cenv.DATABASE_URL) && secret(genv.DATABASE_URL) && secret(cenv.DATABASE_URL) !== secret(genv.DATABASE_URL), 'runtime_secret_not_separate');
  check(!('DATABASE_URL' in eenv) && !('TOKEN_MIGRATOR_DATABASE_URL' in eenv) && !('TOKEN_SECRET_PROVIDER_TOKEN' in eenv), 'executor_secret_scope');
  for (const item of [credential, executor]) {
    const spec = item.spec.template.spec; const container = spec.containers[0];
    check(spec.automountServiceAccountToken === false && container.readinessProbe?.httpGet?.path === '/health/ready'
      && container.livenessProbe?.httpGet?.path === '/health/live', 'readiness_invalid');
    check((spec.volumes || []).some(volume => volume.projected?.sources?.some(source => source.serviceAccountToken?.audience)), 'workload_identity_missing');
    checkNetworkPolicies(items, item);
  }
  const tokenRelay = get('token-event-relay');
  check(secret(env(tokenRelay.spec.template.spec.containers[0]).DATABASE_URL) === secret(genv.DATABASE_URL), 'relay_retired_identity');
  const nats = [get('account-service'), get('billing-service'), gateway, get('account-event-relay'), get('billing-event-relay'), tokenRelay, executor].map(item => env(item.spec.template.spec.containers[0]));
  check(/^tls:\/\//.test(eenv.NATS_URL) && nats.every(value => value.NATS_URL && value.NATS_URL === eenv.NATS_URL && value.NATS_CREDENTIALS_FILE), 'event_cluster_or_credentials_mismatch');
  const jobs = items.filter(item => item?.kind === 'Job' && component(item) === 'token-governance-check');
  check(jobs.length === 1, 'governance_job_missing');
  const job = jobs[0]; const control = job.spec.template.spec.containers[0]; const controlEnv = env(control);
  checkNetworkPolicies(items, job);
  check(job.spec.suspend === true && job.spec.backoffLimit === 0 && !job.metadata.annotations?.['helm.sh/hook']
    && job.spec.template.spec.containers.length === 1 && !(job.spec.template.spec.initContainers?.length)
    && !(job.spec.template.spec.ephemeralContainers?.length) && !control.lifecycle
    && !control.readinessProbe && !control.livenessProbe && !control.startupProbe
    && Array.isArray(control.command) && control.command.length === 1 && control.command[0] === '/service'
    && Array.isArray(control.args) && control.args.length === 0, 'governance_auto_execution');
  check(secret(controlEnv.TOKEN_MIGRATOR_DATABASE_URL) && secret(controlEnv.ACCOUNT_POLICY_DATABASE_URL) === secret(controlEnv.TOKEN_MIGRATOR_DATABASE_URL)
    && ![secret(cenv.DATABASE_URL), secret(genv.DATABASE_URL)].includes(secret(controlEnv.TOKEN_MIGRATOR_DATABASE_URL)), 'operations_secret_not_separate');
  for (const item of deployments) {
    const spec = item.spec.template.spec;
    for (const container of [...spec.containers, ...(spec.initContainers || []), ...(spec.ephemeralContainers || [])]) {
      const value = env(container);
      check(!('TOKEN_MIGRATOR_DATABASE_URL' in value) && !('ACCOUNT_POLICY_DATABASE_URL' in value), 'operations_secret_in_runtime');
    }
  }
  return success();
}

export function checkGovernedComposeConfiguration(config) {
  const services = config?.services || {};
  const credential = services['token-credential-portal']; const executor = services['token-command-executor'];
  const control = services['token-governance-check']; const gateway = services['token-gateway'];
  check(credential && executor && control && gateway, 'workload_missing_or_duplicate');
  checkComposeKMS(config);
  verifyCredentialEndpoints(credential.environment);
  check(!('DATABASE_URL' in executor.environment) && !('TOKEN_SECRET_PROVIDER_TOKEN' in executor.environment), 'executor_secret_scope');
  check(credential.environment.DATABASE_URL && credential.environment.DATABASE_URL !== gateway.environment.DATABASE_URL, 'runtime_secret_not_separate');
  check(services['token-event-relay']?.environment?.DATABASE_URL === gateway.environment.DATABASE_URL, 'relay_retired_identity');
  checkComposeNATS(config);
  const entrypointSafe = control.entrypoint == null || control.entrypoint === '/service'
    || Array.isArray(control.entrypoint) && control.entrypoint.length === 1 && control.entrypoint[0] === '/service';
  const healthcheckDisabled = !control.healthcheck || control.healthcheck.disable === true
    && Object.keys(control.healthcheck).every(key => key === 'disable');
  check(control.profiles?.includes('governance-operations') && control.restart === 'no' && entrypointSafe
    && Array.isArray(control.command) && control.command.length === 0
    && !control.post_start?.length && !control.pre_stop?.length && healthcheckDisabled, 'governance_auto_execution');
  const ports = [];
  for (const [name, service] of Object.entries(services)) {
    if (name !== 'token-governance-check') check(!('TOKEN_MIGRATOR_DATABASE_URL' in (service.environment || {}))
      && !('ACCOUNT_POLICY_DATABASE_URL' in (service.environment || {})), 'operations_secret_in_runtime');
    for (const port of service.ports || []) {
      check(typeof port === 'object', 'compose_not_normalized');
      const host = canonicalBindAddress(port.host_ip || '0.0.0.0');
      const published = String(port.published || ''); const match = published.match(/^(\d+)(?:-(\d+))?$/);
      check(isIP(host) !== 0 && match, 'port_invalid');
      const from = Number(match[1]); const to = Number(match[2] || match[1]);
      check(from > 0 && to >= from && to <= 65535, 'port_invalid');
      const binding = { host, from, to, protocol: port.protocol || 'tcp' };
      check(port.protocol === undefined || ['tcp', 'udp', 'sctp'].includes(port.protocol), 'port_protocol_invalid');
      const wildcard = value => ['0.0.0.0', '::'].includes(value);
      check(!ports.some(previous => previous.protocol === binding.protocol && previous.from <= to && from <= previous.to
        && (previous.host === host || wildcard(previous.host) || wildcard(host))), 'port_collision');
      ports.push(binding);
    }
  }
  check(credential.ports?.every(port => port.host_ip === '127.0.0.1'), 'credential_public_bind');
  for (const service of [credential, executor]) {
    check(service.read_only === true && service.healthcheck?.test?.includes('/healthcheck'), 'readiness_invalid');
    check(!(Object.keys(service.networks || {})).includes('backend'), 'network_union_widened');
    check((service.volumes || []).some(volume => volume.target === '/var/run/secrets/pinvou/workload'
      && volume.read_only === true && volume.bind?.create_host_path === false), 'workload_identity_missing');
  }
  return success();
}

async function main(args) {
  check(args.length === 2 && ['--helm-render', '--compose-render'].includes(args[0]), 'usage');
  let raw; try { raw = await readFile(args[1], 'utf8'); } catch { check(false, 'input_unreadable'); }
  let result;
  if (args[0] === '--compose-render') result = checkGovernedComposeConfiguration(parseReleaseJSON(raw));
  else {
    const documents = YAML.parseAllDocuments(raw, { uniqueKeys: true });
    check(documents.every(document => document.errors.length === 0), 'yaml_invalid');
    result = checkGovernedHelmResources(documents.map(document => document.toJSON()).filter(Boolean));
  }
  process.stdout.write(`${JSON.stringify(result)}\n`);
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch(error => {
    process.stderr.write(`${/^model_token_deployment_[a-z_]+$/.test(error?.message) ? error.message : 'model_token_deployment_invalid'}\n`);
    process.exitCode = 1;
  });
}
